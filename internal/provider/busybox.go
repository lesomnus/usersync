package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/state"
)

// busybox is the busybox (Alpine) backend: it reads actual state via getent and
// mutates it through busybox's addgroup/adduser/delgroup applets. busybox has no
// usermod, so supplementary-group changes are computed as a diff and applied one
// membership at a time. Every command is executed through the injected Runner,
// so it is fully unit-testable without root using run.Fake.
type busybox struct {
	r run.Runner
	// etc is the directory holding the local account databases. Empty means
	// /etc; tests point it at a fixture.
	etc string
}

// NewBusybox returns a Provider backed by busybox (addgroup, adduser, delgroup,
// passwd) and getent, executing every command through r.
func NewBusybox(r run.Runner) Provider {
	return &busybox{r: r}
}

// Scan reads every passwd and group entry via getent and builds the actual
// State. getent is available on busybox, so the shared implementation is reused.
func (b *busybox) Scan(ctx context.Context) (*state.State, error) {
	return scanViaGetent(ctx, b.r)
}

// EnsureGroup creates the group with `addgroup -g` if it is absent. It is
// idempotent: a present group is a no-op.
func (b *busybox) EnsureGroup(ctx context.Context, g GroupSpec) error {
	if b.present(ctx, "group", g.Name) {
		return nil
	}
	if _, err := b.r.Run(ctx, "", "addgroup", "-g", u32(g.GID), g.Name); err != nil {
		return fmt.Errorf("addgroup %s: %w", g.Name, err)
	}
	return nil
}

// EnsureUser creates the user private group (UPG, gid == uid) and then the user
// if it is absent. It is idempotent: a present user is a no-op. It does not
// manage supplementary groups, the SMB account, or the home directory: -D means
// no password is assigned and -H means no home directory is created.
func (b *busybox) EnsureUser(ctx context.Context, u UserSpec) error {
	if b.present(ctx, "passwd", u.Name) {
		return nil
	}
	// Create the UPG only if absent, so a retry after an interrupted apply is idempotent.
	if !b.present(ctx, "group", u.Name) {
		if _, err := b.r.Run(ctx, "", "addgroup", "-g", u32(u.UID), u.Name); err != nil {
			return fmt.Errorf("addgroup %s: %w", u.Name, err)
		}
	}
	if _, err := b.r.Run(ctx, "", "adduser",
		"-u", u32(u.UID),
		"-h", u.Home,
		"-s", u.Shell,
		"-G", u.Name,
		"-g", u.FullName,
		"-D",
		"-H",
		u.Name,
	); err != nil {
		return fmt.Errorf("adduser %s: %w", u.Name, err)
	}
	return nil
}

// SetSupplementaryGroups replaces the user's supplementary group set exactly.
// busybox has no usermod, so the change is applied as a diff against the current
// membership (read via getent): groups desired but not present are added with
// `addgroup <user> <group>`, groups present but not desired are removed with
// `delgroup <user> <group>`. Adds and removes are issued in sorted order for
// determinism. If the user is not found in the scan its current groups are
// treated as empty.
func (b *busybox) SetSupplementaryGroups(ctx context.Context, user string, groups []string) error {
	st, err := scanViaGetent(ctx, b.r)
	if err != nil {
		return fmt.Errorf("scan for %s supplementary groups: %w", user, err)
	}

	current := map[string]bool{}
	if u, ok := st.Users[user]; ok {
		for _, g := range u.Groups {
			current[g] = true
		}
	}
	desired := map[string]bool{}
	for _, g := range groups {
		desired[g] = true
	}

	// Groups to add: desired but not current.
	var add []string
	for g := range desired {
		if !current[g] {
			add = append(add, g)
		}
	}
	// Groups to remove: current but not desired.
	var remove []string
	for g := range current {
		if !desired[g] {
			remove = append(remove, g)
		}
	}
	sort.Strings(add)
	sort.Strings(remove)

	// Both sets can carry scan-derived names (the add set via preserved
	// non-managed memberships): a name that isn't a safe account name (e.g. a
	// leading '-' from a manual /etc/group edit) must never reach a positional
	// exec arg where busybox would parse it as a flag.
	for _, g := range add {
		if !roster.ValidName(g) {
			continue
		}
		if _, err := b.r.Run(ctx, "", "addgroup", user, g); err != nil {
			return fmt.Errorf("addgroup %s %s: %w", user, g, err)
		}
	}
	for _, g := range remove {
		if !roster.ValidName(g) {
			continue
		}
		if _, err := b.r.Run(ctx, "", "delgroup", user, g); err != nil {
			return fmt.Errorf("delgroup %s %s: %w", user, g, err)
		}
	}
	return nil
}

// LockPassword locks the unix password with `passwd -l` so SSH/console login is
// impossible.
func (b *busybox) LockPassword(ctx context.Context, user string) error {
	if _, err := b.r.Run(ctx, "", "passwd", "-l", user); err != nil {
		return fmt.Errorf("passwd -l %s: %w", user, err)
	}
	return nil
}

// LookupUser resolves one user name through NSS. See lookupViaGetent for why
// this is keyed rather than read out of Scan's enumeration.
func (b *busybox) LookupUser(ctx context.Context, name string) (uint32, bool, error) {
	return lookupViaGetent(ctx, b.r, "passwd", name)
}

// LookupGroup resolves one group name through NSS.
func (b *busybox) LookupGroup(ctx context.Context, name string) (uint32, bool, error) {
	return lookupViaGetent(ctx, b.r, "group", name)
}

// RemoveAccount deletes the user and its UPG, keeping the home directory.
// busybox's `deluser` leaves the home alone unless --remove-home is passed, so
// the files stay on disk owned by the numeric uid. It is idempotent — an absent
// user or group is skipped.
func (b *busybox) RemoveAccount(ctx context.Context, user string, opts RemoveOpts) error {
	uid, found := localEntry(b.etc, "passwd", user)
	if found {
		// busybox's deluser leaves the home alone unless --remove-home is passed.
		if _, err := b.r.Run(ctx, "", "deluser", user); err != nil {
			return fmt.Errorf("deluser %s: %w", user, err)
		}
	}
	if found && !opts.KeepUPG && isUPG(b.etc, user, uid) {
		if _, err := b.r.Run(ctx, "", "delgroup", user); err != nil {
			return fmt.Errorf("delgroup %s: %w", user, err)
		}
	}
	return nil
}

// present reports whether `getent <db> <key>` finds an entry. getent exits
// non-zero (or prints nothing) when the key is unknown, both of which mean
// absent; a non-zero exit is therefore not treated as an error.
func (b *busybox) present(ctx context.Context, db, key string) bool {
	out, err := b.r.Run(ctx, "", "getent", db, key)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}
