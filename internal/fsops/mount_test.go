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

func TestMountViewLifecycle(t *testing.T) {
	groups, views := mountScratch(t)
	reader := []ReaderGroup{{Name: "sim-ro", GID: mountReaderGID}}
	fs := OS{}
	l := ViewLayout{Groups: groups, Views: views}

	readViews := func() []string {
		t.Helper()
		v, err := fs.ReadReaderViews(l)
		if err != nil {
			t.Fatal(err)
		}
		return v["sim"]
	}

	// 1. Grant.
	if err := fs.EnsureReaderViews(l, "sim", mountTeamGID, reader); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if got := readViews(); !reflect.DeepEqual(got, []string{"sim-ro"}) {
		t.Fatalf("views = %v", got)
	}
	point := ViewPath(views, "sim", "sim-ro")
	if g := gidOf(t, point); g != mountReaderGID {
		t.Errorf("%s presents gid %d; want the reader's %d", point, g, mountReaderGID)
	}
	if !isReadOnlyPath(point) {
		t.Errorf("%s is writable", point)
	}
	if g := gidOf(t, filepath.Join(point, "data.txt")); g != mountReaderGID {
		t.Errorf("a file under %s presents gid %d; want %d", point, g, mountReaderGID)
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
	// Nothing is made inside the reader group's own folder.
	if entries, err := os.ReadDir(filepath.Join(groups, "sim-ro")); err != nil || len(entries) != 0 {
		t.Errorf("the reader group's folder holds %v (%v); want nothing", entries, err)
	}
	// Converging again changes nothing.
	if err := fs.EnsureReaderViews(l, "sim", mountTeamGID, reader); err != nil {
		t.Fatalf("again: %v", err)
	}
	if got := readViews(); !reflect.DeepEqual(got, []string{"sim-ro"}) {
		t.Fatalf("views after converging again = %v", got)
	}

	// 2. Revoke: the view and its mountpoint go.
	if err := fs.EnsureReaderViews(l, "sim", mountTeamGID, nil); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := readViews(); len(got) != 0 {
		t.Fatalf("after revocation views = %v", got)
	}
	if _, err := os.Stat(point); !os.IsNotExist(err) {
		t.Errorf("the mountpoint %s is still there (%v)", point, err)
	}

	// 3. A mountpoint that outlived its view (its removal failed) is drift, and
	// converging removes it.
	if err := os.Mkdir(point, viewMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(point, viewMode); err != nil {
		t.Fatal(err)
	}
	if got := readViews(); !reflect.DeepEqual(got, []string{"sim-ro@leftover"}) {
		t.Fatalf("with a leftover mountpoint = %v; want [sim-ro@leftover]", got)
	}
	if err := fs.EnsureReaderViews(l, "sim", mountTeamGID, nil); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(point); !os.IsNotExist(err) {
		t.Errorf("the leftover mountpoint %s is still there (%v)", point, err)
	}
}

// The constraint #13 started from: when views lived in the reader group's own
// folder, that group could not be given readers, because --rbind of its folder
// took its id-mapped views along and those cannot be id-mapped again. In the
// view namespace the folder holds no views, so the grant works.
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
