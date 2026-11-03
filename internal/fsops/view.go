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
// (<views>/<name>/<team>) and the gid is what the mount maps to.
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

// ViewPath is the view of team for reader under one base: <base>/<reader>/<team>.
// Which bases are in use is a ViewLayout's to say.
func ViewPath(base, team, reader string) string {
	return filepath.Join(base, reader, team)
}

// ViewLayout says where read-only views are mounted: <views>/<reader>/<team>,
// a namespace only usersync writes, with <views>/<reader> root:<reader> 0750 so
// only that group enters it, under a <views> nobody can list. The source of a
// view is the team's folder under Groups.
//
// Views began inside the reader group's own folder (<groups>/<reader>/<team>).
// That put a system object in a folder people write in, identified it by its
// name, and kept a group holding views from having readers itself — an
// id-mapped mount cannot be id-mapped again. That location is gone (usersync#13);
// an installation still using it moves through a release that has
// `legacy_views: false` first.
type ViewLayout struct {
	Groups string // team folders, the source of every view
	Views  string // the view namespace; empty means no views can be made
}

// Path is where team's view for reader is mounted, and where a consumer opens it.
func (l ViewLayout) Path(team, reader string) string {
	return ViewPath(l.Views, team, reader)
}

// Reader-view state tags. A reader whose view is not as the layout wants it is
// reported under a tagged name, which never equals a declared reader — so the
// reconciler sees drift and converges, and `explain` shows why.
const (
	viewLeftover = "@leftover" // an empty mountpoint usersync made, with no view on it
)

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

// ReadReaderViews returns, for each team folder, the reader groups that hold a
// WORKING read-only view of it in the view namespace.
//
// It reads the mount table rather than walking the tree, so the cost is the
// number of mounts, not the number of files — which is the whole point of the
// arrangement. "Working" is verified rather than assumed: a mount is counted
// only if it is read-only and idmapped AND its root presents the reader group's
// gid, so a view left over from before a gid change reads as absent and is
// rebuilt instead of being trusted.
//
// A mountpoint left behind by a view that is gone comes back as
// "<reader>@leftover", which never equals a declared name, so it reads as drift.
func (o OS) ReadReaderViews(l ViewLayout) (map[string][]string, error) {
	out := map[string][]string{}
	if l.Views == "" {
		return out, nil
	}
	mounts, err := readMounts()
	if err != nil {
		return nil, err
	}
	base := filepath.Clean(l.Views)
	for _, m := range mounts {
		reader, team, ok := splitViewPath(base, m.point)
		if !ok || !isReadOnlyIdmapped(m.opts) {
			continue
		}
		if !o.viewPresents(base, reader, m.point) {
			continue
		}
		out[team] = append(out[team], reader)
	}

	// A mountpoint whose view is gone but which is still there is drift too:
	// apply removes it. Without this it would outlive a failed removal forever,
	// because nothing else reads as a change once the mount is down.
	left, err := leftoverPoints(l.Groups, base, "", mountedPoints(mounts))
	if err != nil {
		return nil, err
	}
	for team, readers := range left {
		for _, r := range readers {
			out[team] = append(out[team], r+viewLeftover)
		}
	}
	for team := range out {
		sort.Strings(out[team])
	}
	return out, nil
}

// mountedPoints is the set of paths something is mounted on.
func mountedPoints(mounts []mountEntry) map[string]bool {
	out := make(map[string]bool, len(mounts))
	for _, m := range mounts {
		out[filepath.Clean(m.point)] = true
	}
	return out
}

// leftoverPoints finds, under base, the mountpoints usersync made that nothing
// is mounted on: <base>/<reader>/<team> for every team folder under groupsBase
// (only team, if it is not empty), keyed by team.
//
// A mountpoint is normally removed right after its view is unmounted. When that
// removal fails — the unmount fell back to a lazy detach and the directory was
// still busy, say — the directory stays: empty, root-owned, viewMode. Only that
// exact shape counts. People cannot make a root-owned directory, so this never
// takes one of theirs; anything else under a reader's folder is left alone.
func leftoverPoints(groupsBase, base, team string, isMounted map[string]bool) (map[string][]string, error) {
	teams := map[string]bool{}
	if team != "" {
		teams[team] = true
	} else {
		entries, err := os.ReadDir(groupsBase)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				teams[e.Name()] = true
			}
		}
	}
	readers, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := map[string][]string{}
	for _, r := range readers {
		if !r.IsDir() {
			continue
		}
		for t := range teams {
			if t == r.Name() {
				continue
			}
			point := ViewPath(base, t, r.Name())
			if isMounted[filepath.Clean(point)] || !isLeftoverPoint(point) {
				continue
			}
			out[t] = append(out[t], r.Name())
		}
	}
	return out, nil
}

// mountpointUID is who owns a mountpoint usersync made. A variable so the
// leftover rules can be tested without root.
var mountpointUID uint32 = 0

// isLeftoverPoint reports whether point is an empty directory of the exact shape
// ensureMountpoint leaves: owned by root, viewMode.
func isLeftoverPoint(point string) bool {
	fi, err := os.Lstat(point)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != viewMode {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != mountpointUID {
		return false
	}
	f, err := os.Open(point)
	if err != nil {
		return false
	}
	defer f.Close()
	names, _ := f.Readdirnames(1)
	return len(names) == 0
}

// viewPresents reports whether the mount at point shows the reader group's own
// gid, which is what the mapping is for. base/reader is the directory usersync
// made for the reader in the view namespace, which carries the reader's gid.
//
// The gid comes from that DIRECTORY because this runs during collection, where
// there is no roster to ask. On a converged system the two agree, since
// ensureReaderDir chowns it to the group on every grant. The post-mount check in
// ensureView does not settle for the proxy: it has the declared gid and compares
// against that.
func (o OS) viewPresents(base, reader, point string) bool {
	folder, err := os.Stat(filepath.Join(base, reader))
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
func (o OS) EnsureReaderViews(l ViewLayout, team string, teamGID uint32, readers []ReaderGroup) error {
	if l.Views == "" {
		if len(readers) > 0 {
			return fmt.Errorf("readers of %s need paths.views: there is nowhere to mount their views", team)
		}
		return nil
	}
	mounts, err := readMounts()
	if err != nil {
		return err
	}
	return o.convergeViews(mounts, l.Groups, l.Views, team, teamGID, readers)
}

// convergeViews makes readers exactly the views of team under base.
func (o OS) convergeViews(mounts []mountEntry, groupsBase, base, team string, teamGID uint32, readers []ReaderGroup) error {
	want := map[string]ReaderGroup{}
	for _, r := range readers {
		want[r.Name] = r
	}

	// Drop views of this team that are no longer declared. The mount table is the
	// only place they are recorded, so this is where a de-declared reader is
	// actually revoked.
	clean := filepath.Clean(base)
	mounted := map[string]bool{}
	for _, m := range mounts {
		reader, t, ok := splitViewPath(clean, m.point)
		if !ok || t != team {
			continue
		}
		// A view is a read-only idmapped mount, and only that: the same test
		// ReadReaderViews uses to say what is granted. A path that merely has the
		// shape <base>/<X>/<team> can be anything — a dataset somebody mounted in
		// a team folder under a name that happens to be a team's — and taking it
		// down because of its name would unmount data that was never ours. It is
		// left alone; if it sits exactly where a declared view must go, the grant
		// is refused rather than mounted over it.
		if !isReadOnlyIdmapped(m.opts) {
			if _, declared := want[reader]; declared {
				return fmt.Errorf("refusing to make the view %s: something that is not a read-only id-mapped view is mounted there", m.point)
			}
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
		// the folder holding it as it was before the grant. A failure here is
		// retried by the sweep below, which reports it.
		_ = os.Remove(m.point)
	}

	// Remove the mountpoints of this team's views that are gone: the ones just
	// unmounted whose removal failed, and any an earlier run left behind.
	if err := sweepLeftoverPoints(groupsBase, base, team, want); err != nil {
		return err
	}

	for _, r := range readers {
		if err := ensureReaderDir(base, r); err != nil {
			return err
		}
		if err := o.ensureView(groupsBase, base, team, teamGID, r, mounted[r.Name]); err != nil {
			return err
		}
	}
	return nil
}

// sweepLeftoverPoints removes team's leftover mountpoints under base, except
// those of readers still wanted there (ensureView mounts over those).
func sweepLeftoverPoints(groupsBase, base, team string, want map[string]ReaderGroup) error {
	mounts, err := readMounts()
	if err != nil {
		return err
	}
	left, err := leftoverPoints(groupsBase, base, team, mountedPoints(mounts))
	if err != nil {
		return err
	}
	for _, r := range left[team] {
		if _, keep := want[r]; keep {
			continue
		}
		if err := os.Remove(ViewPath(base, team, r)); err != nil {
			return fmt.Errorf("remove the mountpoint of a view that is gone: %w", err)
		}
	}
	return nil
}

// ensureReaderDir makes <views>/<reader> the directory a reader group's views
// live in: root:<reader> 0750, under a root-owned <views>. Only the group's
// members enter it, and nobody writes in it — it holds mountpoints and nothing
// else, which is what keeps the view namespace usersync's alone.
//
// <views> itself is 0711: anyone may pass through it to their own group's
// directory, nobody may list it. A listing would name every reader group, and
// the web serves the data root to anonymous visitors too; nothing needs it,
// since the share and the routed path both name <views>/<reader> directly.
func ensureReaderDir(viewsBase string, r ReaderGroup) error {
	if err := os.MkdirAll(viewsBase, viewsRootMode); err != nil {
		return err
	}
	if err := os.Chown(viewsBase, 0, 0); err != nil {
		return err
	}
	if err := os.Chmod(viewsBase, viewsRootMode); err != nil {
		return err
	}
	dir := filepath.Join(viewsBase, r.Name)
	if err := os.MkdirAll(dir, readerDirMode); err != nil {
		return err
	}
	if err := os.Chown(dir, 0, int(r.GID)); err != nil {
		return err
	}
	return os.Chmod(dir, readerDirMode)
}

// readerDirMode is <views>/<reader>: the group may list and enter, nobody writes.
const readerDirMode = 0o750

// viewsRootMode is <views>: traversable by all, listable by none.
const viewsRootMode = 0o711

// ensureView makes one (team, reader) view current. isMounted says whether the
// path already carries a mount, which decides whether it has to come down first.
func (o OS) ensureView(groupsBase, base, team string, teamGID uint32, reader ReaderGroup, isMounted bool) error {
	point := ViewPath(base, team, reader.Name)
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
// hide whatever is in it for as long as it lasts. Nobody but usersync writes in
// the view namespace, so this is a guard against a broken invariant, not
// against people.
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
		// An empty directory is taken only if it is one of ours: root-owned and
		// viewMode, which is what this function leaves behind (a view that was
		// unmounted, or a run that stopped between here and the mount). Anything
		// else is not usersync's, and taking it would make it a root-owned
		// mountpoint and, at revocation, delete it.
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || fi.Mode().Perm() != viewMode {
			return fmt.Errorf("refusing to mount a read-only view over %s: an empty directory that usersync did not make is there (owner uid %d, mode %04o) — rename or remove it", point, statUID(st), fi.Mode().Perm())
		}
	}
	if err := os.Chown(point, 0, 0); err != nil {
		return err
	}
	return os.Chmod(point, viewMode)
}

// statUID is the owner of st for an error message, or -1 when it is unknown.
func statUID(st *syscall.Stat_t) int64 {
	if st == nil {
		return -1
	}
	return int64(st.Uid)
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
