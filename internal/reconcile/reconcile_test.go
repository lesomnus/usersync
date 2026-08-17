package reconcile

import (
	"slices"
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

// activeState builds a state with u present (home already provisioned correctly:
// 0700 owned by its UPG) plus an SMB account with the given enabled state.
func activeState(u state.User, enabled bool) *state.State {
	u.HomeExists = true
	u.HomePerm = 0o700
	u.HomeUID = u.UID
	u.HomeGID = u.UID
	s := state.New()
	s.Users[u.Name] = u
	s.Smb[u.Name] = state.Smb{Name: u.Name, Enabled: enabled}
	return s
}

// okHome / okFolder mark a manually-built entry as correctly provisioned so it
// is steady (no drift heal).
func okHome(u state.User) state.User {
	u.HomeExists, u.HomePerm, u.HomeUID, u.HomeGID = true, 0o700, u.UID, u.UID
	return u
}
func okFolder(g state.Group) state.Group {
	g.FolderExists, g.FolderPerm, g.FolderGID = true, 0o2770, g.GID
	return g
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
	s.Groups["team-a"] = okFolder(state.Group{Name: "team-a", GID: 7001})
	s.Users["skim"] = okHome(state.User{Name: "skim", UID: 3001, GID: 3001, Groups: []string{"team-a"}})
	s.Smb["skim"] = state.Smb{Name: "skim", Enabled: true}

	got := Reconcile(d, s, cls())
	if len(got) != 0 {
		t.Fatalf("steady state must yield 0 actions, got %v", kinds(got))
	}
}

// A group declared `all: true` puts every ACTIVE user in it without the roster
// listing the membership per user — a reserved account is left out.
func TestAllGroupMembership(t *testing.T) {
	d := &roster.Roster{
		Groups: []roster.Group{
			{Name: "everyone", GID: 7001, All: true},
			{Name: "team-a", GID: 7002},
		},
		Users: []roster.User{
			{Name: "jlee", UID: 3001, Groups: []string{"team-a"}},
			{Name: "gone", UID: 3002, Status: roster.Reserved},
		},
	}
	s := state.New()
	s.Groups["everyone"] = okFolder(state.Group{Name: "everyone", GID: 7001})
	s.Groups["team-a"] = okFolder(state.Group{Name: "team-a", GID: 7002})
	s.Users["jlee"] = okHome(state.User{Name: "jlee", UID: 3001, Groups: []string{"team-a"}}) // missing everyone
	s.Smb["jlee"] = state.Smb{Name: "jlee", Enabled: true}

	got := Reconcile(d, s, cls())

	var jlee *Action
	for i := range got {
		if got[i].Kind == UpdateUserGroups && got[i].Name == "jlee" {
			jlee = &got[i]
		}
		if got[i].Name == "gone" && got[i].Kind == UpdateUserGroups && slices.Contains(got[i].Groups, "everyone") {
			t.Error("a reserved user was pulled into the all group")
		}
	}
	if jlee == nil {
		t.Fatalf("want an UpdateUserGroups for jlee, got %v", kinds(got))
	}
	if !slices.Contains(jlee.Groups, "everyone") || !slices.Contains(jlee.Groups, "team-a") {
		t.Fatalf("jlee groups = %v, want to include both everyone and team-a", jlee.Groups)
	}
}

func TestUpdateGroups(t *testing.T) {
	d := &roster.Roster{
		Groups: []roster.Group{{Name: "team-a", GID: 7001}, {Name: "team-b", GID: 7002}},
		Users:  []roster.User{{Name: "jlee", UID: 3002, Groups: []string{"team-a", "team-b"}}},
	}
	s := state.New()
	s.Groups["team-a"] = okFolder(state.Group{Name: "team-a", GID: 7001})
	s.Groups["team-b"] = okFolder(state.Group{Name: "team-b", GID: 7002})
	s.Users["jlee"] = okHome(state.User{Name: "jlee", UID: 3002, Groups: []string{"team-a"}}) // missing team-b
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

func TestOrphanUserSurfacedAndDisabled(t *testing.T) {
	d := &roster.Roster{} // roster empty; system has a managed user
	// enabled orphan => standing Notice + a disable
	got := Reconcile(d, activeState(state.User{Name: "oldie", UID: 3009}, true), cls())
	if len(got) != 2 || got[0].Kind != OrphanUser || got[1].Kind != DisableUser {
		t.Fatalf("orphan enabled => OrphanUser(notice)+DisableUser, got %v", kinds(got))
	}
	// Already-disabled orphan => the Notice remains (a hand-created account must
	// keep being surfaced), but there are 0 Change actions so apply is idempotent.
	got2 := Reconcile(d, activeState(state.User{Name: "oldie", UID: 3009}, false), cls())
	if len(got2) != 1 || got2[0].Kind != OrphanUser {
		t.Fatalf("disabled orphan => standing OrphanUser notice, got %v", kinds(got2))
	}
	if got2[0].Kind.Class() != Notice || countChange(got2) != 0 {
		t.Errorf("disabled orphan must be a Notice with 0 Change actions, got %d change", countChange(got2))
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
	s.Users["skim"] = okHome(state.User{Name: "skim", UID: 3001})
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
	s.Groups["team-a"] = okFolder(state.Group{Name: "team-a", GID: 7001})
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
	s.Users["skim"] = okHome(state.User{Name: "skim", UID: 3001}) // exists, no smb account
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

func TestHomePermDrift(t *testing.T) {
	// Home exists but drifted (0755, owned by root) => must be re-ensured; once
	// correct (0700 owned by the UPG) => no action (idempotent).
	d := &roster.Roster{Users: []roster.User{{Name: "skim", UID: 3001}}}
	s := state.New()
	s.Users["skim"] = state.User{Name: "skim", UID: 3001, HomeExists: true, HomePerm: 0o755, HomeUID: 0, HomeGID: 0}
	s.Smb["skim"] = state.Smb{Name: "skim", Enabled: true}
	if got := Reconcile(d, s, cls()); !hasKind(got, EnsureHome) {
		t.Fatalf("home perm/owner drift => EnsureHome, got %v", kinds(got))
	}
	s.Users["skim"] = okHome(state.User{Name: "skim", UID: 3001})
	if got := Reconcile(d, s, cls()); hasKind(got, EnsureHome) {
		t.Fatalf("correct home => no EnsureHome, got %v", kinds(got))
	}
}

func TestGroupFolderPermDrift(t *testing.T) {
	// Folder exists but lost its setgid bit (0770) => must be re-ensured.
	d := &roster.Roster{Groups: []roster.Group{{Name: "team-a", GID: 7001}}}
	s := state.New()
	s.Groups["team-a"] = state.Group{Name: "team-a", GID: 7001, FolderExists: true, FolderPerm: 0o770, FolderGID: 7001}
	if got := Reconcile(d, s, cls()); len(got) != 1 || got[0].Kind != CreateGroup {
		t.Fatalf("folder perm drift => CreateGroup heal, got %v", kinds(got))
	}
}

func TestAnonymousFolderMode(t *testing.T) {
	// An anonymous:read group wants 2775. A folder still at the private 2770 is
	// drift and must heal to CreateGroup carrying the wider mode; a folder already
	// at 2775 is a no-op.
	d := &roster.Roster{Groups: []roster.Group{{Name: "pub", GID: 7001, Anonymous: roster.AnonRead}}}

	s := state.New()
	s.Groups["pub"] = state.Group{Name: "pub", GID: 7001, FolderExists: true, FolderPerm: 0o2770, FolderGID: 7001}
	got := Reconcile(d, s, cls())
	if len(got) != 1 || got[0].Kind != CreateGroup {
		t.Fatalf("anonymous level raises desired mode => CreateGroup heal, got %v", kinds(got))
	}
	if got[0].DirPerm != 0o2775 {
		t.Fatalf("CreateGroup must carry the anonymous mode 2775, got %04o", got[0].DirPerm)
	}

	s.Groups["pub"] = state.Group{Name: "pub", GID: 7001, FolderExists: true, FolderPerm: 0o2775, FolderGID: 7001}
	if got := Reconcile(d, s, cls()); len(got) != 0 {
		t.Fatalf("folder already at 2775 => no-op, got %v", kinds(got))
	}
}

func TestPreservesNonManagedGroups(t *testing.T) {
	// skim is in team-a (managed) and docker (non-managed, preserved). A managed
	// group change must NOT strip docker.
	d := &roster.Roster{
		Groups: []roster.Group{{Name: "team-a", GID: 7001}, {Name: "team-b", GID: 7002}},
		Users:  []roster.User{{Name: "skim", UID: 3001, Groups: []string{"team-a", "team-b"}}},
	}
	s := state.New()
	s.Groups["team-a"] = okFolder(state.Group{Name: "team-a", GID: 7001})
	s.Groups["team-b"] = okFolder(state.Group{Name: "team-b", GID: 7002})
	u := okHome(state.User{Name: "skim", UID: 3001, Groups: []string{"team-a"}}) // managed
	u.ExtraGroups = []string{"docker"}                                           // non-managed, must survive
	s.Users["skim"] = u
	s.Smb["skim"] = state.Smb{Name: "skim", Enabled: true}

	got := Reconcile(d, s, cls())
	if len(got) != 1 || got[0].Kind != UpdateUserGroups {
		t.Fatalf("want UpdateUserGroups, got %v", kinds(got))
	}
	set := map[string]bool{}
	for _, g := range got[0].Groups {
		set[g] = true
	}
	if !set["docker"] || !set["team-a"] || !set["team-b"] {
		t.Errorf("update must preserve docker and include team-a+team-b, got %v", got[0].Groups)
	}
}

func TestCrossNameUIDCollisionRefused(t *testing.T) {
	// newbie wants uid 3001, but oldie already holds it.
	d := &roster.Roster{Users: []roster.User{{Name: "newbie", UID: 3001}}}
	s := state.New()
	s.Users["oldie"] = okHome(state.User{Name: "oldie", UID: 3001})
	got := Reconcile(d, s, cls())
	// newbie -> RefuseUser; oldie -> orphan notice
	if !hasKind(got, RefuseUser) {
		t.Fatalf("uid held by another name => RefuseUser, got %v", kinds(got))
	}
}

func TestRefuseCreateWhenNameExistsOutOfRange(t *testing.T) {
	// A pre-existing account named "alice" at an out-of-range uid (hidden from
	// managed Users) must block a create — never mutate someone else's account.
	d := &roster.Roster{Users: []roster.User{{Name: "alice", UID: 3001}}}
	s := state.New()
	s.AllUsers["alice"] = 500 // system account, out of managed range
	got := Reconcile(d, s, cls())
	if len(got) != 1 || got[0].Kind != RefuseUser {
		t.Fatalf("create colliding with out-of-range account => RefuseUser, got %v", kinds(got))
	}
}

func TestRefuseCreateOnUPGGroupMismatch(t *testing.T) {
	d := &roster.Roster{Users: []roster.User{{Name: "alice", UID: 3001}}}
	s := state.New()
	s.AllGroups["alice"] = 500 // group named alice at gid != uid (UPG needs 3001)
	got := Reconcile(d, s, cls())
	if len(got) != 1 || got[0].Kind != RefuseUser {
		t.Fatalf("UPG name/gid mismatch => RefuseUser, got %v", kinds(got))
	}
}

func TestOrphanInvalidNameSurfacedNotExeced(t *testing.T) {
	// A scanned account with an unsafe name must still be SURFACED (report-only
	// Notice) but must never reach an exec argument (no DisableUser).
	d := &roster.Roster{}
	s := activeState(state.User{Name: "-x", UID: 3009}, true)
	got := Reconcile(d, s, cls())
	if hasKind(got, DisableUser) {
		t.Fatalf("invalid scanned name must NOT be exec'd, got %v", kinds(got))
	}
	if !hasKind(got, OrphanUser) {
		t.Fatalf("invalid-named orphan must still be surfaced as a Notice, got %v", kinds(got))
	}
}

// Owners drift is its own action: a group can be entirely correct and still
// have the wrong administrators.
func TestSetGroupAdminsOnDrift(t *testing.T) {
	ro := &roster.Roster{
		Groups: []roster.Group{{Name: "team-a", GID: 7001, Owners: []string{"alice"}}},
		Users:  []roster.User{{Name: "alice", UID: 3001, Groups: []string{"team-a"}}},
	}
	base := func(admins []string, known bool) *state.State {
		st := state.New()
		st.Groups["team-a"] = state.Group{
			Name: "team-a", GID: 7001,
			FolderExists: true, FolderPerm: 0o2770, FolderGID: 7001,
			Admins: admins, AdminsKnown: known,
		}
		st.AllGroups["team-a"] = 7001
		return st
	}

	t.Run("declared owner missing", func(t *testing.T) {
		got := kindsFor(t, ro, base(nil, true), SetGroupAdmins)
		if len(got) != 1 || got[0].Name != "team-a" {
			t.Fatalf("actions = %v, want one set-group-admins for team-a", got)
		}
		if len(got[0].Groups) != 1 || got[0].Groups[0] != "alice" {
			t.Errorf("admins = %v, want [alice]", got[0].Groups)
		}
	})

	t.Run("already correct is a no-op", func(t *testing.T) {
		if got := kindsFor(t, ro, base([]string{"alice"}, true), SetGroupAdmins); len(got) != 0 {
			t.Errorf("actions = %v, want none", got)
		}
	})

	// Order is not drift. gpasswd writes them sorted but a hand edit need not,
	// and re-running on a reordering would make `plan` permanently dirty.
	t.Run("order does not matter", func(t *testing.T) {
		ro2 := &roster.Roster{
			Groups: []roster.Group{{Name: "team-a", GID: 7001, Owners: []string{"bob", "alice"}}},
			Users:  []roster.User{{Name: "alice", UID: 3001}, {Name: "bob", UID: 3002}},
		}
		if got := kindsFor(t, ro2, base([]string{"alice", "bob"}, true), SetGroupAdmins); len(got) != 0 {
			t.Errorf("actions = %v, want none", got)
		}
	})

	// A backend with no gshadow cannot tell. Proposing the change anyway would
	// mean every run forever reports the same drift it can never fix.
	t.Run("unknown is not drift", func(t *testing.T) {
		if got := kindsFor(t, ro, base(nil, false), SetGroupAdmins); len(got) != 0 {
			t.Errorf("actions = %v, want none when the backend has no gshadow", got)
		}
	})
}

// kindsFor reconciles and returns the actions of one kind.
func kindsFor(t *testing.T, ro *roster.Roster, st *state.State, kind Kind) []Action {
	t.Helper()
	var out []Action
	for _, a := range Reconcile(ro, st, cls()) {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

// A group being created has no administrators yet, so declared owners must be
// applied then too -- otherwise a new team's owner is set only if someone later
// notices the drift.
func TestSetGroupAdminsOnCreate(t *testing.T) {
	ro := &roster.Roster{
		Groups: []roster.Group{{Name: "team-a", GID: 7001, Owners: []string{"alice"}}},
		Users:  []roster.User{{Name: "alice", UID: 3001}},
	}
	st := state.New() // nothing exists

	acts := Reconcile(ro, st, cls())

	at := map[Kind]int{}
	for i, a := range acts {
		at[a.Kind] = i
	}
	for _, k := range []Kind{CreateGroup, CreateUser, SetGroupAdmins} {
		if _, ok := at[k]; !ok {
			t.Fatalf("kinds = %v, want %v present", kinds(acts), k)
		}
	}
	// Order is load-bearing twice over: `gpasswd -A alice team-a` needs the
	// GROUP to exist and it needs the USER alice to exist. Emitting it in the
	// group loop fails on a fresh system with "user does not exist", and the
	// delegation then lands only if somebody runs apply again.
	if at[SetGroupAdmins] < at[CreateGroup] {
		t.Errorf("set-group-admins at %d precedes create-group at %d", at[SetGroupAdmins], at[CreateGroup])
	}
	if at[SetGroupAdmins] < at[CreateUser] {
		t.Errorf("set-group-admins at %d precedes create-user at %d", at[SetGroupAdmins], at[CreateUser])
	}
}

// okReaders marks a folder's reader ACL as known and matching gids, so a group
// whose readers already agree is steady.
func okReaders(g state.Group, gids ...uint32) state.Group {
	g.ReadersKnown = true
	g.ReaderGIDs = gids
	return g
}

// A team with a declared reader group emits a SetGroupReaders carrying the
// reader's numeric gid — resolved from the roster, so it does not depend on the
// reader group existing on the system yet.
func TestReadersEmitSetGroupReaders(t *testing.T) {
	d := &roster.Roster{
		Groups: []roster.Group{
			{Name: "perception", GID: 7001, Readers: []string{"perception-ro"}},
			{Name: "perception-ro", GID: 7011},
		},
	}
	got := Reconcile(d, state.New(), cls())
	if !hasKind(got, SetGroupReaders) {
		t.Fatalf("want SetGroupReaders, got %v", kinds(got))
	}
	for _, a := range got {
		if a.Kind == SetGroupReaders && a.Name == "perception" {
			if len(a.ReaderGIDs) != 1 || a.ReaderGIDs[0] != 7011 {
				t.Errorf("ReaderGIDs = %v; want [7011]", a.ReaderGIDs)
			}
		}
	}
	// The SetGroupReaders for perception must come AFTER its CreateGroup, so the
	// folder exists when the ACL is applied.
	var iCreate, iReaders = -1, -1
	for i, a := range got {
		if a.Name == "perception" && a.Kind == CreateGroup {
			iCreate = i
		}
		if a.Name == "perception" && a.Kind == SetGroupReaders {
			iReaders = i
		}
	}
	if iCreate < 0 || iReaders < 0 || iCreate > iReaders {
		t.Errorf("SetGroupReaders(%d) must follow CreateGroup(%d)", iReaders, iCreate)
	}
}

// When the folder's ACL already matches the declared readers, nothing is
// proposed — the feature is idempotent.
func TestReadersSteadyWhenACLMatches(t *testing.T) {
	d := &roster.Roster{
		Groups: []roster.Group{
			{Name: "perception", GID: 7001, Readers: []string{"perception-ro"}},
			{Name: "perception-ro", GID: 7011},
		},
	}
	s := state.New()
	s.Groups["perception"] = okReaders(okFolder(state.Group{Name: "perception", GID: 7001}), 7011)
	s.Groups["perception-ro"] = okReaders(okFolder(state.Group{Name: "perception-ro", GID: 7011}))
	if got := Reconcile(d, s, cls()); hasKind(got, SetGroupReaders) {
		t.Errorf("readers already correct, but proposed %v", kinds(got))
	}
}

// A reader removed from the roster must drive the ACL back — the folder still
// grants a gid the roster no longer declares.
func TestReadersDriftWhenRosterNarrows(t *testing.T) {
	d := &roster.Roster{
		Groups: []roster.Group{{Name: "perception", GID: 7001}}, // no readers now
	}
	s := state.New()
	s.Groups["perception"] = okReaders(okFolder(state.Group{Name: "perception", GID: 7001}), 7011)
	got := Reconcile(d, s, cls())
	if !hasKind(got, SetGroupReaders) {
		t.Fatalf("a stale reader on the folder was not corrected: %v", kinds(got))
	}
	for _, a := range got {
		if a.Kind == SetGroupReaders && len(a.ReaderGIDs) != 0 {
			t.Errorf("want empty reader set to clear the ACL, got %v", a.ReaderGIDs)
		}
	}
}
