// Package provider abstracts the OS account backend (create/modify users and
// groups, read actual state). One implementation per backend — shadow-utils
// (useradd/usermod/groupadd) today, busybox/pw later — behind a single
// interface so the reconciler and tests never depend on a specific toolset.
package provider

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/state"
)

// RemoveOpts tunes RemoveAccount.
type RemoveOpts struct {
	// KeepUPG leaves the user's private group in the local database.
	//
	// After a handover the directory usually has no group object whose gid equals
	// the user's uid, so nothing resolves the group on the user's own files and
	// `ls -l` shows a bare number. Keeping the local entry (with
	// `group: files winbind`, where local wins) restores the name at almost no
	// cost, and it is the recommended remedy in identity-roadmap.md — which is
	// only available if this step does not destroy the entry first.
	KeepUPG bool
}

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

	// RemoveOpts tunes RemoveAccount.
	//
	// RemoveAccount deletes the unix user and its UPG but leaves the home
	// directory and everything in it on disk. It is idempotent: an absent user
	// (or UPG) is a no-op.
	//
	// This is "release the local identity", not "delete the user": it exists so a
	// directory service (winbind/AD) can take a name over one account at a time
	// while the files — owned by numeric uid — stay exactly where they are.
	// Destroying data is purge's job, and only purge's.
	RemoveAccount(ctx context.Context, user string, opts RemoveOpts) error

	// SetGroupAdmins replaces the group's administrator list — the third field
	// of /etc/gshadow, which `gpasswd -A` writes and which lets those people run
	// `gpasswd -a`/`-d` on that group without being root.
	//
	// It returns ErrUnsupported on a backend with no equivalent. That is not a
	// failure: busybox and pw have no gshadow at all, and a caller should say so
	// once rather than treat every declared owner as drift on every run.
	SetGroupAdmins(ctx context.Context, group string, admins []string) error
}

// ErrUnsupported reports that the backend has no equivalent of the operation.
// Distinct from a failure: nothing went wrong and retrying will not help, so
// callers report it once and carry on.
var ErrUnsupported = errors.New("provider: not supported by this backend")

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
