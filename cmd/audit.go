package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lesomnus/usersync/internal/audit"
	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/xli"
)

func NewCmdAudit() *xli.Command {
	return &xli.Command{
		Name:  "audit",
		Brief: "verify that what the system resolves matches the roster (read-only)",
		Synop: "Compares every roster entry against what the system actually resolves — including names served by a directory through NSS — and reports any disagreement. Changes nothing, needs no root, and exits non-zero when it finds something. " +
			"This is what `mode: audit` leaves usersync doing once a directory service owns the accounts: the roster is still the ledger of which number belongs to whom, and nothing else checks that the directory agrees with it.",

		Flags: commonFlags(),

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
			actual, err := collectActual(ctx, c, run.Exec{}, cls, true, errW(cmd))
			if err != nil {
				return err
			}

			rep := audit.Run(ro, actual, cls)

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
			}

			if !rep.OK() {
				return fmt.Errorf("audit found %d disagreement(s) between the roster and the system", len(rep.Findings))
			}
			return nil
		}),
	}
}
