//go:build integration

// Package integration runs usersync end-to-end against a REAL system: real
// shadow-utils (useradd/usermod/groupadd) and real Samba (smbpasswd/pdbedit).
// It creates and deletes actual accounts, so it MUST run as root inside a
// disposable container. It is guarded behind the `integration` build tag (so the
// normal `go test ./...` never touches it) and self-skips when not root or when
// the required tools are absent.
//
// Run it inside a throwaway container, locally or in CI, with:
//
//	go test -tags integration -v ./internal/integration/
//
// scripts/verify-integration.sh (local, via the dind sidecar) and
// .github/workflows/integration.yaml (CI) both do exactly that.
package integration

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/lesomnus/usersync/internal/executor"
	"github.com/lesomnus/usersync/internal/fsops"
	"github.com/lesomnus/usersync/internal/idrange"
	"github.com/lesomnus/usersync/internal/provider"
	"github.com/lesomnus/usersync/internal/reconcile"
	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/samba"
	"github.com/lesomnus/usersync/internal/secret"
	"github.com/lesomnus/usersync/internal/state"
)

func requireRootAndTools(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("integration test needs root (creates real accounts); run inside a container")
	}
	for _, tool := range []string{"useradd", "usermod", "groupadd", "userdel", "groupdel", "getent", "passwd", "smbpasswd", "pdbedit"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("missing required tool %q (install shadow-utils + samba)", tool)
		}
	}
}

// del best-effort removes accounts so the test is re-runnable and leaves nothing.
func del(users, groups []string) {
	for _, u := range users {
		_ = exec.Command("userdel", "-r", u).Run()
	}
	for _, g := range groups {
		_ = exec.Command("groupdel", g).Run()
	}
}

func classifier() *idrange.Classifier {
	return idrange.New(idrange.Config{
		SystemFloor: 1000,
		UID:         idrange.Set{Manage: idrange.Range{Min: 3000, Max: 6999}},
		GID:         idrange.Set{Manage: idrange.Range{Min: 7000, Max: 7999}},
	})
}

// statOwnerMode returns a path's permission bits (with the setgid flag folded in
// as 0o2000) and its owning uid/gid.
func statOwnerMode(t *testing.T, path string) (perm os.FileMode, uid, gid uint32) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	perm = fi.Mode().Perm()
	if fi.Mode()&os.ModeSetgid != 0 {
		perm |= 0o2000
	}
	st := fi.Sys().(*syscall.Stat_t)
	return perm, st.Uid, st.Gid
}

func passwdLocked(t *testing.T, u string) bool {
	t.Helper()
	out, err := exec.Command("passwd", "-S", u).Output()
	if err != nil {
		t.Fatalf("passwd -S %s: %v", u, err)
	}
	f := strings.Fields(string(out))
	return len(f) >= 2 && strings.HasPrefix(f[1], "L")
}

func TestApplyEndToEnd(t *testing.T) {
	requireRootAndTools(t)
	ctx := context.Background()

	tmp := t.TempDir()
	homeBase := filepath.Join(tmp, "home")
	groupsBase := filepath.Join(tmp, "groups")
	for _, d := range []string{homeBase, groupsBase} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cls := classifier()
	runner := run.Exec{}
	prov := provider.NewShadowUtils(runner)
	smb := samba.New(runner)
	deps := executor.Deps{
		Provider:   prov,
		Samba:      smb,
		Deriver:    secret.New([]byte("integration-seed")),
		FS:         fsops.OS{},
		HomeBase:   homeBase,
		GroupsBase: groupsBase,
	}

	// Clean slate + guaranteed teardown (these ids live in the shared system).
	del([]string{"skim", "park"}, []string{"skim", "park", "team-a"})
	t.Cleanup(func() { del([]string{"skim", "park"}, []string{"skim", "park", "team-a"}) })

	full := &roster.Roster{
		Groups: []roster.Group{{Name: "team-a", GID: 7001}},
		Users: []roster.User{
			{Name: "skim", UID: 3001, FullName: "Sunghyun Kim", Groups: []string{"team-a"}},
			{Name: "park", UID: 3004, Status: roster.Disabled},
		},
	}

	apply := func(ro *roster.Roster) {
		t.Helper()
		if _, err := ro.Validate(cls, roster.PolicyError); err != nil {
			t.Fatalf("validate: %v", err)
		}
		actual, err := executor.Collect(ctx, prov, smb, cls, executor.CollectOpts{HomeBase: homeBase, GroupsBase: groupsBase, FS: fsops.OS{}})
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if err := deps.Apply(ctx, reconcile.Reconcile(ro, actual, cls)); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
	collect := func() *state.State {
		t.Helper()
		st, err := executor.Collect(ctx, prov, smb, cls, executor.CollectOpts{HomeBase: homeBase, GroupsBase: groupsBase, FS: fsops.OS{}})
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		return st
	}

	// === create ===
	apply(full)
	st := collect()

	if u, err := user.Lookup("skim"); err != nil {
		t.Fatalf("skim not created: %v", err)
	} else if u.Uid != "3001" || u.Gid != "3001" {
		t.Errorf("skim uid/gid = %s/%s, want 3001/3001 (UPG)", u.Uid, u.Gid)
	}
	if got := st.Users["skim"]; !slices.Contains(got.Groups, "team-a") {
		t.Errorf("skim supplementary groups = %v, want to contain team-a", got.Groups)
	}
	if got := st.Users["skim"].Shell; got != "/usr/sbin/nologin" {
		t.Errorf("skim shell = %q, want /usr/sbin/nologin", got)
	}
	if got := st.Users["skim"].FullName; got != "Sunghyun Kim" {
		t.Errorf("skim gecos = %q, want %q", got, "Sunghyun Kim")
	}
	if perm, uid, gid := statOwnerMode(t, filepath.Join(homeBase, "skim")); perm != 0o700 || uid != 3001 || gid != 3001 {
		t.Errorf("skim home = %o %d:%d, want 700 3001:3001", perm, uid, gid)
	}
	if !passwdLocked(t, "skim") {
		t.Error("skim unix password must be locked (SSH blocked)")
	}
	if !st.Smb["skim"].Enabled {
		t.Error("skim SMB account must be present and enabled")
	}
	if st.Smb["park"].Enabled {
		t.Error("park was created disabled; its SMB account must be disabled")
	}
	if perm, _, gid := statOwnerMode(t, filepath.Join(groupsBase, "team-a")); perm != 0o2770 || gid != 7001 {
		t.Errorf("team-a folder = %o gid %d, want 2770 gid 7001 (setgid)", perm, gid)
	}

	// === idempotency: re-plan yields no Change actions ===
	if n := changeCount(reconcile.Reconcile(full, collect(), cls)); n != 0 {
		t.Errorf("steady state must yield 0 change actions, got %d", n)
	}

	// === offboard: drop skim from the roster => SMB disabled, data kept ===
	apply(&roster.Roster{Groups: []roster.Group{{Name: "team-a", GID: 7001}}})
	if _, err := os.Stat(filepath.Join(homeBase, "skim")); err != nil {
		t.Error("skim home must be preserved after offboarding")
	}
	if _, err := user.Lookup("skim"); err != nil {
		t.Error("skim account must NOT be deleted on offboarding")
	}
	if collect().Smb["skim"].Enabled {
		t.Error("skim SMB must be disabled after offboarding")
	}

	// === re-onboard: add skim back => re-enabled, password preserved ===
	apply(full)
	if !collect().Smb["skim"].Enabled {
		t.Error("skim SMB must be re-enabled on re-onboarding")
	}
}

func changeCount(as []reconcile.Action) int {
	n := 0
	for _, a := range as {
		if a.Kind.Class() == reconcile.Change {
			n++
		}
	}
	return n
}
