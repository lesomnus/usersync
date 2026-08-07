// Package provider abstracts the OS account backend (create/modify users and
// groups, read actual state). One implementation per backend — shadow-utils
// (useradd/usermod/groupadd) today, busybox/pw later — behind a single
// interface so the reconciler and tests never depend on a specific toolset.
package provider

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"

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

	// LookupUser resolves ONE user name through NSS and returns its uid. It is a
	// keyed lookup, not a filter over Scan: winbind and sssd do not enumerate
	// domain accounts by default, so a directory-served user is absent from Scan
	// yet resolves perfectly well by name. A name that does not resolve is not an
	// error.
	LookupUser(ctx context.Context, name string) (uid uint32, found bool, err error)

	// LookupGroup is LookupUser for a group name.
	LookupGroup(ctx context.Context, name string) (gid uint32, found bool, err error)

	// RemoveAccount deletes the unix user and its UPG but leaves the home
	// directory and everything in it on disk. It is idempotent: an absent user
	// (or UPG) is a no-op.
	//
	// This is "release the local identity", not "delete the user": it exists so a
	// directory service (winbind/AD) can take a name over one account at a time
	// while the files — owned by numeric uid — stay exactly where they are.
	// Destroying data is purge's job, and only purge's.
	RemoveAccount(ctx context.Context, user string) error
}

// Detect selects a backend by name; "auto"/"" probes PATH in order
// useradd (shadow-utils), adduser (busybox), then pw (FreeBSD).
func Detect(name string, r run.Runner) (Provider, error) {
	switch name {
	case "shadow-utils", "shadowutils":
		return NewShadowUtils(r), nil
	case "busybox":
		return NewBusybox(r), nil
	case "pw":
		return NewPw(r), nil
	case "", "auto":
		// On the BSDs prefer pw: they also ship an `adduser` (a Perl wrapper, not
		// busybox), so probing adduser first would mis-select the busybox backend.
		switch runtime.GOOS {
		case "freebsd", "openbsd", "netbsd", "dragonfly":
			if _, err := exec.LookPath("pw"); err == nil {
				return NewPw(r), nil
			}
		}
		if _, err := exec.LookPath("useradd"); err == nil {
			return NewShadowUtils(r), nil
		}
		if _, err := exec.LookPath("adduser"); err == nil {
			return NewBusybox(r), nil
		}
		if _, err := exec.LookPath("pw"); err == nil {
			return NewPw(r), nil
		}
		return nil, fmt.Errorf("auto-detect: no supported account backend found (need useradd, adduser, or pw)")
	default:
		return nil, fmt.Errorf("unknown provider %q", name)
	}
}
