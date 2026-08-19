package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/lesomnus/usersync/internal/cmd/config"
	"github.com/lesomnus/usersync/internal/executor"
	"github.com/lesomnus/usersync/internal/fsops"
	"github.com/lesomnus/usersync/internal/quota"
	"github.com/lesomnus/usersync/internal/reconcile"
	"github.com/lesomnus/usersync/internal/report"
	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/secret"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
)

// applyFlags are the flags shared by the two commands that reconcile — `apply`
// (once) and `watch` (on every change). --nss-only lives here so both the web
// pod's boot apply and its hot-reload watch can select POSIX-only convergence.
func applyFlags() flg.Flags {
	return append(commonFlags(),
		&flg.Switch{Name: "nss-only", Brief: "manage only POSIX accounts (/etc/passwd, groups, memberships); leave tdbsam, folders, ACLs, and quota to the SMB server"},
	)
}

// nssOnlyActions keeps only the actions a POSIX-only (NSS) apply may run: it
// manages /etc/passwd, /etc/group, and supplementary/administrator memberships,
// and it still reports refusals and orphans, but it leaves everything the SMB
// server owns — tdbsam accounts, the data-tree folders and their ACLs, and
// quotas — to that server. CreateGroup/CreateUser stay; the executor, told
// PosixOnly, runs only their account half. Unknown/new kinds default to dropped,
// the safe side for a mode whose whole purpose is not touching shared state.
func nssOnlyActions(in []reconcile.Action) []reconcile.Action {
	out := make([]reconcile.Action, 0, len(in))
	for _, a := range in {
		switch a.Kind {
		case reconcile.CreateGroup, reconcile.SetGroupAdmins,
			reconcile.CreateUser, reconcile.CreateUserDisabled, reconcile.UpdateUserGroups,
			reconcile.RefuseGroup, reconcile.OrphanGroup,
			reconcile.RefuseUser, reconcile.OrphanUser, reconcile.ReservedPresent:
			out = append(out, a)
		}
	}
	return out
}

func NewCmdApply() *xli.Command {
	return &xli.Command{
		Name:  "apply",
		Brief: "converge the system to the roster (idempotent, no destructive deletes)",
		Synop: "Collects actual state, diffs it against the roster, and executes the actions. Never deletes accounts (disable only); use purge for removal.",

		Flags: applyFlags(),

		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			// Config-level refusals come first: there is no point telling someone to
			// re-run as root for an operation that is going to be refused anyway.
			c := use_config.Must(ctx)
			if err := applyCommonFlags(cmd, c); err != nil {
				return err
			}
			if err := requireManageMode(c, "apply"); err != nil {
				return err
			}

			if err := requireRoot(); err != nil {
				return err
			}
			unlock, err := lockRun()
			if err != nil {
				return err
			}
			defer unlock()

			return runApplyOnce(ctx, cmd, c)
		}),
	}
}

// runApplyOnce loads the roster, collects the actual state, reconciles, executes
// the actions, and prints the report. It is the whole of `apply` minus the flag /
// root / lock preamble, factored out so `watch` can run the same cycle on each
// roster change. The caller holds the run lock and has checked root + manage mode.
func runApplyOnce(ctx context.Context, cmd *xli.Command, c *config.Config) error {
	cls := c.Classifier()
	nssOnly, _ := flg.Get[bool](cmd, "nss-only")

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
	// In NSS-only mode the SMB server owns quota, so no backend is built or probed
	// here regardless of what the config declares — the web pod has no reason to
	// reach /dev/zfs. Otherwise pre-flight the configured backend: if it cannot
	// enforce right now (e.g. zfs unreachable from this container), DEGRADE to no
	// enforcement with a loud warning rather than failing the run — apply is on the
	// boot path under `set -e`, and a quota that cannot be set must not take the
	// whole server down with it (the same best-effort stance darak takes on this
	// exact `zfs` call for usage accounting). ErrUnsupported (no backend
	// configured) is the silent, expected case.
	var qc quota.Controller = quota.Nop{}
	if !nssOnly {
		qc, err = quotaController(c, runner)
		if err != nil {
			return err
		}
		if err := qc.Probe(ctx); err != nil {
			if !errors.Is(err, quota.ErrUnsupported) {
				fmt.Fprintf(errW(cmd), "warning: quota backend unavailable, quotas NOT enforced this run: %v\n", err)
			}
			qc = quota.Nop{}
		}
	}
	actual, err := executor.Collect(ctx, p, s, cls, executor.CollectOpts{
		HomeBase:   c.Paths.Home,
		GroupsBase: c.Paths.Groups,
		FS:         fsops.OS{},
		Quota:      qc,
		Warn:       errW(cmd),
		// The web pod has samba-common-bin (smbpasswd/ntlm_auth) but not pdbedit, so
		// reading tdbsam would fail; NSS-only does not reconcile it anyway.
		SmbOptional: nssOnly,
	})
	if err != nil {
		return err
	}

	actions := reconcile.Reconcile(ro, actual, cls)
	// NSS-only keeps just the POSIX-account actions and drops everything the SMB
	// server owns (tdbsam, data-tree folders and ACLs, quota), so the report shows
	// only what this pod actually runs.
	if nssOnly {
		actions = nssOnlyActions(actions)
	}
	res := report.Result{DryRun: false, Actions: actions, Skipped: skipped}

	// A seed is only needed when some action creates an SMB account — never in
	// NSS-only mode, which creates no tdbsam accounts and so needs no seed mounted.
	var der *secret.Deriver
	if !nssOnly && needsSeed(actions) {
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
		Quota:      qc,
		HomeBase:   c.Paths.Home,
		GroupsBase: c.Paths.Groups,
		PosixOnly:  nssOnly,
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
