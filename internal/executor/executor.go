// Package executor turns the reconciler's actions into real system changes by
// dispatching each Action to the provider (OS accounts), samba (SMB), and fsops
// (directories). It also collects the actual State from those backends. All
// dependencies are interfaces, so the dispatch logic is unit-testable with fakes.
package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/lesomnus/usersync/internal/fsops"
	"github.com/lesomnus/usersync/internal/idrange"
	"github.com/lesomnus/usersync/internal/provider"
	"github.com/lesomnus/usersync/internal/reconcile"
	"github.com/lesomnus/usersync/internal/samba"
	"github.com/lesomnus/usersync/internal/secret"
	"github.com/lesomnus/usersync/internal/state"
)

// DefaultShell blocks interactive login for managed users.
const DefaultShell = "/usr/sbin/nologin"

// CollectOpts tunes state collection.
type CollectOpts struct {
	HomeBase   string
	GroupsBase string
	// FS, if set, is used to record whether each managed home/group directory
	// exists so the reconciler can heal a missing one.
	FS fsops.FS
	// SmbOptional downgrades an SMB scan failure (e.g. pdbedit needs root) to a
	// warning to Warn instead of a fatal error. Read-only commands set this.
	SmbOptional bool
	Warn        io.Writer
}

// Collect gathers the actual State from the account and SMB backends, filtered
// to the managed id range (so the reconciler never sees system or out-of-scope
// accounts). SMB accounts are keyed in by name.
func Collect(ctx context.Context, p provider.Provider, s samba.Samba, cls *idrange.Classifier, opts CollectOpts) (*state.State, error) {
	raw, err := p.Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("scan accounts: %w", err)
	}
	accts, err := s.Accounts(ctx)
	if err != nil {
		if !opts.SmbOptional {
			return nil, fmt.Errorf("scan smb accounts: %w", err)
		}
		if opts.Warn != nil {
			fmt.Fprintf(opts.Warn, "warning: SMB status unavailable (pdbedit): %v\n", err)
		}
		accts = nil
	}

	out := state.New()
	for name, u := range raw.Users {
		if cls.UID(u.UID) != idrange.Managed {
			continue
		}
		if opts.FS != nil {
			u.HomeExists, u.HomePerm, u.HomeUID, u.HomeGID = opts.FS.Stat(filepath.Join(opts.HomeBase, name))
		}
		out.Users[name] = u
	}
	for name, g := range raw.Groups {
		if cls.GID(g.GID) != idrange.Managed {
			continue
		}
		if opts.FS != nil {
			g.FolderExists, g.FolderPerm, _, g.FolderGID = opts.FS.Stat(filepath.Join(opts.GroupsBase, name))
		}
		out.Groups[name] = g
	}
	for name, a := range accts {
		out.Smb[name] = state.Smb{Name: a.Name, Enabled: a.Enabled}
	}
	return out, nil
}

// Deps is everything Apply needs to execute actions.
type Deps struct {
	Provider   provider.Provider
	Samba      samba.Samba
	Deriver    *secret.Deriver // required for actions that create SMB accounts
	FS         fsops.FS
	HomeBase   string
	GroupsBase string
	Shell      string // defaults to DefaultShell if empty
}

func (d Deps) shell() string {
	if d.Shell == "" {
		return DefaultShell
	}
	return d.Shell
}

// Apply executes each action. A failing action is recorded and the rest still
// run, so one bad entry does not block the whole roster; the joined error is
// returned at the end. Refuse/orphan-group actions are report-only (no-ops).
func (d Deps) Apply(ctx context.Context, actions []reconcile.Action) error {
	var errs []error
	for _, a := range actions {
		if err := d.one(ctx, a); err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", a.Kind, a.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (d Deps) one(ctx context.Context, a reconcile.Action) error {
	switch a.Kind {
	case reconcile.CreateGroup:
		if err := d.Provider.EnsureGroup(ctx, provider.GroupSpec{Name: a.Name, GID: a.GID}); err != nil {
			return err
		}
		return d.FS.EnsureGroupDir(filepath.Join(d.GroupsBase, a.Name), a.GID)

	case reconcile.CreateUser:
		return d.createUser(ctx, a, true)
	case reconcile.CreateUserDisabled:
		return d.createUser(ctx, a, false)

	case reconcile.UpdateUserGroups:
		return d.Provider.SetSupplementaryGroups(ctx, a.Name, a.Groups)

	case reconcile.EnsureHome:
		return d.FS.EnsureHomeDir(filepath.Join(d.HomeBase, a.Name), a.UID, a.UID)

	case reconcile.AddSmb:
		pw, err := d.initPW(a.Name)
		if err != nil {
			return err
		}
		if err := d.Samba.Create(ctx, a.Name, pw); err != nil {
			return err
		}
		return d.Samba.Enable(ctx, a.Name)

	case reconcile.EnableUser:
		return d.Samba.Enable(ctx, a.Name)

	case reconcile.DisableUser, reconcile.OrphanUser:
		return d.Samba.Disable(ctx, a.Name)

	case reconcile.ReservedPresent:
		return nil // standing notice; no mutation

	case reconcile.RefuseUser, reconcile.RefuseGroup, reconcile.OrphanGroup:
		return nil // report-only; no mutation

	default:
		return fmt.Errorf("unhandled action kind %v", a.Kind)
	}
}

// createUser performs the full user convergence: UPG+user, home dir,
// supplementary groups, unix password lock, then SMB registration (enabled or
// disabled). uid == gid (UPG). The home directory is created immediately after
// the account so a later failure cannot leave an account with no home. If an
// SMB account already exists (a.HasSmb), its password is left untouched — only
// its enabled state is reconciled (never reset an existing password).
func (d Deps) createUser(ctx context.Context, a reconcile.Action, enable bool) error {
	home := filepath.Join(d.HomeBase, a.Name)
	spec := provider.UserSpec{
		Name:     a.Name,
		UID:      a.UID,
		GID:      a.UID, // UPG
		Home:     home,
		Shell:    d.shell(),
		FullName: a.FullName,
	}
	if err := d.Provider.EnsureUser(ctx, spec); err != nil {
		return err
	}
	if err := d.FS.EnsureHomeDir(home, a.UID, a.UID); err != nil {
		return err
	}
	if err := d.Provider.SetSupplementaryGroups(ctx, a.Name, a.Groups); err != nil {
		return err
	}
	if err := d.Provider.LockPassword(ctx, a.Name); err != nil {
		return err
	}
	if !a.HasSmb {
		pw, err := d.initPW(a.Name)
		if err != nil {
			return err
		}
		if err := d.Samba.Create(ctx, a.Name, pw); err != nil {
			return err
		}
	}
	if enable {
		return d.Samba.Enable(ctx, a.Name)
	}
	return d.Samba.Disable(ctx, a.Name)
}

func (d Deps) initPW(user string) (string, error) {
	if d.Deriver == nil {
		return "", fmt.Errorf("no seed configured: cannot derive initial password (set seed_file or USERSYNC_SEED)")
	}
	return d.Deriver.InitPW(user), nil
}
