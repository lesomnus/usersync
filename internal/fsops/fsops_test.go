package fsops

import (
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The parser is what turns getfacl output into the set compared against the
// roster, so the shapes getfacl actually emits are pinned here.
func TestParseReaderGIDs(t *testing.T) {
	// A real `getfacl -pnE` on a team folder with one writer (owning group) and
	// two reader groups, one of which the mask has rendered #effective:r--.
	const out = `# file: teams/perception
# owner: 0
# group: 10001
user::rwx
group::rwx
group:10011:r-x
group:10012:r-x	#effective:r--
mask::rwx
other::---
default:user::rwx
default:group::rwx
default:group:10011:r-x
default:mask::rwx
default:other::---
`
	got := parseReaderGIDs(out)
	want := []uint32{10011, 10012}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseReaderGIDs = %v; want %v", got, want)
	}
}

// A writer group (rw) is not a reader, and default entries are templates, not
// the access grant — neither may leak into the compared set.
func TestParseReaderGIDsExcludesWritersAndDefaults(t *testing.T) {
	const out = `group::rwx
group:10001:rwx
group:10011:r-x
default:group:10099:r-x
mask::rwx
`
	got := parseReaderGIDs(out)
	want := []uint32{10011}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseReaderGIDs = %v; want %v", got, want)
	}
}

// The mapping is the whole grant, and a mistake in it does not fail loudly: an
// id nobody mapped becomes the overflow id, which is unreadable even by root.
// So the two properties the kernel cares about are asserted directly, over the
// FULL id space, for both orderings of the two gids.
func TestIdmapSpecIsInjectiveAndTotal(t *testing.T) {
	for _, tc := range []struct{ team, reader uint32 }{
		{10001, 10011}, // team below reader — the ordinary roster shape
		{10011, 10001}, // reader below team
		{0, 10011},     // team gid 0: no identity range below it
		{10001, 0},     // reader gid 0
	} {
		spec, err := idmapSpec(tc.team, tc.reader)
		if err != nil {
			t.Fatalf("idmapSpec(%d,%d): %v", tc.team, tc.reader, err)
		}
		uids, gids := parseIdmap(t, spec)

		// uids are untouched, but the mapping has to be there at all: a gid-only
		// idmapping is rejected by mount_setattr with EINVAL.
		if len(uids) != 1 || uids[0] != (extent{0, 0, maxID}) {
			t.Errorf("idmapSpec(%d,%d) uid map = %v; want one identity extent over the whole space", tc.team, tc.reader, uids)
		}

		if got := mapID(gids, uint64(tc.team)); got != uint64(tc.reader) {
			t.Errorf("idmapSpec(%d,%d): team gid maps to %d; want the reader gid", tc.team, tc.reader, got)
		}
		checkInjective(t, gids)
		// Every id but the reader's own gid must be mapped. The reader's is the
		// destination, so mapping it as a source too would not be injective —
		// which is why a file on disk owned by the reader group is invisible in
		// the view, and why nothing is ever chgrp'd to it.
		checkCovers(t, gids, uint64(tc.reader))
	}
}

// A group cannot be its own reader: the mapping would have to send a gid to
// itself and to something else at once. The roster refuses this too, and this is
// the layer that cannot be talked into it.
func TestIdmapSpecRefusesSelfReader(t *testing.T) {
	if _, err := idmapSpec(10001, 10001); err == nil {
		t.Fatal("idmapSpec accepted a group as its own reader")
	}
}

// Against the mapping measured by hand on the deployment, so a refactor that
// changes the emitted extents has to be deliberate.
func TestIdmapSpecMatchesTheMeasuredMapping(t *testing.T) {
	got, err := idmapSpec(60001, 60002)
	if err != nil {
		t.Fatal(err)
	}
	const want = "u:0:0:4294967295 g:0:0:60001 g:60001:60002:1 g:60003:60003:4294907292"
	if got != want {
		t.Fatalf("idmapSpec = %q\nwant        %q", got, want)
	}
}

// mountinfo escapes the characters that would otherwise break its own field
// splitting, so a group folder with a space in its name still resolves.
func TestReadMountsParsesPointsAndOptions(t *testing.T) {
	const fixture = `21 20 0:20 / /proc rw,nosuid,relatime shared:5 - proc proc rw
2973 878 0:155 /teams/perception /srv/data/teams/perception-ro/perception ro,noatime,idmapped shared:814 - zfs tank/nas rw,xattr,posixacl
99 878 0:155 /x /srv/data/teams/od\040d/t rw,noatime shared:9 - zfs tank/nas rw
`
	dir := t.TempDir()
	p := filepath.Join(dir, "mountinfo")
	if err := os.WriteFile(p, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	old := mountsFile
	mountsFile = p
	defer func() { mountsFile = old }()

	got, err := readMounts()
	if err != nil {
		t.Fatal(err)
	}
	want := []mountEntry{
		{point: "/proc", opts: "rw,nosuid,relatime"},
		{point: "/srv/data/teams/perception-ro/perception", opts: "ro,noatime,idmapped"},
		{point: "/srv/data/teams/od d/t", opts: "rw,noatime"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("readMounts = %#v\nwant         %#v", got, want)
	}
}

// Only a read-only idmapped mount is a reader view. A writable bind of the same
// folder would let the reader write, so "idmapped" alone must not count.
func TestIsReadOnlyIdmapped(t *testing.T) {
	for opts, want := range map[string]bool{
		"ro,noatime,idmapped": true,
		"idmapped,ro":         true,
		"rw,noatime,idmapped": false,
		"ro,noatime":          false,
		"ro,noatime,idmap":    false, // a prefix is not the flag
	} {
		if got := isReadOnlyIdmapped(opts); got != want {
			t.Errorf("isReadOnlyIdmapped(%q) = %v; want %v", opts, got, want)
		}
	}
}

// A view is exactly two components below the group root. Anything else — the
// root itself, a group folder, something deeper, something outside — is somebody
// else's mount and must not be read as a grant.
func TestSplitViewPath(t *testing.T) {
	const base = "/srv/data/teams"
	for _, tc := range []struct {
		point        string
		reader, team string
		ok           bool
	}{
		{"/srv/data/teams/perception-ro/perception", "perception-ro", "perception", true},
		{"/srv/data/teams", "", "", false},
		{"/srv/data/teams/perception", "", "", false},
		{"/srv/data/teams/a/b/c", "", "", false},
		{"/srv/data/homes/alice", "", "", false},
	} {
		reader, team, ok := splitViewPath(base, tc.point)
		if ok != tc.ok || reader != tc.reader || team != tc.team {
			t.Errorf("splitViewPath(%q) = (%q,%q,%v); want (%q,%q,%v)",
				tc.point, reader, team, ok, tc.reader, tc.team, tc.ok)
		}
	}
}

// The view lives in the READER's folder, under the team's name — that is what
// makes the reader's existing share serve it with no new share at all.
func TestViewPath(t *testing.T) {
	got := ViewPath("/srv/data/teams", "perception", "perception-ro")
	want := "/srv/data/teams/perception-ro/perception"
	if got != want {
		t.Fatalf("ViewPath = %q; want %q", got, want)
	}
}

// Mounting over a directory hides whatever is in it for as long as the mount
// lasts, and a reader group's folder is a folder people put things in. A name
// collision must be an error somebody sees, not data that quietly disappears.
func TestEnsureMountpointRefusesNonEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	point := filepath.Join(dir, "perception")
	if err := os.Mkdir(point, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(point, "notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureMountpoint(point); err == nil {
		t.Fatal("ensureMountpoint mounted over a directory that had files in it")
	}

	// Empty is fine, and so is being called again on one it already made.
	if err := os.Remove(filepath.Join(point, "notes.md")); err != nil {
		t.Fatal(err)
	}
	if err := ensureMountpoint(point); err != nil {
		t.Fatalf("ensureMountpoint on an empty directory: %v", err)
	}
	if err := ensureMountpoint(point); err != nil {
		t.Fatalf("ensureMountpoint is not idempotent: %v", err)
	}
}

// Readers used to be written onto the folder as an ACL. While such an entry is
// there it is a second way in, one that outlives the roster declaring it — so
// converging a folder means clearing it. One setfacl on the folder is enough:
// without x on the folder there is no route to the entries beneath it.
func TestEnsureReaderViewsClearsTheLegacyFolderACL(t *testing.T) {
	if _, err := exec.LookPath("setfacl"); err != nil {
		t.Skip("setfacl not installed")
	}
	base := t.TempDir()
	team := filepath.Join(base, "perception")
	if err := os.Mkdir(team, 0o2770); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("setfacl", "-m", "g:10011:rX", team).CombinedOutput(); err != nil {
		t.Skipf("filesystem has no ACL support: %s", out)
	}

	fs := OS{}
	if gids, err := fs.ReadReaderGIDs(team); err != nil || len(gids) != 1 {
		t.Fatalf("setup: readers = %v, %v; want one entry", gids, err)
	}
	if err := fs.EnsureReaderViews(base, "perception", 10001, nil); err != nil {
		t.Fatalf("EnsureReaderViews: %v", err)
	}
	if gids, err := fs.ReadReaderGIDs(team); err != nil || len(gids) != 0 {
		t.Fatalf("after converging, readers = %v, %v; want none", gids, err)
	}
}

// --- helpers -------------------------------------------------------------

// extent is one `<from>:<to>:<count>` triple of an idmap spec.
type extent struct{ from, to, count uint64 }

func parseIdmap(t *testing.T, spec string) (uids, gids []extent) {
	t.Helper()
	for _, field := range strings.Fields(spec) {
		var kind rune
		var e extent
		if n, err := fmt.Sscanf(field, "%c:%d:%d:%d", &kind, &e.from, &e.to, &e.count); n != 4 || err != nil {
			t.Fatalf("unparsable extent %q in %q", field, spec)
		}
		switch kind {
		case 'u':
			uids = append(uids, e)
		case 'g':
			gids = append(gids, e)
		default:
			t.Fatalf("unknown extent kind %q", kind)
		}
	}
	return uids, gids
}

func mapID(exts []extent, id uint64) uint64 {
	for _, e := range exts {
		if id >= e.from && id < e.from+e.count {
			return e.to + (id - e.from)
		}
	}
	return 65534 // overflow: what the kernel shows for an unmapped id
}

// checkInjective asserts no id is a source twice and none is a destination
// twice. mount_setattr rejects an overlap outright, so this catches a bad spec
// here rather than as a mount failure on a live server.
func checkInjective(t *testing.T, exts []extent) {
	t.Helper()
	for i, a := range exts {
		for _, b := range exts[i+1:] {
			if a.from < b.from+b.count && b.from < a.from+a.count {
				t.Errorf("sources overlap: %v and %v", a, b)
			}
			if a.to < b.to+b.count && b.to < a.to+a.count {
				t.Errorf("destinations overlap: %v and %v", a, b)
			}
		}
	}
}

// checkCovers asserts every source id below maxID is mapped, except the one id
// that is allowed to be missing. It compares intervals rather than ids: the
// space is 2^32 wide and the claim is about its whole extent.
func checkCovers(t *testing.T, exts []extent, except uint64) {
	t.Helper()
	sorted := slices.SortedFunc(slices.Values(exts), func(a, b extent) int {
		return cmp.Compare(a.from, b.from)
	})
	// gap reports the hole [from, to) as acceptable only when it is exactly the
	// one id that may go unmapped.
	gap := func(from, to uint64) {
		if from >= to {
			return
		}
		if from == except && to == except+1 {
			return
		}
		t.Fatalf("ids [%d,%d) are unmapped (they would become the overflow id)", from, to)
	}
	var next uint64
	for _, e := range sorted {
		gap(next, e.from)
		if end := e.from + e.count; end > next {
			next = end
		}
	}
	gap(next, maxID)
}
