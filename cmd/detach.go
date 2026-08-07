package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/lesomnus/usersync/internal/idrange"
	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
)

// detachVerdict is what NSS reports for a name once its local account is gone.
// The home directory is owned by a NUMBER, not by a name, so the only question
// that matters after a detach is which identity that number now belongs to.
type detachVerdict int

const (
	// handedOver: another NSS source (winbind/AD) answers for the name with the
	// same uid. Ownership of every file is unchanged — this is the success case.
	handedOver detachVerdict = iota
	// unresolved: nothing answers for the name. The files are intact but owned by
	// a number that maps to nobody, so they list as a bare uid. Recoverable: the
	// roster still declares the user, so `usersync apply` recreates the account.
	unresolved
	// hijacked: something answers for the name with a DIFFERENT uid. The new
	// holder of the name does not own the files, and whoever else ends up with
	// the old number does. This is the uid-reuse hazard, realised.
	hijacked
)

// classifyDetach decides the verdict from the uid before the detach and what
// NSS resolves afterwards.
func classifyDetach(oldUID, newUID uint32, resolved bool) detachVerdict {
	switch {
	case !resolved:
		return unresolved
	case newUID != oldUID:
		return hijacked
	default:
		return handedOver
	}
}

// rosterDeclares reports whether the roster has an entry for the name, at ANY
// status. Presence is what reserves the uid, so an entry — even a `reserved`
// tombstone — is enough.
func rosterDeclares(ro *roster.Roster, user string) bool {
	for _, u := range ro.Users {
		if u.Name == user {
			return true
		}
	}
	return false
}

func NewCmdDetach() *xli.Command {
	return &xli.Command{
		Name:  "detach",
		Brief: "release a user's LOCAL account, keeping the home (hand the name to AD)",
		Synop: "Deletes the local unix account, its UPG and its SMB (tdbsam) entry, leaving the home directory and every file in it untouched, so a directory service (winbind/AD) can take the name over one user at a time. " +
			"The roster must still declare the user: that entry is what keeps the uid reserved, and it makes the operation reversible — `usersync apply` recreates the local account. " +
			"Afterwards the name is looked up again and the run FAILS if it now resolves to a different uid.",

		Args: arg.Args{
			&arg.String{Name: "user", Brief: "the managed user whose local account to release"},
		},
		Flags: flg.Flags{
			&flg.String{Name: "roster", Brief: "roster that must still declare the user", Value: z.Ptr("roster.yaml")},
			&flg.Switch{Name: "yes", Alias: 'y', Brief: "skip the confirmation prompt"},
			&flg.Switch{Name: "keep-smb", Brief: "keep the tdbsam entry (default: remove it, so the local password cannot still authenticate)"},
		},

		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			if err := requireRoot(); err != nil {
				return err
			}
			unlock, err := lockRun()
			if err != nil {
				return err
			}
			defer unlock()

			c := use_config.Must(ctx)
			cls := c.Classifier()
			user := arg.MustGet[string](cmd, "user")
			if !roster.ValidName(user) {
				return fmt.Errorf("invalid user name %q (must match %s)", user, roster.NamePattern)
			}

			uid, home, err := lookupUser(ctx, user)
			if err != nil {
				return err
			}
			if cls.UID(uid) != idrange.Managed {
				return fmt.Errorf("refusing to detach %q: uid %d is not in the managed range", user, uid)
			}

			// The roster entry is the ledger that reserves this uid. Detaching a
			// name the roster has forgotten would free the number while files in the
			// home still carry it — precisely the reuse hazard the tombstones exist
			// to prevent — and would also remove the way back.
			ro, skipped, err := loadRoster(cmd, c, cls)
			if err != nil {
				return err
			}
			warnSkipped(cmd, skipped)
			if !rosterDeclares(ro, user) {
				return fmt.Errorf("refusing to detach %q: the roster does not declare it, so detaching would free uid %d while %s still holds files owned by that number; add the entry back (any status reserves the uid) and retry",
					user, uid, home)
			}

			keepSmb, _ := flg.Get[bool](cmd, "keep-smb")
			if yes, _ := flg.Get[bool](cmd, "yes"); !yes {
				ok, err := confirm(cmd, fmt.Sprintf("Release the LOCAL account for %q (uid %d)? The home %s and its files are kept. [y/N] ", user, uid, home))
				if err != nil {
					return err
				}
				if !ok {
					cmd.Println("aborted")
					return nil
				}
			}

			runner := run.Exec{}
			p, s, err := backends(c, runner)
			if err != nil {
				return err
			}

			// Drop the local SMB credential first. Left behind, it is a password that
			// still authenticates if smbd ever falls back to the local passdb — a
			// stale way in for an identity that is supposed to live in AD now.
			if !keepSmb {
				if err := s.Delete(ctx, user); err != nil {
					fmt.Fprintf(errW(cmd), "warning: could not remove the SMB account (none registered?): %v\n", err)
				}
			}
			if err := p.RemoveAccount(ctx, user); err != nil {
				return err
			}
			cmd.Printf("released the local account for %q (uid %d)\n", user, uid)

			// The home must survive: that is the entire difference from `purge`.
			if _, err := os.Stat(home); err != nil {
				return fmt.Errorf("BUG: the home %s did not survive the detach: %w", home, err)
			}

			newUID, _, lookupErr := lookupUser(ctx, user)
			switch classifyDetach(uid, newUID, lookupErr == nil) {
			case handedOver:
				cmd.Printf("%s now resolves to uid %d from another source (winbind/AD) — ownership of %s is unchanged\n", user, newUID, home)
				return nil

			case unresolved:
				fmt.Fprintf(errW(cmd),
					"warning: nothing resolves %q any more, so %s is now owned by the bare number %d.\n"+
						"  This is expected only if the domain join is not in place yet.\n"+
						"  To undo: `usersync apply` recreates the local account (the roster still declares it).\n",
					user, home, uid)
				return nil

			case hijacked:
				return fmt.Errorf("DANGER: %q now resolves to uid %d, but %s is owned by uid %d — "+
					"the name and the files have come apart, and whoever holds uid %d can read them. "+
					"Run `usersync apply` to restore the local account, then fix the directory's uidNumber for %q to %d before retrying",
					user, newUID, home, uid, uid, user, uid)
			}
			return nil
		}),
	}
}
