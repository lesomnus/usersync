package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lesomnus/usersync/internal/audit"
	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
)

func NewCmdAudit() *xli.Command {
	return &xli.Command{
		Name:  "audit",
		Brief: "verify that what the system resolves matches the roster (read-only)",
		Synop: "Compares every roster entry against what the system actually resolves — including names served by a directory through NSS — and reports any disagreement. Changes nothing, needs no root, and exits non-zero when it finds something. " +
			"This is what `mode: audit` leaves usersync doing once a directory service owns the accounts: the roster is still the ledger of which number belongs to whom, and nothing else checks that the directory agrees with it.",

		// audit reports, it never provisions, so no --seed-file.
		Flags: append(rosterFlags(), &flg.Switch{Name: "json", Brief: "machine-readable JSON report"}),

		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			if err := applyCommonFlags(cmd, c); err != nil {
				return err
			}
			cls := c.Classifier()

			ro, skipped, err := loadRoster(cmd, c, cls)
			if err != nil {
				return err
			}
			warnSkipped(cmd, skipped)

			// SMB state is irrelevant here — the question is purely which number a
			// name resolves to — so an unavailable pdbedit is only a warning and the
			// command stays runnable without root.
			runner := run.Exec{}
			actual, err := collectActual(ctx, c, runner, cls, true, errW(cmd))
			if err != nil {
				return err
			}

			// Resolve every declared name with a KEYED lookup rather than reading it
			// out of the enumeration above. winbind does not enumerate domain
			// accounts by default, so after a handover the enumeration would show
			// none of them and every roster entry would be reported missing — an
			// alarm on every user, every run, in the exact state audit exists for.
			p, _, err := backends(c, runner)
			if err != nil {
				return err
			}
			res := audit.Resolved{Users: map[string]uint32{}, Groups: map[string]uint32{}}
			for _, u := range ro.Users {
				id, found, err := p.LookupUser(ctx, u.Name)
				if err != nil {
					return fmt.Errorf("looking up user %q: %w", u.Name, err)
				}
				if found {
					res.Users[u.Name] = id
				}
			}
			for _, g := range ro.Groups {
				id, found, err := p.LookupGroup(ctx, g.Name)
				if err != nil {
					return fmt.Errorf("looking up group %q: %w", g.Name, err)
				}
				if found {
					res.Groups[g.Name] = id
				}
			}

			rep := audit.Run(ro, res, actual, cls)

			if jsonRequested(cmd) {
				enc := json.NewEncoder(cmd)
				enc.SetIndent("", "  ")
				if err := enc.Encode(rep); err != nil {
					return err
				}
			} else {
				cmd.Println("AUDIT (roster vs. what the system resolves)")
				for _, f := range rep.Findings {
					cmd.Println("  ✗ " + f.String())
				}
				cmd.Printf("Summary: %d users, %d groups checked — %d findings\n",
					rep.UsersChecked, rep.GroupsChecked, len(rep.Findings))
				// The undeclared/collision sweep can only see names that enumerate,
				// and a directory does not enumerate by default. Print the count so a
				// clean result is not read as proof that nothing else is out there.
				cmd.Printf("         (undeclared/collision checks saw %d users and %d groups in the enumeration)\n",
					rep.EnumeratedUsers, rep.EnumeratedGroups)
			}

			if !rep.OK() {
				return fmt.Errorf("audit found %d disagreement(s) between the roster and the system", len(rep.Findings))
			}
			return nil
		}),
	}
}
