package cmd

import (
	"context"
	"fmt"

	"github.com/lesomnus/usersync/internal/cmd/config"
	"github.com/lesomnus/usersync/internal/reconcile"
	"github.com/lesomnus/usersync/internal/report"
	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
)

func NewCmdPlan() *xli.Command {
	return &xli.Command{
		Name:  "plan",
		Brief: "preview the actions needed to converge the system to the roster (no changes)",
		Synop: "Collects actual state, diffs it against the roster, and prints the planned actions. Makes no changes.",

		Flags: append(commonFlags(),
			&flg.Switch{Name: "commands", Brief: "also print the exact backend commands each action would run"},
		),

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

			actual, err := collectActual(ctx, c, run.Exec{}, cls, true, errW(cmd))
			if err != nil {
				return err
			}

			actions := reconcile.Reconcile(ro, actual, cls)
			res := report.Result{DryRun: true, Actions: actions, Skipped: skipped}

			if jsonRequested(cmd) {
				if err := report.JSON(cmd, res); err != nil {
					return err
				}
			} else {
				if err := report.Text(cmd, res); err != nil {
					return err
				}
			}

			if v, _ := flg.Get[bool](cmd, "commands"); v {
				if err := printCommands(ctx, cmd, c, actions); err != nil {
					return err
				}
			}

			if code := report.ExitCode(res); code != 0 {
				return fmt.Errorf("plan contains refusals requiring manual intervention")
			}
			return nil
		}),
	}
}

// printCommands re-runs the same apply dispatch through a print-only runner and
// filesystem, so the preview is exactly the code path apply would take (minus
// real execution and passwords).
func printCommands(ctx context.Context, cmd *xli.Command, c *config.Config, actions []reconcile.Action) error {
	cmd.Println("\nCOMMANDS (dry-run; not executed):")
	d, err := dryDeps(c, cmd)
	if err != nil {
		return err
	}
	// Errors here are from the print backends (which never fail); ignore.
	_, _ = d.Apply(ctx, actions)
	return nil
}
