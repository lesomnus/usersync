package reconcile

import (
	"testing"

	"github.com/lesomnus/usersync/internal/idrange"
	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/usersync/internal/state"
)

func cls() *idrange.Classifier {
	return idrange.New(idrange.Config{
		SystemFloor: 1000,
		UID:         idrange.Set{Manage: idrange.Range{Min: 3000, Max: 6999}},
		GID:         idrange.Set{Manage: idrange.Range{Min: 7000, Max: 7999}},
	})
}

// kinds returns the action kinds in order for concise assertions.
func kinds(as []Action) []Kind {
	ks := make([]Kind, len(as))
	for i, a := range as {
		ks[i] = a.Kind
	}
	return ks
}

func hasKind(as []Action, k Kind) bool {
	for _, a := range as {
		if a.Kind == k {
			return true
		}
	}
	return false
}

func countChange(as []Action) int {
	n := 0
	for _, a := range as {
		if a.Kind.Class() == Change {
			n++
		}
	}
	return n
}

// activeState builds a state with u present (and its home already provisioned,
// unless the caller set HomeExists) plus an SMB account with the given enabled state.
func activeState(u state.User, enabled bool) *state.State {
	u.HomeExists = true
	s := state.New()
	s.Users[u.Name] = u
	s.Smb[u.Name] = state.Smb{Name: u.Name, Enabled: enabled}
	return s
}

func TestCreateUserAndGroup(t *testing.T) {
	d := &roster.Roster{
		Groups: []roster.Group{{Name: "team-a", GID: 7001}},
		Users:  []roster.User{{Name: "skim", UID: 3001, FullName: "Sunghyun Kim", Groups: []string{"team-a"}}},
	}
	got := Reconcile(d, state.New(), cls())
	if !hasKind(got, CreateGroup) || !hasKind(got, CreateUser) {
		t.Fatalf("want CreateGroup+CreateUser, got %v", kinds(got))
	}
}

func TestIdempotentNoChange(t *testing.T) {
	d := &roster.Roster{
		Groups: []roster.Group{{Name: "team-a", GID: 7001}},
		Users:  []roster.User{{Name: "skim", UID: 3001, Groups: []string{"team-a"}}},
	}
	s := state.New()
	s.Groups["team-a"] = state.Group{Name: "team-a", GID: 7001, FolderExists: true}
	s.Users["skim"] = state.User{Name: "skim", UID: 3001, GID: 3001, Groups: []string{"team-a"}, HomeExists: true}
	s.Smb["skim"] = state.Smb{Name: "skim", Enabled: true}

	got := Reconcile(d, s, cls())
	if len(got) != 0 {
		t.Fatalf("steady state must yield 0 actions, got %v", kinds(got))
	}
}

func TestUpdateGroups(t *testing.T) {
	d := &roster.Roster{
		Groups: []roster.Group{{Name: "team-a", GID: 7001}, {Name: "team-b", GID: 7002}},
		Users:  []roster.User{{Name: "jlee", UID: 3002, Groups: []string{"team-a", "team-b"}}},
	}
	s := state.New()
	s.Groups["team-a"] = state.Group{Name: "team-a", GID: 7001, FolderExists: true}
	s.Groups["team-b"] = state.Group{Name: "team-b", GID: 7002, FolderExists: true}
	s.Users["jlee"] = state.User{Name: "jlee", UID: 3002, Groups: []string{"team-a"}, HomeExists: true} // missing team-b
	s.Smb["jlee"] = state.Smb{Name: "jlee", Enabled: true}

	got := Reconcile(d, s, cls())
	if len(got) != 1 || got[0].Kind != UpdateUserGroups {
		t.Fatalf("want single UpdateUserGroups, got %v", kinds(got))
	}
}

func TestReEnableOnReappear(t *testing.T) {
	d := &roster.Roster{Users: []roster.User{{Name: "skim", UID: 3001}}}
	got := Reconcile(d, activeState(state.User{Name: "skim", UID: 3001}, false), cls())
	if len(got) != 1 || got[0].Kind != EnableUser {
		t.Fatalf("disabled SMB + active desired => EnableUser, got %v", kinds(got))
	}
}

func TestUIDMismatchRefused(t *testing.T) {
	d := &roster.Roster{Users: []roster.User{{Name: "skim", UID: 3001}}}
	got := Reconcile(d, activeState(state.User{Name: "skim", UID: 3999}, true), cls())
	if len(got) != 1 || got[0].Kind != RefuseUser {
		t.Fatalf("uid mismatch => RefuseUser, got %v", kinds(got))
	}
	if got[0].Kind.Class() != Refuse {
		t.Errorf("RefuseUser must be class Refuse")
	}
}

func TestGIDMismatchRefused(t *testing.T) {
	d := &roster.Roster{Groups: []roster.Group{{Name: "team-a", GID: 7001}}}
	s := state.New()
	s.Groups["team-a"] = state.Group{Name: "team-a", GID: 5000}
	got := Reconcile(d, s, cls())
	if len(got) != 1 || got[0].Kind != RefuseGroup {
		t.Fatalf("gid mismatch => RefuseGroup, got %v", kinds(got))
	}
}

func TestOrphanUserAutoDisable(t *testing.T) {
	d := &roster.Roster{} // roster empty; system has a managed user
	got := Reconcile(d, activeState(state.User{Name: "oldie", UID: 3009}, true), cls())
	if len(got) != 1 || got[0].Kind != OrphanUser {
		t.Fatalf("orphan enabled user => OrphanUser, got %v", kinds(got))
	}
	// Already-disabled orphan => steady state, no action (idempotent).
	got2 := Reconcile(d, activeState(state.User{Name: "oldie", UID: 3009}, false), cls())
	if len(got2) != 0 {
		t.Fatalf("already-disabled orphan => 0 actions, got %v", kinds(got2))
	}
}

func TestDisabledLifecycle(t *testing.T) {
	d := &roster.Roster{Users: []roster.User{{Name: "park", UID: 3004, Status: roster.Disabled}}}

	// absent => create locked
	if got := Reconcile(d, state.New(), cls()); len(got) != 1 || got[0].Kind != CreateUserDisabled {
		t.Fatalf("disabled+absent => CreateUserDisabled, got %v", kinds(got))
	}
	// present + enabled => disable
	if got := Reconcile(d, activeState(state.User{Name: "park", UID: 3004}, true), cls()); !hasKind(got, DisableUser) {
		t.Fatalf("disabled+enabled => DisableUser, got %v", kinds(got))
	}
	// present + already disabled => steady state
	if got := Reconcile(d, activeState(state.User{Name: "park", UID: 3004}, false), cls()); len(got) != 0 {
		t.Fatalf("disabled+disabled => 0 actions, got %v", kinds(got))
	}
}

func TestReservedLifecycle(t *testing.T) {
	d := &roster.Roster{Users: []roster.User{{Name: "oldhand", UID: 3005, Status: roster.Reserved}}}

	// absent => nothing (uid reserved via load-time uniqueness, not an action)
	if got := Reconcile(d, state.New(), cls()); len(got) != 0 {
		t.Fatalf("reserved+absent => 0 actions, got %v", kinds(got))
	}
	// present + enabled => standing notice + disable, but never delete
	got := Reconcile(d, activeState(state.User{Name: "oldhand", UID: 3005}, true), cls())
	if len(got) != 2 || got[0].Kind != ReservedPresent || got[1].Kind != DisableUser {
		t.Fatalf("reserved+enabled => ReservedPresent+DisableUser, got %v", kinds(got))
	}
	// present + disabled => STANDING notice remains (no Change action), so the
	// operator keeps being nudged that a reserved account lingers.
	got = Reconcile(d, activeState(state.User{Name: "oldhand", UID: 3005}, false), cls())
	if len(got) != 1 || got[0].Kind != ReservedPresent {
		t.Fatalf("reserved+disabled => ReservedPresent notice, got %v", kinds(got))
	}
	if got[0].Kind.Class() != Notice {
		t.Errorf("ReservedPresent must be a Notice (not a Change), so idempotency holds")
	}
	if countChange(got) != 0 {
		t.Errorf("reserved+disabled must have 0 Change actions (idempotent), got %d", countChange(got))
	}
}

func TestHomeHeal(t *testing.T) {
	// A present active user whose home directory is missing must be healed, and
	// only then (idempotent once the home exists).
	d := &roster.Roster{Users: []roster.User{{Name: "skim", UID: 3001}}}
	s := state.New()
	s.Users["skim"] = state.User{Name: "skim", UID: 3001, HomeExists: false}
	s.Smb["skim"] = state.Smb{Name: "skim", Enabled: true}
	got := Reconcile(d, s, cls())
	if !hasKind(got, EnsureHome) {
		t.Fatalf("missing home => EnsureHome, got %v", kinds(got))
	}
	// once present, no EnsureHome.
	s.Users["skim"] = state.User{Name: "skim", UID: 3001, HomeExists: true}
	if got := Reconcile(d, s, cls()); hasKind(got, EnsureHome) {
		t.Fatalf("present home => no EnsureHome, got %v", kinds(got))
	}
}

func TestGroupFolderHeal(t *testing.T) {
	d := &roster.Roster{Groups: []roster.Group{{Name: "team-a", GID: 7001}}}
	s := state.New()
	s.Groups["team-a"] = state.Group{Name: "team-a", GID: 7001, FolderExists: false}
	got := Reconcile(d, s, cls())
	if len(got) != 1 || got[0].Kind != CreateGroup {
		t.Fatalf("present group with missing folder => CreateGroup (idempotent re-ensure), got %v", kinds(got))
	}
	// folder present => no-op.
	s.Groups["team-a"] = state.Group{Name: "team-a", GID: 7001, FolderExists: true}
	if got := Reconcile(d, s, cls()); len(got) != 0 {
		t.Fatalf("present group + folder => 0 actions, got %v", kinds(got))
	}
}

func TestCreatePreservesExistingSmb(t *testing.T) {
	// unix account absent but an SMB account lingers (e.g. manual userdel): the
	// create action must be flagged so the executor does not reset the password.
	d := &roster.Roster{Users: []roster.User{{Name: "skim", UID: 3001}}}
	s := state.New()
	s.Smb["skim"] = state.Smb{Name: "skim", Enabled: true} // SMB present, unix absent
	got := Reconcile(d, s, cls())
	if len(got) != 1 || got[0].Kind != CreateUser {
		t.Fatalf("unix absent => CreateUser, got %v", kinds(got))
	}
	if !got[0].HasSmb {
		t.Error("CreateUser must set HasSmb when an SMB account already exists (do not reset password)")
	}
}

func TestReserveBlocksReuseIsLoaderConcern(t *testing.T) {
	// Sanity: reconcile does not create a reserved user, so the uid is never
	// handed out by usersync; combined with loader uniqueness the number stays
	// reserved. Reconcile with reserved+absent must not emit CreateUser*.
	d := &roster.Roster{Users: []roster.User{{Name: "oldhand", UID: 3005, Status: roster.Reserved}}}
	if got := Reconcile(d, state.New(), cls()); countChange(got) != 0 {
		t.Fatalf("reserved must never create an account, got %v", kinds(got))
	}
}

func TestAddSmbWhenMissing(t *testing.T) {
	d := &roster.Roster{Users: []roster.User{{Name: "skim", UID: 3001}}}
	s := state.New()
	s.Users["skim"] = state.User{Name: "skim", UID: 3001, HomeExists: true} // exists, no smb account
	got := Reconcile(d, s, cls())
	if len(got) != 1 || got[0].Kind != AddSmb {
		t.Fatalf("active present without smb => AddSmb, got %v", kinds(got))
	}
}

func TestDeterministicOrder(t *testing.T) {
	d := &roster.Roster{
		Users: []roster.User{{Name: "zoe", UID: 3003}, {Name: "amy", UID: 3001}, {Name: "bob", UID: 3002}},
	}
	a := Reconcile(d, state.New(), cls())
	b := Reconcile(d, state.New(), cls())
	if len(a) != 3 {
		t.Fatalf("want 3 actions, got %d", len(a))
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			t.Fatal("reconcile output must be deterministic")
		}
	}
	if a[0].Name != "amy" || a[1].Name != "bob" || a[2].Name != "zoe" {
		t.Fatalf("want name-sorted order, got %s,%s,%s", a[0].Name, a[1].Name, a[2].Name)
	}
}
