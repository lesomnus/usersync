// Package fsops performs the filesystem side of provisioning (home and group
// folders, and the read-only views that carry reader groups). These operations
// are backend-invariant (the same on any Linux), so they are kept out of the
// account-backend abstraction. The FS interface lets the executor be unit-tested
// without a real (root-requiring) filesystem.
package fsops

import (
	"os"
	"os/exec"
	"syscall"
)

// FS creates and permissions the home and group directories, maintains the
// read-only views that grant reader groups, and observes a directory's
// presence/mode/owner (used to detect drift / partial provisioning).
type FS interface {
	// EnsureGroupDir makes path a setgid directory owned by group gid, with the
	// given mode (perm bits plus 0o2000 setgid, e.g. 0o2770 private, or 0o2775 /
	// 0o2777 when the group grants anonymous read / write). A zero perm defaults
	// to 0o2770.
	EnsureGroupDir(path string, gid, perm uint32) error
	// EnsureHomeDir makes path 0700 owned by uid:gid.
	EnsureHomeDir(path string, uid, gid uint32) error
	// Stat reports whether path exists and, if so, its permission bits (with the
	// setgid bit folded in as 0o2000) and owning uid/gid.
	Stat(path string) (exists bool, perm, uid, gid uint32)

	// ReadReaderViews returns, per team folder, the reader groups that currently
	// hold a working read-only view of it in the view namespace.
	ReadReaderViews(l ViewLayout) (map[string][]string, error)
	// EnsureReaderViews makes readers exactly the groups holding a read-only
	// view of the team's folder.
	EnsureReaderViews(l ViewLayout, team string, teamGID uint32, readers []ReaderGroup) error
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

func (OS) EnsureGroupDir(path string, gid, perm uint32) error {
	mode := fileMode(perm)
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	// Preserve the owner user; set the owning group.
	if err := os.Chown(path, -1, int(gid)); err != nil {
		return err
	}
	// Re-apply mode explicitly: MkdirAll is subject to umask and MkdirAll does
	// not set the setgid bit reliably on a pre-existing dir.
	return os.Chmod(path, mode)
}

// fileMode turns a perm word that folds setgid in as 0o2000 (the state/roster
// convention) into an os.FileMode with os.ModeSetgid set. A zero perm defaults
// to a private setgid group folder (0o2770).
func fileMode(perm uint32) os.FileMode {
	if perm == 0 {
		perm = 0o2770
	}
	mode := os.FileMode(perm & 0o777)
	if perm&0o2000 != 0 {
		mode |= os.ModeSetgid
	}
	return mode
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

// mount(8) is shelled out rather than reimplemented (view.go): the tools are the
// same ones an operator reaches for to check the result by hand, so what
// usersync does and what they show are produced by one implementation.

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
