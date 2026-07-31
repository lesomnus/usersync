// Package provider abstracts the OS account backend (create/modify users and
// groups, read actual state). One implementation per backend — shadow-utils
// (useradd/usermod/groupadd) today, busybox/pw later — behind a single
// interface so the reconciler and tests never depend on a specific toolset.
package provider

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/state"
)

// GroupSpec is the desired unix group for EnsureGroup.
type GroupSpec struct {
	Name string
	GID  uint32
}

// UserSpec is the desired unix user for EnsureUser. GID is the UPG (== UID).
type UserSpec struct {
	Name     string
	UID      uint32
	GID      uint32
	Home     string
	Shell    string
	FullName string
}

// Provider is the OS user/group lifecycle backend.
type Provider interface {
	// Scan collects the actual users and groups within the managed range. The
	// returned State has Users and Groups populated; Smb is left empty (SMB is
	// the Samba backend's concern).
	Scan(ctx context.Context) (*state.State, error)

	// EnsureGroup creates the group if absent (idempotent).
	EnsureGroup(ctx context.Context, g GroupSpec) error

	// EnsureUser creates the user and its UPG if absent (idempotent). It does
	// not manage supplementary groups (see SetSupplementaryGroups), the SMB
	// account, or the home directory contents.
	EnsureUser(ctx context.Context, u UserSpec) error

	// SetSupplementaryGroups replaces the user's supplementary group set exactly.
	SetSupplementaryGroups(ctx context.Context, user string, groups []string) error

	// LockPassword locks the unix password so SSH/console login is impossible.
	LockPassword(ctx context.Context, user string) error
}

// Detect selects a backend by name; "auto"/"" probes PATH. Only shadow-utils is
// implemented; busybox/pw are recognized but return a clear not-implemented error.
func Detect(name string, r run.Runner) (Provider, error) {
	switch name {
	case "shadow-utils", "shadowutils":
		return NewShadowUtils(r), nil
	case "busybox":
		return nil, fmt.Errorf("provider %q not implemented yet (only shadow-utils)", name)
	case "pw":
		return nil, fmt.Errorf("provider %q not implemented yet (only shadow-utils)", name)
	case "", "auto":
		if _, err := exec.LookPath("useradd"); err == nil {
			return NewShadowUtils(r), nil
		}
		return nil, fmt.Errorf("auto-detect: no supported account backend found (need useradd)")
	default:
		return nil, fmt.Errorf("unknown provider %q", name)
	}
}
