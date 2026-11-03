//go:build mount

package fsops

import (
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

// These mount for real: id-mapped, read-only binds under a scratch directory.
// They need root and a kernel and filesystem that support id-mapped mounts, so
// they sit behind `-tags mount`. CI runs them (the `mount` job); run them by
// hand on the host that serves the views, before a change to how views are made
// reaches it:
//
//	go test -c -tags mount -o fsops-mount.test ./internal/fsops
//	sudo ./fsops-mount.test -test.v
//
// Nothing outside t.TempDir() is touched, and every mount is taken down.

const (
	mountTeamGID   = 48001
	mountReaderGID = 48002
)

func mountScratch(t *testing.T) (groups, views string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	root := t.TempDir()
	groups, views = filepath.Join(root, "teams"), filepath.Join(root, "views")
	team, reader := filepath.Join(groups, "sim"), filepath.Join(groups, "sim-ro")
	for _, d := range []struct {
		path string
		gid  int
	}{{team, mountTeamGID}, {reader, mountReaderGID}} {
		if err := os.MkdirAll(d.path, 0o2770); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(d.path, 0, d.gid); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d.path, 0o2770); err != nil {
			t.Fatal(err)
		}
	}
	f := filepath.Join(team, "data.txt")
	if err := os.WriteFile(f, []byte("x"), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(f, 0, mountTeamGID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		l := ViewLayout{Groups: groups, Views: views}
		_ = OS{}.EnsureReaderViews(l, "sim", mountTeamGID, nil)
		_ = OS{}.EnsureReaderViews(ViewLayout{Groups: groups, Legacy: true}, "sim", mountTeamGID, nil)
	})
	return groups, views
}

func gidOf(t *testing.T, p string) uint32 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Gid
}

func TestMountViewsThroughTheTransition(t *testing.T) {
	groups, views := mountScratch(t)
	reader := []ReaderGroup{{Name: "sim-ro", GID: mountReaderGID}}
	fs := OS{}

	readViews := func(l ViewLayout) []string {
		t.Helper()
		v, err := fs.ReadReaderViews(l)
		if err != nil {
			t.Fatal(err)
		}
		return v["sim"]
	}

	// 1. Before: legacy only, as it is deployed today.
	legacy := ViewLayout{Groups: groups, Legacy: true}
	if err := fs.EnsureReaderViews(legacy, "sim", mountTeamGID, reader); err != nil {
		t.Fatalf("legacy: %v", err)
	}
	if got := readViews(legacy); !reflect.DeepEqual(got, []string{"sim-ro"}) {
		t.Fatalf("legacy views = %v", got)
	}

	// 2. Transition: the same declaration under a layout with a namespace reads
	// as half-done until it is converged, then as done, with both mounted.
	transition := ViewLayout{Groups: groups, Views: views, Legacy: true}
	if got := readViews(transition); !reflect.DeepEqual(got, []string{"sim-ro@partial"}) {
		t.Fatalf("transition before converging = %v; want sim-ro@partial", got)
	}
	if err := fs.EnsureReaderViews(transition, "sim", mountTeamGID, reader); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if got := readViews(transition); !reflect.DeepEqual(got, []string{"sim-ro"}) {
		t.Fatalf("transition views = %v", got)
	}
	newPoint, oldPoint := ViewPath(views, "sim", "sim-ro"), ViewPath(groups, "sim", "sim-ro")
	for _, p := range []string{newPoint, oldPoint} {
		if g := gidOf(t, p); g != mountReaderGID {
			t.Errorf("%s presents gid %d; want the reader's %d", p, g, mountReaderGID)
		}
		if !isReadOnlyPath(p) {
			t.Errorf("%s is writable", p)
		}
		if g := gidOf(t, filepath.Join(p, "data.txt")); g != mountReaderGID {
			t.Errorf("a file under %s presents gid %d; want %d", p, g, mountReaderGID)
		}
	}
	// The namespace root lets everyone through and nobody list it; the group's
	// directory is the group's to enter and nobody's to write.
	if fi, err := os.Stat(views); err != nil || fi.Mode().Perm() != viewsRootMode {
		t.Errorf("%s mode = %v (%v); want %04o", views, fi.Mode().Perm(), err, viewsRootMode)
	}
	dir := filepath.Join(views, "sim-ro")
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if st.Uid != 0 || st.Gid != mountReaderGID || fi.Mode().Perm() != readerDirMode {
		t.Errorf("%s is %d:%d %04o; want 0:%d %04o", dir, st.Uid, st.Gid, fi.Mode().Perm(), mountReaderGID, readerDirMode)
	}

	// 3. Done: legacy off. Reads as stale until converged; converging takes the
	// legacy view down and leaves the reader's own folder as it was.
	done := ViewLayout{Groups: groups, Views: views}
	if got := readViews(done); !reflect.DeepEqual(got, []string{"sim-ro", "sim-ro@stale"}) {
		t.Fatalf("done before converging = %v; want [sim-ro sim-ro@stale]", got)
	}
	if err := fs.EnsureReaderViews(done, "sim", mountTeamGID, reader); err != nil {
		t.Fatalf("done: %v", err)
	}
	if got := readViews(done); !reflect.DeepEqual(got, []string{"sim-ro"}) {
		t.Fatalf("done views = %v", got)
	}
	if _, err := os.Stat(oldPoint); !os.IsNotExist(err) {
		t.Errorf("the legacy mountpoint %s is still there (%v)", oldPoint, err)
	}

	// 4. Revocation: no readers, no views anywhere.
	if err := fs.EnsureReaderViews(done, "sim", mountTeamGID, nil); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := readViews(done); len(got) != 0 {
		t.Fatalf("after revocation views = %v", got)
	}
}

// The constraint #13 started from: a group whose folder holds views cannot be
// given readers at the legacy location, because --rbind of its folder takes
// its id-mapped views along and those cannot be id-mapped again. In the view
// namespace the folder holds no views, so the same grant works.
func TestMountReaderGroupCanBeReadInTheNamespace(t *testing.T) {
	groups, views := mountScratch(t)
	fs := OS{}
	done := ViewLayout{Groups: groups, Views: views}

	// sim-ro reads sim.
	if err := fs.EnsureReaderViews(done, "sim", mountTeamGID, []ReaderGroup{{Name: "sim-ro", GID: mountReaderGID}}); err != nil {
		t.Fatalf("sim-ro reads sim: %v", err)
	}
	// all-ro reads sim-ro — the grant that failed with EPERM on nas.roster#32.
	allRO := filepath.Join(groups, "all-ro")
	if err := os.MkdirAll(allRO, 0o2770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(allRO, 0, 48003); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fs.EnsureReaderViews(done, "sim-ro", mountReaderGID, nil) })
	if err := fs.EnsureReaderViews(done, "sim-ro", mountReaderGID, []ReaderGroup{{Name: "all-ro", GID: 48003}}); err != nil {
		t.Fatalf("all-ro reads sim-ro in the namespace: %v", err)
	}
}
