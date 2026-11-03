package fsops

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

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

	// Empty is not enough: a person made this one (their uid, 0755), and taking
	// it would turn their folder into a root-owned mountpoint that revocation
	// later deletes.
	if err := os.Remove(filepath.Join(point, "notes.md")); err != nil {
		t.Fatal(err)
	}
	if err := ensureMountpoint(point); err == nil || !strings.Contains(err.Error(), "did not make") {
		t.Fatalf("ensureMountpoint took an empty directory it did not make: %v", err)
	}
}

// What ensureMountpoint makes itself it takes again: root-owned, viewMode. This
// is the state an unmounted view, or a run that stopped before mounting, leaves.
func TestEnsureMountpointIsIdempotentOnItsOwn(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: the mountpoint is chowned to root")
	}
	point := filepath.Join(t.TempDir(), "perception")
	if err := ensureMountpoint(point); err != nil {
		t.Fatalf("ensureMountpoint on a new path: %v", err)
	}
	if err := ensureMountpoint(point); err != nil {
		t.Fatalf("ensureMountpoint is not idempotent: %v", err)
	}
}

// A view is a read-only idmapped mount. A mount that only has a view's SHAPE —
// <views>/<X>/<team> — is somebody else's, and converging must neither take it
// down (a reader no longer declared) nor mount over it (a reader declared).
func TestEnsureReaderViewsLeavesNonViewMountsAlone(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "rl"), 0o750); err != nil {
		t.Fatal(err)
	}
	point := filepath.Join(base, "rl", "perception")
	fixture := "99 878 0:155 /data /" + strings.TrimPrefix(point, "/") + " rw,noatime shared:9 - zfs tank/rl-data rw\n"
	p := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(p, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	old := mountsFile
	mountsFile = p
	defer func() { mountsFile = old }()

	fs := OS{}
	// rl is not a declared reader of perception. Before, this was taken for a
	// stale view and unmounted.
	if err := fs.EnsureReaderViews(ViewLayout{Groups: t.TempDir(), Views: base}, "perception", 10001, nil); err != nil {
		t.Fatalf("EnsureReaderViews touched a mount that is not a view: %v", err)
	}
	// rl is declared. The view's place is taken by something else: refuse.
	err := fs.EnsureReaderViews(ViewLayout{Groups: t.TempDir(), Views: base}, "perception", 10001, []ReaderGroup{{Name: "rl", GID: 10014}})
	if err == nil || !strings.Contains(err.Error(), "not a read-only id-mapped view") {
		t.Fatalf("EnsureReaderViews over a non-view mount = %v; want a refusal", err)
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

func TestViewLayoutPath(t *testing.T) {
	l := ViewLayout{Groups: "/g", Views: "/v"}
	if got := l.Path("sim", "sim-ro"); got != "/v/sim-ro/sim" {
		t.Errorf("Path = %q; want /v/sim-ro/sim", got)
	}
}

// A view counts as granted only when it is mounted, read-only and idmapped, in
// the view namespace. A mount at the old location inside the reader's folder is
// not a view any more, and without a namespace there are no views at all.
func TestReadReaderViews(t *testing.T) {
	root := t.TempDir()
	groups, views := filepath.Join(root, "teams"), filepath.Join(root, "views")
	for _, d := range []string{
		filepath.Join(groups, "sim-ro", "sim"),
		filepath.Join(views, "sim-ro", "sim"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mount := func(point, opts string) string {
		return "1 1 0:1 /teams/sim " + point + " " + opts + " shared:1 - zfs tank/nas rw\n"
	}
	read := func(l ViewLayout, mounts ...string) map[string][]string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "mountinfo")
		if err := os.WriteFile(p, []byte(strings.Join(mounts, "")), 0o600); err != nil {
			t.Fatal(err)
		}
		old := mountsFile
		mountsFile = p
		defer func() { mountsFile = old }()
		got, err := OS{}.ReadReaderViews(l)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	l := ViewLayout{Groups: groups, Views: views}
	view := mount(filepath.Join(views, "sim-ro", "sim"), "ro,noatime,idmapped")
	old := mount(filepath.Join(groups, "sim-ro", "sim"), "ro,noatime,idmapped")
	writable := mount(filepath.Join(views, "sim-ro", "sim"), "rw,noatime,idmapped")

	for _, tc := range []struct {
		what   string
		layout ViewLayout
		mounts []string
		want   map[string][]string
	}{
		{"a view", l, []string{view}, map[string][]string{"sim": {"sim-ro"}}},
		{"the old location is not read", l, []string{old}, map[string][]string{}},
		{"a writable mount is not a view", l, []string{writable}, map[string][]string{}},
		{"no namespace, no views", ViewLayout{Groups: groups}, []string{view, old}, map[string][]string{}},
	} {
		if got := read(tc.layout, tc.mounts...); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: views = %v; want %v", tc.what, got, tc.want)
		}
	}
}

// Readers declared without a view namespace are refused: there is nowhere to
// put their views. Without readers there is nothing to do.
func TestEnsureReaderViewsNeedsANamespace(t *testing.T) {
	l := ViewLayout{Groups: t.TempDir()}
	if err := (OS{}).EnsureReaderViews(l, "sim", 10017, nil); err != nil {
		t.Errorf("no readers, no namespace: %v", err)
	}
	if err := (OS{}).EnsureReaderViews(l, "sim", 10017, []ReaderGroup{{Name: "sim-ro", GID: 10020}}); err == nil || !strings.Contains(err.Error(), "paths.views") {
		t.Errorf("readers without a namespace = %v; want a refusal naming paths.views", err)
	}
}

// A mountpoint left behind after its view came down (the removal failed) reads
// as drift, and converging removes it. Only the exact shape usersync makes
// counts: an empty directory, viewMode, owned by the mountpoint owner. Anything
// else of the same name is never reported nor removed.
func TestLeftoverMountpoints(t *testing.T) {
	old := mountpointUID
	mountpointUID = uint32(os.Getuid())
	defer func() { mountpointUID = old }()

	root := t.TempDir()
	groups, views := filepath.Join(root, "teams"), filepath.Join(root, "views")
	mk := func(p string, mode os.FileMode) string {
		t.Helper()
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mk(filepath.Join(groups, "sim"), 0o770)
	mk(filepath.Join(groups, "rl"), 0o770)
	leftover := mk(filepath.Join(views, "sim-ro", "sim"), viewMode)
	// Something not of usersync's shape, by mode.
	other := mk(filepath.Join(views, "sim-ro", "rl"), 0o770)
	// Ours by shape but not empty: not ours either.
	full := mk(filepath.Join(views, "ops", "sim"), 0o755)
	if err := os.WriteFile(filepath.Join(full, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(full, viewMode); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(full, 0o755) // so TempDir can clean up

	p := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	oldMounts := mountsFile
	mountsFile = p
	defer func() { mountsFile = oldMounts }()

	done := ViewLayout{Groups: groups, Views: views}
	got, err := OS{}.ReadReaderViews(done)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{"sim": {"sim-ro@leftover"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("views = %v; want %v", got, want)
	}

	if err := sweepLeftoverPoints(groups, views, "sim", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("the leftover mountpoint %s is still there (%v)", leftover, err)
	}
	for _, kept := range []string{other, full} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}
	got, err = OS{}.ReadReaderViews(done)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("after sweeping views = %v; want none", got)
	}

	// A reader still declared keeps its mountpoint: the view is about to be
	// mounted over it.
	mk(filepath.Join(views, "sim-ro", "sim"), viewMode)
	if err := sweepLeftoverPoints(groups, views, "sim", map[string]ReaderGroup{"sim-ro": {Name: "sim-ro"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(views, "sim-ro", "sim")); err != nil {
		t.Errorf("a declared reader's mountpoint was removed: %v", err)
	}
}
