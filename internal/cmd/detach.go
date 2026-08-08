package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/lesomnus/usersync/internal/idrange"
	"github.com/lesomnus/usersync/internal/provider"
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

// rosterEntry returns the roster's entry for the name, or nil.
//
// The caller needs the whole entry, not just presence: the DECLARED uid (an
// entry reserves the number it names, which can disagree with what the account
// currently carries) and the status (which decides whether `usersync apply` can
// undo the detach).
func rosterEntry(ro *roster.Roster, user string) *roster.User {
	for i := range ro.Users {
		if ro.Users[i].Name == user {
			return &ro.Users[i]
		}
	}
	return nil
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
			&flg.Switch{Name: "keep-upg", Brief: "keep the user's private group in /etc/group so the gid on their files still resolves to a name"},
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
			entry := rosterEntry(ro, user)
			if entry == nil {
				return fmt.Errorf("refusing to detach %q: the roster does not declare it, so detaching would free uid %d while %s still holds files owned by that number; add the entry back (any status reserves the uid) and retry",
					user, uid, home)
			}
			declared := entry.UID
			// A reserved entry is a tombstone: it says no account should exist. So
			// `apply` will not recreate one, and the undo this command advertises
			// does not exist for it. A local account standing against a tombstone is
			// its own problem — `usersync audit` reports it as tombstone-live — and
			// the tool for removing it deliberately is `purge`, which archives first.
			if entry.Status == roster.Reserved {
				return fmt.Errorf("refusing to detach %q: its roster entry is `status: reserved`, so `usersync apply` would not recreate the account and this step could not be undone. A reserved entry means no account should exist at all — use `usersync purge` (which archives the home first) if that is what you want",
					user)
			}
			// The entry reserves the number it DECLARES. If the account has drifted
			// onto a different one, that number is reserved by nobody: releasing the
			// account frees it while the home still carries it, and the documented
			// undo makes it worse rather than better — `usersync apply` recreates the
			// account on the declared uid, so the files stay behind on the old one.
			if declared != uid {
				return fmt.Errorf("refusing to detach %q: the roster reserves uid %d but the account holds uid %d, so %s is owned by a number no entry reserves. `usersync apply` would recreate %q at %d and leave those files behind — run `usersync audit` and reconcile the drift first",
					user, declared, uid, home, user, declared)
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

			// Whether the home is there BEFORE anything is removed. Checking only
			// afterwards cannot tell "we destroyed it" from "it was never there",
			// and a home that is simply absent — never created, or its dataset not
			// mounted — must not be reported as data loss.
			homeExisted := false
			if _, err := os.Stat(home); err == nil {
				homeExisted = true
			}

			// Drop the local SMB credential first. Left behind, it is a password that
			// still authenticates if smbd ever falls back to the local passdb — a
			// stale way in for an identity that is supposed to live in AD now.
			//
			// smbpasswd -x fails both when there is no account and when something is
			// actually wrong (a locked tdb, a misconfigured passdb). Those must not
			// read the same: look first, and treat a failure to delete an account we
			// know exists as fatal, because continuing would leave exactly the stale
			// credential this step is here to remove.
			if !keepSmb {
				accts, err := s.Accounts(ctx)
				if err != nil {
					return fmt.Errorf("could not read the SMB accounts, so the local credential for %q cannot be confirmed removed: %w", user, err)
				}
				if _, ok := accts[user]; !ok {
					fmt.Fprintf(errW(cmd), "note: %q has no SMB account registered; nothing to remove\n", user)
				} else if err := s.Delete(ctx, user); err != nil {
					return fmt.Errorf("failed to remove the SMB account for %q, which would stay usable against the local passdb: %w", user, err)
				}
			}
			keepUPG, _ := flg.Get[bool](cmd, "keep-upg")
			if err := p.RemoveAccount(ctx, user, provider.RemoveOpts{KeepUPG: keepUPG}); err != nil {
				return err
			}
			cmd.Printf("released the local account for %q (uid %d)\n", user, uid)

			// The home must survive: that is the entire difference from `purge`.
			if homeExisted {
				if _, err := os.Stat(home); err != nil {
					return fmt.Errorf("BUG: the home %s existed before the detach and does not now: %w", home, err)
				}
			} else {
				fmt.Fprintf(errW(cmd), "note: %s did not exist before the detach either; nothing was lost\n", home)
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
