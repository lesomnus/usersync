package fsops

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// ReaderGroup is a group that may read another group's folder, paired with the
// gid its view has to present. Both halves are needed: the name locates the view
// (it lives in that group's own folder) and the gid is what the mount maps to.
type ReaderGroup struct {
	Name string
	GID  uint32
}

// maxID is the exclusive top of the id space a mount idmapping can cover.
// (uid_t)-1 is not a valid id, so 4294967295 is the largest count the kernel
// accepts — the same number /proc/self/uid_map carries for a full mapping.
const maxID = 4294967295

// viewMode is the mode of a mountpoint directory. It is root:root rather than
// owned by the reader group on purpose: the team folder's gid becomes the
// reader's gid THROUGH the mapping, so a stat of the mountpoint showing the
// reader's gid means the view is mounted, and showing 0 means it is not. That
// makes one stat a complete answer, with no mount table to consult.
const viewMode = 0o555

// ViewPath is where reader's read-only view of team lives: inside the reader
// group's own folder, under the team's name.
//
// It goes there rather than in a views root of its own because the reader group
// already has a folder and an SMB share, and its members already reach both. A
// separate root would need a share of its own, and that share would need a name
// that cannot collide with any group's — while this arrangement adds nothing to
// smb.conf at all. It also generalises: `readers` is a LIST, and one mount can
// map the team's gid to exactly one reader gid, so there has to be a view per
// (team, reader) pair. Keyed by reader, each pair has an obvious home.
func ViewPath(groupsBase, team, reader string) string {
	return filepath.Join(groupsBase, reader, team)
}

// idmapSpec builds the `X-mount.idmap` mapping that makes files owned by
// teamGID appear to be owned by readerGID, and changes nothing else.
//
// Two properties are not optional, and getting either wrong is silent:
//
//   - It must be INJECTIVE. readerGID is the mapping's destination, so it must
//     not also appear as a source; a file on disk owned by the reader group is
//     therefore unmapped and shows up as the overflow id.
//   - It must COVER every id the tree contains. An unmapped id becomes the
//     overflow id (65534), and a file that lands there is unreadable by anyone
//     at all — root included. Covering the whole 32-bit space costs three or
//     four extents, so there is no reason to guess at a narrower window.
//
// A uid mapping is required even though no uid changes: mount_setattr rejects a
// gid-only idmapping with EINVAL.
func idmapSpec(teamGID, readerGID uint32) (string, error) {
	if teamGID == readerGID {
		return "", fmt.Errorf("a group cannot be its own reader (gid %d)", teamGID)
	}
	if teamGID >= maxID || readerGID >= maxID {
		return "", fmt.Errorf("gid out of mappable range (team %d, reader %d)", teamGID, readerGID)
	}

	ext := []string{fmt.Sprintf("u:0:0:%d", uint64(maxID))}
	// Identity below the lower of the two, the one substitution, identity in the
	// gap, identity above the higher — skipping readerGID as a source. Emitted in
	// ascending source order, which is how the kernel stores them anyway.
	lo, hi := teamGID, readerGID
	if hi < lo {
		lo, hi = hi, lo
	}
	add := func(from, to, count uint64) {
		if count > 0 {
			ext = append(ext, fmt.Sprintf("g:%d:%d:%d", from, to, count))
		}
	}
	add(0, 0, uint64(lo))
	if teamGID < readerGID {
		add(uint64(teamGID), uint64(readerGID), 1)
	}
	add(uint64(lo)+1, uint64(lo)+1, uint64(hi)-uint64(lo)-1)
	if readerGID < teamGID {
		add(uint64(teamGID), uint64(readerGID), 1)
	}
	add(uint64(hi)+1, uint64(hi)+1, uint64(maxID)-uint64(hi)-1)
	return strings.Join(ext, " "), nil
}

// ReadReaderViews returns, for each group folder under groupsBase, the reader
// groups that currently hold a WORKING read-only view of it.
//
// It reads the mount table rather than walking the tree, so the cost is the
// number of mounts, not the number of files — which is the whole point of the
// arrangement. "Working" is verified rather than assumed: a mount is counted
// only if it is read-only and idmapped AND its root presents the reader group's
// gid, so a view left over from before a gid change reads as absent and is
// rebuilt instead of being trusted.
func (o OS) ReadReaderViews(groupsBase string) (map[string][]string, error) {
	mounts, err := readMounts()
	if err != nil {
		return nil, err
	}
	base := filepath.Clean(groupsBase)

	out := map[string][]string{}
	for _, m := range mounts {
		reader, team, ok := splitViewPath(base, m.point)
		if !ok || !isReadOnlyIdmapped(m.opts) {
			continue
		}
		if !o.viewPresents(groupsBase, reader, m.point) {
			continue
		}
		out[team] = append(out[team], reader)
	}
	for team := range out {
		sort.Strings(out[team])
	}
	return out, nil
}

// viewPresents reports whether the mount at point shows the reader group's own
// gid, which is what the mapping is for.
//
// The gid comes from the reader's FOLDER because this runs during collection,
// where there is no roster to ask. On a converged system the two agree — the
// folder is chgrp'd to the group by EnsureGroupDir — and where they do not, the
// folder is drift in its own right and CreateGroup corrects it first. The
// post-mount check in ensureView does not settle for the proxy: it has the
// declared gid and compares against that.
func (o OS) viewPresents(groupsBase, reader, point string) bool {
	folder, err := os.Stat(filepath.Join(groupsBase, reader))
	if err != nil {
		return false
	}
	return viewGIDIs(point, folder.Sys().(*syscall.Stat_t).Gid)
}

// viewGIDIs reports whether the root of the mount at point presents gid. One
// stat answers it, because the mapping's whole visible effect is this number.
func viewGIDIs(point string, gid uint32) bool {
	fi, err := os.Stat(point)
	if err != nil {
		return false
	}
	return fi.Sys().(*syscall.Stat_t).Gid == gid
}

// EnsureReaderViews makes readers exactly the groups holding a read-only view of
// the team's folder: it mounts the ones that are missing or wrong, and unmounts
// the ones no longer declared.
//
// Nothing in the team's tree is touched. The folder keeps its 2770 root:<team>
// and its files their 0660 — a reader reads because the mount shows them the
// team's gid as their own, and cannot write because the mount is read-only, so
// declaring a reader costs the same whether the folder holds ten files or ten
// million.
//
// It also clears any named-group ACL entry left on the folder ITSELF by the
// previous implementation. That is one setfacl, not a walk: entries deeper in
// the tree are unreachable once the folder does not let the reader in, so they
// are left where they are rather than paid for again.
func (o OS) EnsureReaderViews(groupsBase, team string, teamGID uint32, readers []ReaderGroup) error {
	if err := o.clearFolderReaderACL(filepath.Join(groupsBase, team)); err != nil {
		return err
	}

	want := map[string]ReaderGroup{}
	for _, r := range readers {
		want[r.Name] = r
	}

	// Drop views of this team that are no longer declared. The mount table is the
	// only place they are recorded, so this is where a de-declared reader is
	// actually revoked.
	mounts, err := readMounts()
	if err != nil {
		return err
	}
	base := filepath.Clean(groupsBase)
	mounted := map[string]bool{}
	for _, m := range mounts {
		reader, t, ok := splitViewPath(base, m.point)
		if !ok || t != team {
			continue
		}
		if _, keep := want[reader]; keep {
			mounted[reader] = true
			continue
		}
		if err := unmountView(m.point); err != nil {
			return err
		}
		// The mountpoint is ours and empty by construction, so removing it leaves
		// the reader's folder as it was before the grant.
		_ = os.Remove(m.point)
	}

	for _, r := range readers {
		if err := o.ensureView(groupsBase, team, teamGID, r, mounted[r.Name]); err != nil {
			return err
		}
	}
	return nil
}

// ensureView makes one (team, reader) view current. isMounted says whether the
// path already carries a mount, which decides whether it has to come down first.
func (o OS) ensureView(groupsBase, team string, teamGID uint32, reader ReaderGroup, isMounted bool) error {
	point := ViewPath(groupsBase, team, reader.Name)
	if isMounted && viewGIDIs(point, reader.GID) && isReadOnlyPath(point) {
		return nil
	}

	spec, err := idmapSpec(teamGID, reader.GID)
	if err != nil {
		return fmt.Errorf("view of %s for %s: %w", team, reader.Name, err)
	}
	if isMounted {
		if err := unmountView(point); err != nil {
			return err
		}
	}
	if err := ensureMountpoint(point); err != nil {
		return err
	}

	// Read-only is set in the SAME call as the bind. Mounting writable and
	// remounting read-only afterwards would leave a window in which the reader —
	// who by then already sees the team's files as their own group — could write
	// to them.
	//
	// --rbind rather than --bind: a dataset mounted inside the team's folder is
	// part of what the reader was granted, and a non-recursive bind would leave
	// it out silently. If such a submount cannot be idmapped the mount fails,
	// which is the direction to fail in.
	src := filepath.Join(groupsBase, team)
	if out, err := run("mount", "--rbind", "-o", "ro,X-mount.idmap="+spec, src, point); err != nil {
		return fmt.Errorf("mount read-only view %s: %w: %s", point, err, strings.TrimSpace(out))
	}
	// Verified against the DECLARED gid, not against the reader's folder: this is
	// the one place that knows what the mapping was supposed to produce, and a
	// mount that came up presenting something else must not be left in place.
	if !viewGIDIs(point, reader.GID) || !isReadOnlyPath(point) {
		_ = unmountView(point)
		return fmt.Errorf("view %s did not come up read-only as gid %d", point, reader.GID)
	}
	return nil
}

// ensureMountpoint makes point an empty root-owned directory to mount over.
//
// A non-empty directory is refused rather than mounted over: the mount would
// hide whatever is in it for as long as it lasts, and a reader group's folder is
// a folder people put things in.
func ensureMountpoint(point string) error {
	fi, err := os.Stat(point)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(point, viewMode); err != nil {
			return err
		}
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("mountpoint %s exists and is not a directory", point)
	default:
		entries, err := os.ReadDir(point)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return fmt.Errorf("refusing to mount a read-only view over %s: the directory is not empty", point)
		}
	}
	if err := os.Chown(point, 0, 0); err != nil {
		return err
	}
	return os.Chmod(point, viewMode)
}

// unmountView takes a view down, lazily if it is busy. A lazy detach still
// revokes it immediately for every new lookup; only descriptors already open
// keep working, and waiting for those would mean a de-declared reader keeps
// their access until nobody is reading.
func unmountView(point string) error {
	if _, err := run("umount", point); err == nil {
		return nil
	}
	if out, err := run("umount", "-l", point); err != nil {
		return fmt.Errorf("unmount %s: %w: %s", point, err, strings.TrimSpace(out))
	}
	return nil
}

// clearFolderReaderACL removes named-group ACL entries from the folder itself,
// which is what the pre-view implementation used to grant readers. Dropping the
// entry on the folder is enough to close the grant: without x on the folder
// there is no way in to the entries beneath it.
func (o OS) clearFolderReaderACL(path string) error {
	gids, err := o.ReadReaderGIDs(path)
	if err != nil || len(gids) == 0 {
		return nil // no ACL support, or nothing to clear
	}
	if out, err := run("setfacl", "-b", "-k", path); err != nil {
		return fmt.Errorf("clear legacy reader ACL on %s: %w: %s", path, err, strings.TrimSpace(out))
	}
	return nil
}

// isReadOnlyPath reports whether the mount serving path is read-only.
func isReadOnlyPath(path string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	const stRdonly = 1 // ST_RDONLY
	return st.Flags&stRdonly != 0
}

// isReadOnlyIdmapped reports whether a mountinfo options field describes a
// read-only idmapped mount.
func isReadOnlyIdmapped(opts string) bool {
	var ro, idmapped bool
	for _, o := range strings.Split(opts, ",") {
		switch o {
		case "ro":
			ro = true
		case "idmapped":
			idmapped = true
		}
	}
	return ro && idmapped
}

// splitViewPath decomposes a mount point into the (reader, team) pair it would
// be a view for, or reports that it is not in that shape. A view sits exactly
// two components below the group folder root.
func splitViewPath(base, point string) (reader, team string, ok bool) {
	rel, err := filepath.Rel(base, filepath.Clean(point))
	if err != nil {
		return "", "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || parts[0] == "" || parts[0] == ".." || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// mountEntry is one line of /proc/self/mountinfo, reduced to the two fields that
// matter here: where it is mounted and the per-mount options.
type mountEntry struct {
	point string
	opts  string
}

// mountsFile is overridable so the parser can be tested against a fixture.
var mountsFile = "/proc/self/mountinfo"

// readMounts parses the mount table of the CALLING process.
//
// Mounts belong to a mount namespace, so this is deliberately self and not a
// host-wide view: each container makes its own views and sees its own. A pod
// that has not made them shows its users an empty directory, which is the safe
// way to be wrong.
func readMounts() ([]mountEntry, error) {
	f, err := os.Open(mountsFile)
	if err != nil {
		return nil, fmt.Errorf("read mount table: %w", err)
	}
	defer f.Close()

	var out []mountEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// id parent major:minor root mountpoint options [optional...] - type source superopts
		f := strings.Fields(sc.Text())
		if len(f) < 6 {
			continue
		}
		out = append(out, mountEntry{point: unescapeMount(f[4]), opts: f[5]})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read mount table: %w", err)
	}
	return out, nil
}

// unescapeMount undoes the octal escaping the kernel applies to the path fields
// of mountinfo (space, tab, newline and backslash).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
