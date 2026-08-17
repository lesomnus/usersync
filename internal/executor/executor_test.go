package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/lesomnus/usersync/internal/idrange"
	"github.com/lesomnus/usersync/internal/provider"
	"github.com/lesomnus/usersync/internal/reconcile"
	"github.com/lesomnus/usersync/internal/samba"
	"github.com/lesomnus/usersync/internal/secret"
	"github.com/lesomnus/usersync/internal/state"
)

// --- fakes: record an ordered operation log ---

type fakeProvider struct {
	log  *[]string
	scan *state.State
}

func (f fakeProvider) Scan(context.Context) (*state.State, error) { return f.scan, nil }
func (f fakeProvider) EnsureGroup(_ context.Context, g provider.GroupSpec) error {
	*f.log = append(*f.log, fmt.Sprintf("EnsureGroup(%s,%d)", g.Name, g.GID))
	return nil
}
func (f fakeProvider) EnsureUser(_ context.Context, u provider.UserSpec) error {
	*f.log = append(*f.log, fmt.Sprintf("EnsureUser(%s,uid=%d,gid=%d,home=%s,shell=%s)", u.Name, u.UID, u.GID, u.Home, u.Shell))
	return nil
}
func (f fakeProvider) SetSupplementaryGroups(_ context.Context, user string, groups []string) error {
	*f.log = append(*f.log, fmt.Sprintf("SetGroups(%s,[%s])", user, strings.Join(groups, ",")))
	return nil
}
func (f fakeProvider) LockPassword(_ context.Context, user string) error {
	*f.log = append(*f.log, "Lock("+user+")")
	return nil
}

// Lookup{User,Group} answer from the same canned scan. The executor never calls
// them — `audit` does — but the interface requires them.
func (f fakeProvider) LookupUser(_ context.Context, name string) (uint32, bool, error) {
	if f.scan == nil {
		return 0, false, nil
	}
	u, ok := f.scan.Users[name]
	return u.UID, ok, nil
}
func (f fakeProvider) LookupGroup(_ context.Context, name string) (uint32, bool, error) {
	if f.scan == nil {
		return 0, false, nil
	}
	g, ok := f.scan.Groups[name]
	return g.GID, ok, nil
}

// RemoveAccount is never reached through the executor — `detach` calls the
// provider directly, and no reconcile Action maps to it. It is here to satisfy
// the interface, and it logs so that a future action that DID route through the
// executor would show up in these ordered-operation assertions rather than
// passing silently.
func (f fakeProvider) RemoveAccount(_ context.Context, user string, _ provider.RemoveOpts) error {
	*f.log = append(*f.log, "RemoveAccount("+user+")")
	return nil
}

type fakeSamba struct {
	log   *[]string
	accts map[string]samba.Account
}

func (f fakeSamba) Accounts(context.Context) (map[string]samba.Account, error) { return f.accts, nil }
func (f fakeSamba) Create(_ context.Context, user, pw string) error {
	*f.log = append(*f.log, fmt.Sprintf("SmbCreate(%s,pw=%s)", user, pw))
	return nil
}
func (f fakeSamba) Enable(_ context.Context, user string) error {
	*f.log = append(*f.log, "SmbEnable("+user+")")
	return nil
}
func (f fakeSamba) Disable(_ context.Context, user string) error {
	*f.log = append(*f.log, "SmbDisable("+user+")")
	return nil
}
func (f fakeSamba) Delete(_ context.Context, user string) error {
	*f.log = append(*f.log, "SmbDelete("+user+")")
	return nil
}

type fakeFS struct{ log *[]string }

func (f fakeFS) EnsureGroupDir(path string, gid, perm uint32) error {
	*f.log = append(*f.log, fmt.Sprintf("GroupDir(%s,%d)", path, gid))
	return nil
}
func (f fakeFS) EnsureHomeDir(path string, uid, gid uint32) error {
	*f.log = append(*f.log, fmt.Sprintf("HomeDir(%s,%d,%d)", path, uid, gid))
	return nil
}
func (f fakeFS) Stat(string) (bool, uint32, uint32, uint32) { return true, 0o700, 0, 0 }
func (f fakeFS) ReadReaderGIDs(string) ([]uint32, error)    { return nil, nil }
func (f fakeFS) EnsureReaderACL(path string, writerGID uint32, readerGIDs []uint32) error {
	*f.log = append(*f.log, fmt.Sprintf("ReaderACL(%s,%d,%v)", path, writerGID, readerGIDs))
	return nil
}

func newDeps(log *[]string) Deps {
	return Deps{
		Provider:   fakeProvider{log: log},
		Samba:      fakeSamba{log: log},
		Deriver:    secret.New([]byte("test-seed")),
		FS:         fakeFS{log: log},
		HomeBase:   "/research/home",
		GroupsBase: "/research/groups",
	}
}

func run(t *testing.T, actions ...reconcile.Action) []string {
	t.Helper()
	var log []string
	d := newDeps(&log)
	if _, err := d.Apply(context.Background(), actions); err != nil {
		t.Fatal(err)
	}
	return log
}

func TestCreateUserSequence(t *testing.T) {
	log := run(t, reconcile.Action{Kind: reconcile.CreateUser, Name: "skim", UID: 3001, FullName: "Sunghyun Kim", Groups: []string{"team-a"}, Home: true})
	want := []string{
		"EnsureUser(skim,uid=3001,gid=3001,home=/research/home/skim,shell=/usr/sbin/nologin)",
		"HomeDir(/research/home/skim,3001,3001)",
		"SetGroups(skim,[team-a])",
		"Lock(skim)",
		"SmbCreate(skim,pw=" + secret.New([]byte("test-seed")).InitPW("skim") + ")",
		"SmbEnable(skim)",
	}
	if strings.Join(log, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sequence mismatch:\n got %v\nwant %v", log, want)
	}
}

// A `home: false` user (Home unset on the action) is created with an account and
// SMB but NO home directory.
func TestCreateUserWithoutHome(t *testing.T) {
	log := run(t, reconcile.Action{Kind: reconcile.CreateUser, Name: "intern", UID: 3067, HasSmb: false})
	joined := strings.Join(log, " ")
	if strings.Contains(joined, "HomeDir(") {
		t.Fatalf("home: false user must not create a home directory: %v", log)
	}
	if !strings.Contains(joined, "EnsureUser(intern") || !strings.Contains(joined, "SmbEnable(intern)") {
		t.Fatalf("home: false user still gets an account and SMB: %v", log)
	}
}

func TestCreateUserDisabledEndsDisabled(t *testing.T) {
	log := run(t, reconcile.Action{Kind: reconcile.CreateUserDisabled, Name: "park", UID: 3004})
	if last := log[len(log)-1]; last != "SmbDisable(park)" {
		t.Fatalf("disabled create must end with SmbDisable, got %q", last)
	}
	joined := strings.Join(log, " ")
	if !strings.Contains(joined, "SmbCreate(park") || strings.Contains(joined, "SmbEnable") {
		t.Fatalf("disabled create must SmbCreate but not SmbEnable: %v", log)
	}
}

func TestCreateGroupSequence(t *testing.T) {
	log := run(t, reconcile.Action{Kind: reconcile.CreateGroup, Name: "team-a", GID: 7001})
	want := []string{"EnsureGroup(team-a,7001)", "GroupDir(/research/groups/team-a,7001)"}
	if strings.Join(log, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %v, want %v", log, want)
	}
}

func TestSimpleActions(t *testing.T) {
	cases := []struct {
		a    reconcile.Action
		want string
	}{
		{reconcile.Action{Kind: reconcile.UpdateUserGroups, Name: "jlee", Groups: []string{"team-a", "team-b"}}, "SetGroups(jlee,[team-a,team-b])"},
		{reconcile.Action{Kind: reconcile.EnableUser, Name: "skim"}, "SmbEnable(skim)"},
		{reconcile.Action{Kind: reconcile.DisableUser, Name: "park"}, "SmbDisable(park)"},
	}
	for _, tc := range cases {
		log := run(t, tc.a)
		if len(log) != 1 || log[0] != tc.want {
			t.Errorf("%v => %v, want [%s]", tc.a.Kind, log, tc.want)
		}
	}
}

func TestRefuseAndOrphanGroupAreNoops(t *testing.T) {
	log := run(t,
		reconcile.Action{Kind: reconcile.RefuseUser, Name: "x"},
		reconcile.Action{Kind: reconcile.RefuseGroup, Name: "y"},
		reconcile.Action{Kind: reconcile.OrphanGroup, Name: "z"},
	)
	if len(log) != 0 {
		t.Fatalf("refuse/orphan-group must be no-ops, got %v", log)
	}
}

func TestAddSmbCreatesAndEnables(t *testing.T) {
	log := run(t, reconcile.Action{Kind: reconcile.AddSmb, Name: "skim", UID: 3001})
	if len(log) != 2 || !strings.HasPrefix(log[0], "SmbCreate(skim") || log[1] != "SmbEnable(skim)" {
		t.Fatalf("AddSmb => SmbCreate+SmbEnable, got %v", log)
	}
}

func TestEnsureHomeAction(t *testing.T) {
	log := run(t, reconcile.Action{Kind: reconcile.EnsureHome, Name: "skim", UID: 3001})
	if len(log) != 1 || log[0] != "HomeDir(/research/home/skim,3001,3001)" {
		t.Fatalf("EnsureHome => HomeDir, got %v", log)
	}
}

func TestCreateUserPreservesExistingSmbPassword(t *testing.T) {
	// HasSmb: the SMB account already exists, so its password must NOT be reset.
	log := run(t, reconcile.Action{Kind: reconcile.CreateUser, Name: "skim", UID: 3001, HasSmb: true})
	if strings.Contains(strings.Join(log, " "), "SmbCreate") {
		t.Fatalf("must not SmbCreate when HasSmb set: %v", log)
	}
	if last := log[len(log)-1]; last != "SmbEnable(skim)" {
		t.Fatalf("should still enable: %v", log)
	}
}

func TestNoSeedFailsOnCreate(t *testing.T) {
	var log []string
	d := newDeps(&log)
	d.Deriver = nil
	_, err := d.Apply(context.Background(), []reconcile.Action{{Kind: reconcile.CreateUser, Name: "skim", UID: 3001}})
	if err == nil {
		t.Fatal("create without seed must error")
	}
}

func TestCollectFiltersToManaged(t *testing.T) {
	cls := idrange.New(idrange.Config{
		SystemFloor: 1000,
		UID:         idrange.Set{Manage: idrange.Range{Min: 3000, Max: 6999}},
		GID:         idrange.Set{Manage: idrange.Range{Min: 7000, Max: 7999}},
	})
	raw := state.New()
	raw.Users["root"] = state.User{Name: "root", UID: 0}
	raw.Users["skim"] = state.User{Name: "skim", UID: 3001}
	raw.Users["legacy"] = state.User{Name: "legacy", UID: 2000}
	raw.Groups["team-a"] = state.Group{Name: "team-a", GID: 7001}
	raw.Groups["docker"] = state.Group{Name: "docker", GID: 999}

	var log []string
	d := Deps{
		Provider: fakeProvider{log: &log, scan: raw},
		Samba:    fakeSamba{log: &log, accts: map[string]samba.Account{"skim": {Name: "skim", Enabled: true}}},
	}
	got, err := Collect(context.Background(), d.Provider, d.Samba, cls, CollectOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Users["skim"]; !ok {
		t.Error("skim (managed) should be kept")
	}
	if _, ok := got.Users["root"]; ok {
		t.Error("root (protected) must be filtered out")
	}
	if _, ok := got.Users["legacy"]; ok {
		t.Error("legacy (out of scope) must be filtered out")
	}
	if _, ok := got.Groups["team-a"]; !ok {
		t.Error("team-a (managed) should be kept")
	}
	if _, ok := got.Groups["docker"]; ok {
		t.Error("docker (protected) must be filtered out")
	}
	if !got.Smb["skim"].Enabled {
		t.Error("smb account should be merged in")
	}
}

func (f fakeProvider) SetGroupAdmins(_ context.Context, group string, admins []string) error {
	*f.log = append(*f.log, fmt.Sprintf("SetGroupAdmins(%s,%v)", group, admins))
	return nil
}
