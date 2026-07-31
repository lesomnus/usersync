package cmd

import (
	"context"
	"fmt"

	"github.com/lesomnus/usersync/internal/executor"
	"github.com/lesomnus/usersync/internal/fsops"
	"github.com/lesomnus/usersync/internal/reconcile"
	"github.com/lesomnus/usersync/internal/report"
	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/secret"
	"github.com/lesomnus/xli"
)

func NewCmdApply() *xli.Command {
	return &xli.Command{
		Name:  "apply",
		Brief: "converge the system to the roster (idempotent, no destructive deletes)",
		Synop: "Collects actual state, diffs it against the roster, and executes the actions. Never deletes accounts (disable only); use purge for removal.",

		Flags: commonFlags(),

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
			if err := applyCommonFlags(cmd, c); err != nil {
				return err
			}
			cls := c.Classifier()

			ro, skipped, err := loadRoster(cmd, c, cls)
			if err != nil {
				return err
			}
			warnSkipped(cmd, skipped)

			runner := run.Exec{}
			p, s, err := backends(c, runner)
			if err != nil {
				return err
			}
			actual, err := executor.Collect(ctx, p, s, cls, executor.CollectOpts{
				HomeBase:   c.Paths.Home,
				GroupsBase: c.Paths.Groups,
				FS:         fsops.OS{},
			})
			if err != nil {
				return err
			}

			actions := reconcile.Reconcile(ro, actual, cls)
			res := report.Result{DryRun: false, Actions: actions, Skipped: skipped}

			// A seed is only needed when some action creates an SMB account.
			var der *secret.Deriver
			if needsSeed(actions) {
				der, err = deriver(c)
				if err != nil {
					return err
				}
			}

			d := executor.Deps{
				Provider:   p,
				Samba:      s,
				Deriver:    der,
				FS:         fsops.OS{},
				HomeBase:   c.Paths.Home,
				GroupsBase: c.Paths.Groups,
			}
			results, applyErr := d.Apply(ctx, actions)
			res.Errors = results // report reflects what actually happened

			if jsonRequested(cmd) {
				_ = report.JSON(cmd, res)
			} else {
				_ = report.Text(cmd, res)
			}

			if applyErr != nil {
				return fmt.Errorf("apply: %w", applyErr)
			}
			if report.ExitCode(res) != 0 {
				return fmt.Errorf("apply completed but roster contains refusals requiring manual intervention")
			}
			return nil
		}),
	}
}

// needsSeed reports whether any action will DERIVE a password (i.e. register a
// new SMB account). A create whose SMB account already exists (HasSmb) reuses
// the existing password and needs no seed.
func needsSeed(actions []reconcile.Action) bool {
	for _, a := range actions {
		switch a.Kind {
		case reconcile.AddSmb:
			return true
		case reconcile.CreateUser, reconcile.CreateUserDisabled:
			if !a.HasSmb {
				return true
			}
		}
	}
	return false
}
