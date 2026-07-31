//go:build integration

// Package integration runs usersync end-to-end against a REAL system: real
// shadow-utils (useradd/usermod/groupadd) and real Samba (smbpasswd/pdbedit,
// and smbd/smbclient for wire auth). It creates and deletes actual accounts, so
// it MUST run as root inside a disposable container. It is guarded behind the
// `integration` build tag (so the normal `go test ./...` never touches it) and
// self-skips when not root or when the required tools are absent.
//
// Run it inside a throwaway container, locally or in CI, with:
//
//	go test -tags integration -v ./internal/integration/
//
// scripts/verify-integration.sh (local, via the dind sidecar) and the
// `integration` job in .github/workflows/ci.yaml both do exactly that.
package integration

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lesomnus/usersync/internal/executor"
	"github.com/lesomnus/usersync/internal/fsops"
	"github.com/lesomnus/usersync/internal/idrange"
	"github.com/lesomnus/usersync/internal/provider"
	"github.com/lesomnus/usersync/internal/reconcile"
	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/samba"
	"github.com/lesomnus/usersync/internal/secret"
	"github.com/lesomnus/usersync/internal/smbconf"
	"github.com/lesomnus/usersync/internal/state"
)

const seedValue = "integration-seed"

// testProvider selects the account backend under test via USERSYNC_TEST_PROVIDER
// (shadow-utils by default; "busybox" and "pw" for the other backends), so the
// same suite runs against each backend in its own container.
func testProvider(r run.Runner) provider.Provider {
	switch os.Getenv("USERSYNC_TEST_PROVIDER") {
	case "busybox":
		return provider.NewBusybox(r)
	case "pw":
		return provider.NewPw(r)
	default:
		return provider.NewShadowUtils(r)
	}
}

func requireRootAndTools(t *testing.T, tools ...string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("integration test needs root (creates real accounts); run inside a container")
	}
	// Harness tools (used by the test itself, not the provider under test).
	base := []string{"userdel", "groupdel", "getent", "passwd", "smbpasswd", "pdbedit"}
	// Backend-specific account tools.
	switch os.Getenv("USERSYNC_TEST_PROVIDER") {
	case "busybox":
		base = append(base, "adduser", "addgroup", "delgroup")
	case "pw":
		base = append(base, "pw")
	default:
		base = append(base, "useradd", "usermod", "groupadd")
	}
	for _, tool := range append(base, tools...) {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("missing required tool %q", tool)
		}
	}
}

// del best-effort removes accounts so the tests are re-runnable and leave nothing.
func del() {
	for _, u := range []string{"skim", "park"} {
		_ = exec.Command("userdel", "-r", u).Run()
	}
	for _, g := range []string{"skim", "park", "team-a"} {
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

// stack bundles the wired backends for a given home/group root.
type stack struct {
	deps       executor.Deps
	prov       provider.Provider
	smb        samba.Samba
	cls        *idrange.Classifier
	homeBase   string
	groupsBase string
}

func setup(t *testing.T) stack {
	t.Helper()
	tmp := t.TempDir()
	homeBase := filepath.Join(tmp, "home")
	groupsBase := filepath.Join(tmp, "groups")
	for _, d := range []string{homeBase, groupsBase} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// t.TempDir() creates its dir (and per-test parent) as 0700 root; make them
	// traversable (o+x) so smbd, running as the authenticated user, can reach
	// that user's 0700 home (mirrors the 0755 /research/home in production).
	for _, d := range []string{filepath.Dir(tmp), tmp} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatalf("chmod %s: %v", d, err)
		}
	}
	runner := run.Exec{}
	prov := testProvider(runner)
	smb := samba.New(runner)
	deps := executor.Deps{
		Provider:   prov,
		Samba:      smb,
		Deriver:    secret.New([]byte(seedValue)),
		FS:         fsops.OS{},
		HomeBase:   homeBase,
		GroupsBase: groupsBase,
	}
	del()
	t.Cleanup(del)
	return stack{deps, prov, smb, classifier(), homeBase, groupsBase}
}

func (s stack) collect(t *testing.T) *state.State {
	t.Helper()
	st, err := executor.Collect(context.Background(), s.prov, s.smb, s.cls,
		executor.CollectOpts{HomeBase: s.homeBase, GroupsBase: s.groupsBase, FS: fsops.OS{}})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return st
}

func (s stack) apply(t *testing.T, ro *roster.Roster) {
	t.Helper()
	if _, err := ro.Validate(s.cls, roster.PolicyError); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if _, err := s.deps.Apply(context.Background(), reconcile.Reconcile(ro, s.collect(t), s.cls)); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

func fullRoster() *roster.Roster {
	return &roster.Roster{
		Groups: []roster.Group{{Name: "team-a", GID: 7001}},
		Users: []roster.User{
			{Name: "skim", UID: 3001, FullName: "Sunghyun Kim", Groups: []string{"team-a"}},
			{Name: "park", UID: 3004, Status: roster.Disabled},
		},
	}
}

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
	s := setup(t)

	// === create ===
	s.apply(t, fullRoster())
	st := s.collect(t)

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
	if perm, uid, gid := statOwnerMode(t, filepath.Join(s.homeBase, "skim")); perm != 0o700 || uid != 3001 || gid != 3001 {
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
	if perm, _, gid := statOwnerMode(t, filepath.Join(s.groupsBase, "team-a")); perm != 0o2770 || gid != 7001 {
		t.Errorf("team-a folder = %o gid %d, want 2770 gid 7001 (setgid)", perm, gid)
	}

	// === idempotency: re-plan yields no Change actions ===
	if n := changeCount(reconcile.Reconcile(fullRoster(), s.collect(t), s.cls)); n != 0 {
		t.Errorf("steady state must yield 0 change actions, got %d", n)
	}

	// === offboard: drop skim from the roster => SMB disabled, data kept ===
	s.apply(t, &roster.Roster{Groups: []roster.Group{{Name: "team-a", GID: 7001}}})
	if _, err := os.Stat(filepath.Join(s.homeBase, "skim")); err != nil {
		t.Error("skim home must be preserved after offboarding")
	}
	if _, err := user.Lookup("skim"); err != nil {
		t.Error("skim account must NOT be deleted on offboarding")
	}
	if s.collect(t).Smb["skim"].Enabled {
		t.Error("skim SMB must be disabled after offboarding")
	}

	// === re-onboard: add skim back => re-enabled, password preserved ===
	s.apply(t, fullRoster())
	if !s.collect(t).Smb["skim"].Enabled {
		t.Error("skim SMB must be re-enabled on re-onboarding")
	}
}

// TestSmbWireAuth verifies the actual access model over SMB: a user can reach
// their own home, cannot reach another user's home, and a wrong password is
// rejected. It brings up a real smbd against the tdbsam that apply populated,
// and authenticates with the seed-derived password (which the test recomputes).
func TestSmbWireAuth(t *testing.T) {
	requireRootAndTools(t, "smbd", "smbclient")
	s := setup(t)
	s.apply(t, fullRoster())

	pw := secret.New([]byte(seedValue)).InitPW("skim")

	// Runtime dirs smbd needs (no systemd in the container to create them).
	runDir := filepath.Join(t.TempDir(), "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll("/run/samba", 0o755)

	// Keep the DEFAULT private dir so smbd reads the same passdb.tdb that
	// `smbpasswd -a` wrote during apply; only redirect the volatile dirs.
	conf := filepath.Join(t.TempDir(), "smb.conf")
	confBody := fmt.Sprintf(`[global]
	workgroup = WORKGROUP
	security = user
	passdb backend = tdbsam
	map to guest = never
	smb ports = 445
	load printers = no
	printing = bsd
	printcap name = /dev/null
	disable spoolss = yes
	pid directory = %[1]s
	lock directory = %[1]s
	log file = %[1]s/smbd.log
[homes]
	browseable = no
	read only = no
	valid users = %%S
[team-a]
	path = %[2]s/team-a
	valid users = @team-a
	read only = no
	create mask = 0660
	directory mask = 2770
`, runDir, s.groupsBase)
	if err := os.WriteFile(conf, []byte(confBody), 0o644); err != nil {
		t.Fatal(err)
	}

	// Start smbd in the foreground; skip (not fail) if the environment can't run it.
	smbd := exec.Command("smbd", "--configfile="+conf, "--foreground", "--no-process-group")
	var logbuf bytes.Buffer
	smbd.Stdout, smbd.Stderr = &logbuf, &logbuf
	if err := smbd.Start(); err != nil {
		t.Skipf("cannot start smbd: %v", err)
	}
	t.Cleanup(func() { _ = smbd.Process.Kill(); _ = smbd.Wait() })

	ready := false
	for i := 0; i < 40; i++ {
		if c, err := net.DialTimeout("tcp", "127.0.0.1:445", 300*time.Millisecond); err == nil {
			_ = c.Close()
			ready = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !ready {
		t.Skipf("smbd did not open port 445 (env cannot run smbd):\n%s", logbuf.String())
	}

	smbc := func(share, user, pass, cmds string) (string, error) {
		out, err := exec.Command("smbclient", "//localhost/"+share,
			"-U", user+"%"+pass, "-c", cmds).CombinedOutput()
		return string(out), err
	}

	// 1) own home: read + write succeed, and the write lands on disk.
	if out, err := smbc("skim", "skim", pw, "put /etc/hostname probe.txt; ls"); err != nil {
		t.Errorf("skim must access own home over SMB: %v\n%s", err, out)
	} else if _, e := os.Stat(filepath.Join(s.homeBase, "skim", "probe.txt")); e != nil {
		t.Errorf("SMB write did not land in skim's home: %v", e)
	}

	// 2) another user's home is denied.
	if out, err := smbc("park", "skim", pw, "ls"); err == nil {
		t.Errorf("skim must NOT access park's home over SMB:\n%s", out)
	}

	// 3) wrong password is rejected (SMB auth actually enforced).
	if out, err := smbc("skim", "skim", "wrong-"+pw, "ls"); err == nil {
		t.Errorf("a wrong SMB password must be rejected:\n%s", out)
	}
}

// TestModeDriftHeal drifts the home/group-folder permissions and owner after a
// successful apply, then verifies a re-apply restores them (0700 owned by the
// UPG; 2770 setgid owned by the team group).
func TestModeDriftHeal(t *testing.T) {
	requireRootAndTools(t)
	s := setup(t)
	s.apply(t, fullRoster())

	home := filepath.Join(s.homeBase, "skim")
	folder := filepath.Join(s.groupsBase, "team-a")
	for _, op := range []func() error{
		func() error { return os.Chmod(home, 0o777) },
		func() error { return os.Chown(home, 0, 0) },
		func() error { return os.Chmod(folder, 0o770) }, // lose setgid
	} {
		if err := op(); err != nil {
			t.Fatal(err)
		}
	}

	s.apply(t, fullRoster()) // must heal the drift

	if perm, uid, gid := statOwnerMode(t, home); perm != 0o700 || uid != 3001 || gid != 3001 {
		t.Errorf("home not healed: %o %d:%d, want 700 3001:3001", perm, uid, gid)
	}
	if perm, _, gid := statOwnerMode(t, folder); perm != 0o2770 || gid != 7001 {
		t.Errorf("group folder not healed: %o gid %d, want 2770 gid 7001", perm, gid)
	}
}

// TestSmbConfGenerate generates the shares block into a base smb.conf, verifies
// real testparm accepts it, that a backup is kept, and that it is idempotent.
func TestSmbConfGenerate(t *testing.T) {
	requireRootAndTools(t, "testparm")
	s := setup(t)
	s.apply(t, fullRoster()) // so team-a exists for the share

	conf := filepath.Join(t.TempDir(), "smb.conf")
	base := "[global]\n\tworkgroup = WORKGROUP\n\tsecurity = user\n"
	if err := os.WriteFile(conf, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := smbconf.Apply(context.Background(), conf, s.groupsBase, fullRoster().Groups, run.Exec{}, false)
	if err != nil {
		t.Fatalf("smbconf apply: %v", err)
	}
	if !changed {
		t.Fatal("expected the smb.conf to change")
	}
	if _, err := os.Stat(conf + ".bak"); err != nil {
		t.Errorf("backup not written: %v", err)
	}
	b, _ := os.ReadFile(conf)
	for _, want := range []string{smbconf.BeginMarker, "[homes]", "[team-a]", "force group = team-a"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("generated smb.conf missing %q", want)
		}
	}
	if err := smbconf.Validate(context.Background(), run.Exec{}, conf); err != nil {
		t.Errorf("real testparm rejected the generated smb.conf: %v", err)
	}
	// idempotent: a second Apply is a no-op.
	if changed, err := smbconf.Apply(context.Background(), conf, s.groupsBase, fullRoster().Groups, run.Exec{}, false); err != nil {
		t.Fatal(err)
	} else if changed {
		t.Error("second smbconf apply should be a no-op")
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

// TestPreservesProtectedGroupOnUpdate proves the blocker fix end-to-end: a
// managed user's membership in a protected/out-of-scope group survives a
// supplementary-group update triggered by a roster change.
func TestPreservesProtectedGroupOnUpdate(t *testing.T) {
	requireRootAndTools(t)
	s := setup(t)
	s.apply(t, fullRoster()) // skim in team-a

	// Put skim into a protected group (gid 900) out of band.
	_ = exec.Command("groupadd", "-g", "900", "extra").Run()
	t.Cleanup(func() { _ = exec.Command("groupdel", "extra").Run() })
	if out, err := exec.Command("usermod", "-aG", "extra", "skim").CombinedOutput(); err != nil {
		t.Fatalf("usermod -aG extra skim: %v\n%s", err, out)
	}

	// Roster change (skim gains team-b) fires a supplementary-group update.
	s.apply(t, &roster.Roster{
		Groups: []roster.Group{{Name: "team-a", GID: 7001}, {Name: "team-b", GID: 7002}},
		Users: []roster.User{
			{Name: "skim", UID: 3001, Groups: []string{"team-a", "team-b"}},
			{Name: "park", UID: 3004, Status: roster.Disabled},
		},
	})

	out, err := exec.Command("id", "-nG", "skim").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"extra", "team-a", "team-b"} {
		if !slices.Contains(strings.Fields(string(out)), want) {
			t.Errorf("skim must remain in %q after the update, got %q", want, string(out))
		}
	}
}
