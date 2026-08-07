package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/state"
)

// shadowUtils is the shadow-utils backend: it reads actual state via getent and
// mutates it through the classic useradd/usermod/groupadd family. Every command
// is executed through the injected Runner, so it is fully unit-testable without
// root using run.Fake.
type shadowUtils struct {
	r run.Runner
}

// NewShadowUtils returns a Provider backed by shadow-utils (useradd, usermod,
// groupadd) and getent, executing every command through r.
func NewShadowUtils(r run.Runner) Provider {
	return &shadowUtils{r: r}
}

// Scan reads every passwd and group entry via getent and builds the actual
// State. It does not filter by id range — the caller classifies. A user's
// supplementary groups are the groups whose member list names the user,
// excluding the user's primary group. Smb is left empty. Malformed lines
// (too few fields or unparseable uid/gid) are skipped.
func (s *shadowUtils) Scan(ctx context.Context) (*state.State, error) {
	st := state.New()

	passwd, err := s.r.Run(ctx, "", "getent", "passwd")
	if err != nil {
		return nil, fmt.Errorf("getent passwd: %w", err)
	}
	group, err := s.r.Run(ctx, "", "getent", "group")
	if err != nil {
		return nil, fmt.Errorf("getent group: %w", err)
	}

	// passwd line: name:x:uid:gid:gecos:home:shell
	for _, line := range strings.Split(passwd, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// name:x:uid:gid:gecos:home:shell — the GECOS field may itself contain a
		// colon, so parse the fixed fields from both ends and treat everything in
		// between as the gecos.
		f := strings.Split(line, ":")
		if len(f) < 7 {
			continue
		}
		uid, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			continue
		}
		gid, err := strconv.ParseUint(f[3], 10, 32)
		if err != nil {
			continue
		}
		name := f[0]
		shell := f[len(f)-1]
		home := f[len(f)-2]
		gecos := strings.Join(f[4:len(f)-2], ":")
		st.Users[name] = state.User{
			Name:     name,
			UID:      uint32(uid),
			GID:      uint32(gid),
			FullName: gecos,
			Home:     home,
			Shell:    shell,
		}
	}

	// group line: name:x:gid:member1,member2 (members may be empty)
	for _, line := range strings.Split(group, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) < 4 {
			continue
		}
		gid, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			continue
		}
		name := f[0]
		st.Groups[name] = state.Group{Name: name, GID: uint32(gid)}

		if f[3] == "" {
			continue
		}
		for _, member := range strings.Split(f[3], ",") {
			member = strings.TrimSpace(member)
			if member == "" {
				continue
			}
			u, ok := st.Users[member]
			if !ok {
				continue
			}
			// A membership that names the user's own primary group is not a
			// supplementary group.
			if uint32(gid) == u.GID {
				continue
			}
			u.Groups = append(u.Groups, name)
			st.Users[member] = u
		}
	}

	return st, nil
}

// EnsureGroup creates the group with groupadd if it is absent. It is idempotent:
// a present group is a no-op.
func (s *shadowUtils) EnsureGroup(ctx context.Context, g GroupSpec) error {
	if s.present(ctx, "group", g.Name) {
		return nil
	}
	if _, err := s.r.Run(ctx, "", "groupadd", "-g", u32(g.GID), g.Name); err != nil {
		return fmt.Errorf("groupadd %s: %w", g.Name, err)
	}
	return nil
}

// EnsureUser creates the user private group (UPG, gid == uid) and then the user
// if it is absent. It is idempotent: a present user is a no-op. It does not
// manage supplementary groups, the SMB account, or the home directory.
func (s *shadowUtils) EnsureUser(ctx context.Context, u UserSpec) error {
	if s.present(ctx, "passwd", u.Name) {
		return nil
	}
	// Create the UPG only if it isn't already present, so a retry after an apply
	// that was interrupted between groupadd and useradd is idempotent.
	if !s.present(ctx, "group", u.Name) {
		if _, err := s.r.Run(ctx, "", "groupadd", "-g", u32(u.UID), u.Name); err != nil {
			return fmt.Errorf("groupadd %s: %w", u.Name, err)
		}
	}
	if _, err := s.r.Run(ctx, "", "useradd",
		"-u", u32(u.UID),
		"-g", u32(u.GID),
		"-M",
		"-d", u.Home,
		"-s", u.Shell,
		"-c", u.FullName,
		u.Name,
	); err != nil {
		return fmt.Errorf("useradd %s: %w", u.Name, err)
	}
	return nil
}

// SetSupplementaryGroups replaces the user's supplementary group set with
// usermod -G (replace semantics). An empty set passes an empty argument, which
// clears all supplementary groups.
func (s *shadowUtils) SetSupplementaryGroups(ctx context.Context, user string, groups []string) error {
	if _, err := s.r.Run(ctx, "", "usermod", "-G", strings.Join(groups, ","), user); err != nil {
		return fmt.Errorf("usermod -G %s: %w", user, err)
	}
	return nil
}

// LockPassword locks the unix password with usermod -L so SSH/console login is
// impossible.
func (s *shadowUtils) LockPassword(ctx context.Context, user string) error {
	if _, err := s.r.Run(ctx, "", "usermod", "-L", user); err != nil {
		return fmt.Errorf("usermod -L %s: %w", user, err)
	}
	return nil
}

// RemoveAccount deletes the user and its UPG, keeping the home directory.
// `userdel` is deliberately called WITHOUT -r: the files stay on disk, owned by
// the numeric uid, waiting for the directory service to resolve that number
// again. It is idempotent — an absent user or group is skipped.
func (s *shadowUtils) RemoveAccount(ctx context.Context, user string) error {
	if s.present(ctx, "passwd", user) {
		if _, err := s.r.Run(ctx, "", "userdel", user); err != nil {
			return fmt.Errorf("userdel %s: %w", user, err)
		}
	}
	// userdel already drops the UPG when it is the user's primary group and has
	// no other members; this mops up the case where it does not.
	if s.present(ctx, "group", user) {
		if _, err := s.r.Run(ctx, "", "groupdel", user); err != nil {
			return fmt.Errorf("groupdel %s: %w", user, err)
		}
	}
	return nil
}

// present reports whether `getent <db> <key>` finds an entry. getent exits
// non-zero (or prints nothing) when the key is unknown, both of which mean
// absent; a non-zero exit is therefore not treated as an error.
func (s *shadowUtils) present(ctx context.Context, db, key string) bool {
	out, err := s.r.Run(ctx, "", "getent", db, key)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// u32 formats an id for a command-line argument.
func u32(v uint32) string { return strconv.FormatUint(uint64(v), 10) }
