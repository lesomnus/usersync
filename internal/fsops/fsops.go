// Package fsops performs the filesystem side of provisioning (home and group
// folders). These operations are backend-invariant (the same on any Linux), so
// they are kept out of the account-backend abstraction. The FS interface lets
// the executor be unit-tested without a real (root-requiring) filesystem.
package fsops

import (
	"os"
	"syscall"
)

// FS creates and permissions the home and group directories, and observes a
// directory's presence/mode/owner (used to detect drift / partial provisioning).
type FS interface {
	// EnsureGroupDir makes path 2770 (setgid) owned by group gid.
	EnsureGroupDir(path string, gid uint32) error
	// EnsureHomeDir makes path 0700 owned by uid:gid.
	EnsureHomeDir(path string, uid, gid uint32) error
	// Stat reports whether path exists and, if so, its permission bits (with the
	// setgid bit folded in as 0o2000) and owning uid/gid.
	Stat(path string) (exists bool, perm, uid, gid uint32)
}

// OS is the real filesystem implementation.
type OS struct{}

func (OS) Stat(path string) (bool, uint32, uint32, uint32) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, 0, 0, 0
	}
	perm := uint32(fi.Mode().Perm())
	if fi.Mode()&os.ModeSetgid != 0 {
		perm |= 0o2000
	}
	st := fi.Sys().(*syscall.Stat_t)
	return true, perm, st.Uid, st.Gid
}

func (OS) EnsureGroupDir(path string, gid uint32) error {
	if err := os.MkdirAll(path, 0o2770); err != nil {
		return err
	}
	// Preserve the owner user; set the owning group.
	if err := os.Chown(path, -1, int(gid)); err != nil {
		return err
	}
	// Re-apply mode explicitly: MkdirAll is subject to umask and MkdirAll does
	// not set the setgid bit reliably on a pre-existing dir.
	return os.Chmod(path, os.ModeSetgid|0o770)
}

func (OS) EnsureHomeDir(path string, uid, gid uint32) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := os.Chown(path, int(uid), int(gid)); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}
