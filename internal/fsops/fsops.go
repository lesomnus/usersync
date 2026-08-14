// Package fsops performs the filesystem side of provisioning (home and group
// folders). These operations are backend-invariant (the same on any Linux), so
// they are kept out of the account-backend abstraction. The FS interface lets
// the executor be unit-tested without a real (root-requiring) filesystem.
package fsops

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// ErrACLUnsupported means the filesystem at a path cannot store POSIX ACLs, so
// a declared reader group cannot be enforced there. Deliberately distinct from
// an ordinary setfacl failure: this one is a property of the deployment (a
// filesystem with no ACL support, or mounted without it), and the caller turns
// it into a refusal rather than a retry. On ZFS it means the dataset's acltype
// is off; on ext4/xfs it is on by default. Measured on the deployment's own
// pool before this was written — see the ADR-1 discussion.
var ErrACLUnsupported = errors.New("filesystem does not support POSIX ACLs")

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

	// ReadReaderGIDs returns the gids granted a read-only (r-x, no w) ACL entry
	// on path's ACCESS ACL, sorted. It is how reader drift is detected: the
	// reconciler compares this against the roster's declared reader gids.
	ReadReaderGIDs(path string) ([]uint32, error)
	// EnsureReaderACL makes readerGIDs exactly the read-only groups on path, as
	// both an access entry and a default entry (so files created afterwards
	// inherit it), and removes any named-group ACL entry not in the set. The
	// writer group keeps rwx via the base mode. Returns ErrACLUnsupported when
	// the filesystem cannot store ACLs at all.
	EnsureReaderACL(path string, writerGID uint32, readerGIDs []uint32) error
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

// getfacl/setfacl are shelled out rather than reimplemented. The ACL wire
// format is a kernel/xattr detail (system.posix_acl_access), and the tools are
// the same ones an operator reaches for to check the result by hand — so what
// usersync writes and what `getfacl` shows an admin are produced by one
// implementation, not two that can disagree.

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

// EnsureReaderACL sets the reader entries to exactly readerGIDs and clears any
// other named-group entry, on both the access and default ACLs.
//
// It rebuilds rather than patches: the whole named-group ACL is replaced from
// the declared set, so a reader removed from the roster loses the entry, which
// a series of `-m` additions would never do. The writer group's rwx comes from
// the folder's own group mode (2770), so it needs no ACL entry — and giving it
// one would only add a way for the two to disagree.
func (OS) EnsureReaderACL(path string, writerGID uint32, readerGIDs []uint32) error {
	if err := aclSupported(path); err != nil {
		return err
	}

	// Start from a clean slate so a de-declared reader cannot survive. -k drops
	// the default ACL, -b drops all extended access entries; the base
	// owner/group/other remain, so the 2770 mode is untouched.
	if _, err := run("setfacl", "-k", path); err != nil {
		return fmt.Errorf("clear default ACL on %s: %w", path, err)
	}
	if _, err := run("setfacl", "-b", path); err != nil {
		return fmt.Errorf("clear access ACL on %s: %w", path, err)
	}
	if len(readerGIDs) == 0 {
		return nil
	}

	// The default entries make inheritance work: a file created afterwards — over
	// the web or over SMB — is born with the reader's r-x already on it. The
	// mask must permit r-x, and the writer group and owner keep rwx via default
	// entries so inherited files stay writable by the team.
	spec := []string{"m", "d:u::rwx", "d:g::rwx", "d:o::---"}
	for _, gid := range readerGIDs {
		spec = append(spec,
			fmt.Sprintf("g:%d:r-x", gid),   // access: read the folder
			fmt.Sprintf("d:g:%d:r-x", gid), // default: and everything created in it
		)
	}
	args := []string{"-" + spec[0], strings.Join(spec[1:], ","), path}
	if _, err := run("setfacl", args...); err != nil {
		return fmt.Errorf("set reader ACL on %s: %w", path, err)
	}
	return nil
}

// aclSupported reports whether the filesystem at path can store ACLs, by
// probing with a harmless self-referential setfacl and reading the errno.
//
// The probe grants the file's own owning group what the mode already gives it,
// so on success nothing has changed; on a filesystem with no ACL support it
// fails with ENOTSUP/EOPNOTSUPP, which is the signal to refuse.
func aclSupported(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	gid := fi.Sys().(*syscall.Stat_t).Gid
	out, err := run("setfacl", "-m", fmt.Sprintf("g:%d:rwx", gid), path)
	if err == nil {
		return nil
	}
	low := strings.ToLower(out + " " + err.Error())
	if strings.Contains(low, "not supported") || strings.Contains(low, "notsup") ||
		strings.Contains(low, "operation not supported") {
		return fmt.Errorf("%w: %s", ErrACLUnsupported, path)
	}
	return fmt.Errorf("acl probe on %s: %w", path, err)
}

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
