package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/state"
)

// pw is the FreeBSD backend: it reads actual state via getent and mutates it
// through the pw(8) command family (groupadd/useradd/usermod/lock). Presence is
// probed with pw's own show subcommands rather than getent. Every command is
// executed through the injected Runner, so it is fully unit-testable without
// root using run.Fake.
type pw struct {
	r run.Runner
	// etc is the directory holding the local account databases. Empty means
	// /etc; tests point it at a fixture.
	etc string
}

// NewPw returns a Provider backed by FreeBSD's pw(8) and getent, executing every
// command through r.
func NewPw(r run.Runner) Provider {
	return &pw{r: r}
}

// Scan reads every passwd and group entry via getent and builds the actual
// State. getent is available on FreeBSD, so the shared implementation is reused.
func (p *pw) Scan(ctx context.Context) (*state.State, error) {
	return scanViaGetent(ctx, p.r)
}

// EnsureGroup creates the group with `pw groupadd` if it is absent. It is
// idempotent: a present group is a no-op.
func (p *pw) EnsureGroup(ctx context.Context, g GroupSpec) error {
	if p.present(ctx, "groupshow", g.Name) {
		return nil
	}
	if _, err := p.r.Run(ctx, "", "pw", "groupadd", "-n", g.Name, "-g", u32(g.GID)); err != nil {
		return fmt.Errorf("pw groupadd %s: %w", g.Name, err)
	}
	return nil
}

// EnsureUser creates the user private group (UPG, gid == uid) and then the user
// if it is absent. It is idempotent: a present user is a no-op. Without -m no
// home directory is created; it does not manage supplementary groups or the SMB
// account.
func (p *pw) EnsureUser(ctx context.Context, u UserSpec) error {
	if p.present(ctx, "usershow", u.Name) {
		return nil
	}
	// Create the UPG only if absent, so a retry after an interrupted apply is idempotent.
	if !p.present(ctx, "groupshow", u.Name) {
		if _, err := p.r.Run(ctx, "", "pw", "groupadd", "-n", u.Name, "-g", u32(u.UID)); err != nil {
			return fmt.Errorf("pw groupadd %s: %w", u.Name, err)
		}
	}
	if _, err := p.r.Run(ctx, "", "pw", "useradd",
		"-n", u.Name,
		"-u", u32(u.UID),
		"-g", u32(u.GID),
		"-d", u.Home,
		"-s", u.Shell,
		"-c", u.FullName,
	); err != nil {
		return fmt.Errorf("pw useradd %s: %w", u.Name, err)
	}
	return nil
}

// SetSupplementaryGroups replaces the user's supplementary group set with
// `pw usermod -G` (replace semantics). An empty set passes an empty argument,
// which clears all supplementary groups.
func (p *pw) SetSupplementaryGroups(ctx context.Context, user string, groups []string) error {
	if _, err := p.r.Run(ctx, "", "pw", "usermod", user, "-G", strings.Join(groups, ",")); err != nil {
		return fmt.Errorf("pw usermod -G %s: %w", user, err)
	}
	return nil
}

// LockPassword locks the unix password with `pw lock` so SSH/console login is
// impossible.
func (p *pw) LockPassword(ctx context.Context, user string) error {
	if _, err := p.r.Run(ctx, "", "pw", "lock", user); err != nil {
		return fmt.Errorf("pw lock %s: %w", user, err)
	}
	return nil
}

// LookupUser resolves one user name through NSS. See lookupViaGetent for why
// this is keyed rather than read out of Scan's enumeration.
func (p *pw) LookupUser(ctx context.Context, name string) (uint32, bool, error) {
	return lookupViaGetent(ctx, p.r, "passwd", name)
}

// LookupGroup resolves one group name through NSS.
func (p *pw) LookupGroup(ctx context.Context, name string) (uint32, bool, error) {
	return lookupViaGetent(ctx, p.r, "group", name)
}

// RemoveAccount deletes the user and its UPG, keeping the home directory.
// `pw userdel` is deliberately called WITHOUT -r, so the files stay on disk
// owned by the numeric uid. It is idempotent — an absent user or group is
// skipped.
func (p *pw) RemoveAccount(ctx context.Context, user string, opts RemoveOpts) error {
	uid, found := localEntry(p.etc, "passwd", user)
	if found {
		// No -r, so the files stay on disk owned by the numeric uid.
		if _, err := p.r.Run(ctx, "", "pw", "userdel", "-n", user); err != nil {
			return fmt.Errorf("pw userdel %s: %w", user, err)
		}
	}
	if found && !opts.KeepUPG && isUPG(p.etc, user, uid) {
		if _, err := p.r.Run(ctx, "", "pw", "groupdel", "-n", user); err != nil {
			return fmt.Errorf("pw groupdel %s: %w", user, err)
		}
	}
	return nil
}

// present reports whether `pw <show> <name>` finds an entry. pw exits non-zero
// when the name is unknown, which means absent; a non-zero exit is therefore not
// treated as an error.
func (p *pw) present(ctx context.Context, show, name string) bool {
	_, err := p.r.Run(ctx, "", "pw", show, name)
	return err == nil
}

// SetGroupAdmins is not supported: there is no /etc/gshadow on this backend, so
// there is nowhere to record a group administrator. Reported rather than
// silently accepted, because a declared owner that goes nowhere would otherwise
// look like drift on every run.
func (p *pw) SetGroupAdmins(ctx context.Context, group string, admins []string) error {
	return ErrUnsupported
}
