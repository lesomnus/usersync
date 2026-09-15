// Package fsops performs the filesystem side of provisioning (home and group
// folders, and the read-only views that carry reader groups). These operations
// are backend-invariant (the same on any Linux), so they are kept out of the
// account-backend abstraction. The FS interface lets the executor be unit-tested
// without a real (root-requiring) filesystem.
package fsops

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
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

	// ReadReaderGIDs returns the gids granted a read-only (r-x, no w) ACL entry
	// on path's ACCESS ACL, sorted.
	//
	// Readers are no longer expressed this way, so on a converged system this
	// answers empty. It is still read because a folder provisioned by an older
	// version carries those entries, and they have to come off before the view
	// is the only thing granting a reader — otherwise a reader taken out of the
	// roster would keep the access the ACL still gives them.
	ReadReaderGIDs(path string) ([]uint32, error)

	// ReadReaderViews returns, per group folder name under groupsBase, the
	// reader groups that currently hold a working read-only view of it.
	ReadReaderViews(groupsBase string) (map[string][]string, error)
	// EnsureReaderViews makes readers exactly the groups holding a read-only
	// view of the team's folder, and clears any reader ACL left on the folder
	// itself by an older version.
	EnsureReaderViews(groupsBase, team string, teamGID uint32, readers []ReaderGroup) error
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

// getfacl/setfacl are shelled out rather than reimplemented. The ACL wire
// format is a kernel/xattr detail (system.posix_acl_access), and the tools are
// the same ones an operator reaches for to check the result by hand — so what
// usersync writes and what `getfacl` shows an admin are produced by one
// implementation, not two that can disagree. The same reasoning applies to
// mount(8) in view.go.

// ReadReaderGIDs parses `getfacl` for named-group entries that grant read but
// not write on the access ACL.
func (OS) ReadReaderGIDs(path string) ([]uint32, error) {
	out, err := exec.Command("getfacl", "-pnE", path).Output()
	if err != nil {
		return nil, fmt.Errorf("getfacl %s: %w", path, err)
	}
	return parseReaderGIDs(string(out)), nil
}

// parseReaderGIDs extracts the numeric gids of named-group ACCESS entries that
// grant read but not write. Pulled out of ReadReaderGIDs so the getfacl parsing
// is testable without a real filesystem.
func parseReaderGIDs(getfacl string) []uint32 {
	var gids []uint32
	for _, line := range strings.Split(getfacl, "\n") {
		line = strings.TrimSpace(line)
		// A default entry ("default:...") is an inheritance template, not the
		// access grant being compared; the owning-group entry ("group::...") has
		// an empty gid field. Both are skipped.
		if strings.HasPrefix(line, "default:") || !strings.HasPrefix(line, "group:") {
			continue
		}
		// group:<gid>:<perms> — the numeric form, because getfacl was asked with
		// -n. An effective-rights comment (#effective:...) is trimmed off first.
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		f := strings.SplitN(line, ":", 3)
		if len(f) != 3 || f[1] == "" {
			continue
		}
		gid, err := strconv.ParseUint(f[1], 10, 32)
		if err != nil {
			continue
		}
		// A reader has r and not w. A writer group would show rw and is not a
		// reader; this is what lets one folder carry both.
		if strings.Contains(f[2], "r") && !strings.Contains(f[2], "w") {
			gids = append(gids, uint32(gid))
		}
	}
	sort.Slice(gids, func(i, j int) bool { return gids[i] < gids[j] })
	return gids
}

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
