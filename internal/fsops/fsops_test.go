package fsops

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

// The round trip — setfacl then getfacl — only where the tools and an
// ACL-capable filesystem are present. On this filesystem or CI without acl
// tools it is skipped rather than failed: the parser test above is what runs
// everywhere; this is the belt that catches a wrong setfacl invocation.
func TestEnsureAndReadReaderACL(t *testing.T) {
	if _, err := exec.LookPath("setfacl"); err != nil {
		t.Skip("setfacl not installed")
	}
	dir := t.TempDir()
	fs := OS{}

	if err := fs.EnsureReaderACL(dir, 0, []uint32{10011, 10012}); err != nil {
		if err == ErrACLUnsupported {
			t.Skip("filesystem has no ACL support")
		}
		t.Fatalf("EnsureReaderACL: %v", err)
	}
	gids, err := fs.ReadReaderGIDs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gids, []uint32{10011, 10012}) {
		t.Fatalf("readers = %v; want [10011 10012]", gids)
	}

	// Removing a reader must actually remove the entry — a patch of additions
	// never would.
	if err := fs.EnsureReaderACL(dir, 0, []uint32{10011}); err != nil {
		t.Fatal(err)
	}
	gids, _ = fs.ReadReaderGIDs(dir)
	if !reflect.DeepEqual(gids, []uint32{10011}) {
		t.Fatalf("after narrowing, readers = %v; want [10011]", gids)
	}

	// And clearing to none leaves the base ACL only.
	if err := fs.EnsureReaderACL(dir, 0, nil); err != nil {
		t.Fatal(err)
	}
	if gids, _ := fs.ReadReaderGIDs(dir); len(gids) != 0 {
		t.Fatalf("after clearing, readers = %v; want none", gids)
	}
}

// A reader granted on a folder that ALREADY has files must be able to read those
// files — the grant is retroactive, not only forward. A file created before the
// reader existed gains the access entry, and a nested directory gains the default
// entry so its future files inherit it too.
func TestEnsureReaderACLIsRetroactive(t *testing.T) {
	if _, err := exec.LookPath("setfacl"); err != nil {
		t.Skip("setfacl not installed")
	}
	dir := t.TempDir()
	pre := filepath.Join(dir, "old.txt")
	if err := os.WriteFile(pre, []byte("x"), 0o660); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o2770); err != nil {
		t.Fatal(err)
	}

	fs := OS{}
	if err := fs.EnsureReaderACL(dir, 0, []uint32{10011}); err != nil {
		if err == ErrACLUnsupported {
			t.Skip("filesystem has no ACL support")
		}
		t.Fatalf("EnsureReaderACL: %v", err)
	}

	// The pre-existing file gained the reader's ACCESS entry (readable now).
	if out, err := exec.Command("getfacl", "-pnE", pre).Output(); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(out), "group:10011:") {
		t.Fatalf("pre-existing file did not gain the reader access ACL:\n%s", out)
	}
	// The pre-existing subdirectory gained the reader's DEFAULT entry (its future
	// files will inherit the grant).
	if out, err := exec.Command("getfacl", "-pnE", sub).Output(); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(out), "default:group:10011:") {
		t.Fatalf("pre-existing subdir did not gain the reader default ACL:\n%s", out)
	}
}
