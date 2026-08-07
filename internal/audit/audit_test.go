package audit

import (
	"sort"
	"strings"
	"testing"

	"github.com/lesomnus/usersync/internal/idrange"
	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/usersync/internal/state"
)

func classifier() *idrange.Classifier {
	return idrange.New(idrange.Config{
		SystemFloor: 1000,
		UID:         idrange.Set{Manage: idrange.Range{Min: 3000, Max: 9999}},
		GID:         idrange.Set{Manage: idrange.Range{Min: 10000, Max: 19999}},
	})
}

func declared() *roster.Roster {
	return &roster.Roster{
		Groups: []roster.Group{{Name: "team-a", GID: 10001}},
		Users: []roster.User{
			{Name: "skim", UID: 3001},
			{Name: "park", UID: 3004, Status: roster.Disabled},
			{Name: "oldhand", UID: 3005, Status: roster.Reserved},
		},
	}
}

// resolved builds the scanned state from name->id pairs, as NSS would answer.
// Both the user and group maps are populated exactly as given; the audit reads
// the unfiltered AllUsers/AllGroups, which is what makes out-of-band drift
// visible instead of looking like an absence.
func resolved(users, groups map[string]uint32) *state.State {
	st := state.New()
	for n, id := range users {
		st.AllUsers[n] = id
	}
	for n, id := range groups {
		st.AllGroups[n] = id
	}
	return st
}

// deriveKeyed models a system where the keyed lookups and the enumeration agree
// — everything local. A directory that answers keyed lookups without enumerating
// is the interesting case, and TestDirectoryThatDoesNotEnumerate covers it.
func deriveKeyed(st *state.State) Resolved {
	r := Resolved{Users: map[string]uint32{}, Groups: map[string]uint32{}}
	for n, id := range st.AllUsers {
		r.Users[n] = id
	}
	for n, id := range st.AllGroups {
		r.Groups[n] = id
	}
	return r
}

func runAudit(ro *roster.Roster, st *state.State) Report {
	return Run(ro, deriveKeyed(st), st, classifier())
}

// The healthy case: every declared name resolves to exactly the declared number,
// the tombstone resolves to nothing, and out-of-band system accounts are ignored.
func TestCleanAudit(t *testing.T) {
	rep := runAudit(declared(), resolved(
		map[string]uint32{
			"skim": 3001, "park": 3004,
			"root": 0, "nobody": 65534, // out of band: not our business
		},
		map[string]uint32{
			"team-a": 10001,
			"skim":   3001, "park": 3004, // UPGs: gid == uid, outside the team-gid window
			"sudo": 27,
		},
	))

	if !rep.OK() {
		t.Errorf("expected a clean audit, got %d findings:\n%s", len(rep.Findings), render(rep))
	}
	if rep.UsersChecked != 3 || rep.GroupsChecked != 1 {
		t.Errorf("checked %d users / %d groups, want 3 / 1", rep.UsersChecked, rep.GroupsChecked)
	}
}

// The state this command exists for: the accounts have been handed to a
// directory, which answers a keyed lookup for every one of them and enumerates
// none of them. winbind does not enumerate unless `winbind enum users = yes`,
// and sssd defaults `enumerate = false`, because enumerating a domain is
// expensive — so this is the DEFAULT configuration, not an exotic one.
//
// Judging declared entries from the enumeration would report every single user
// as missing and exit non-zero: an alarm on every user, every run, from the one
// tool that is supposed to still work after the handover.
func TestDirectoryThatDoesNotEnumerate(t *testing.T) {
	keyed := Resolved{
		Users:  map[string]uint32{"skim": 3001, "park": 3004},
		Groups: map[string]uint32{"team-a": 10001},
	}
	empty := state.New() // the enumeration returns nothing at all

	rep := Run(declared(), keyed, empty, classifier())
	if !rep.OK() {
		t.Errorf("a directory that answers keyed lookups but does not enumerate must audit clean, got:\n%s", render(rep))
	}
	if rep.UsersChecked != 3 || rep.GroupsChecked != 1 {
		t.Errorf("checked %d users / %d groups, want 3 / 1", rep.UsersChecked, rep.GroupsChecked)
	}
	// ...and the report has to admit how little the enumeration-based half saw,
	// so a clean result is not read as proof that nothing else is out there.
	if rep.EnumeratedUsers != 0 || rep.EnumeratedGroups != 0 {
		t.Errorf("enumerated counts = %d/%d, want 0/0", rep.EnumeratedUsers, rep.EnumeratedGroups)
	}
}

// The keyed lookup is what decides a declared entry, so drift is still caught
// even when nothing enumerates.
func TestKeyedLookupStillCatchesDrift(t *testing.T) {
	keyed := Resolved{
		Users:  map[string]uint32{"skim": 100042, "park": 3004},
		Groups: map[string]uint32{"team-a": 10001},
	}
	rep := Run(declared(), keyed, state.New(), classifier())
	got := codesFor(rep, "user", "skim")
	if len(got) != 1 || got[0] != IDMismatch {
		t.Errorf("skim codes = %v, want [id-mismatch]:\n%s", got, render(rep))
	}
}

func codesFor(rep Report, kind, name string) []Code {
	var out []Code
	for _, f := range rep.Findings {
		if f.Kind == kind && f.Name == name {
			out = append(out, f.Code)
		}
	}
	return out
}

func render(rep Report) string {
	var b strings.Builder
	for _, f := range rep.Findings {
		b.WriteString("  " + f.String() + "\n")
	}
	return b.String()
}

func TestFindings(t *testing.T) {
	tests := []struct {
		name       string
		users      map[string]uint32
		groups     map[string]uint32
		wantKind   string
		wantName   string
		wantCodes  []Code
		wantDetail string
	}{
		{
			name:      "declared user does not resolve at all",
			users:     map[string]uint32{"park": 3004},
			groups:    map[string]uint32{"team-a": 10001},
			wantKind:  "user",
			wantName:  "skim",
			wantCodes: []Code{Missing},
		},
		{
			// The dangerous one: the directory answered, but with its own number.
			// The files never moved, so the person no longer owns their own data.
			name:      "declared user resolves to a different uid",
			users:     map[string]uint32{"skim": 100042, "park": 3004},
			groups:    map[string]uint32{"team-a": 10001},
			wantKind:  "user",
			wantName:  "skim",
			wantCodes: []Code{IDMismatch},
		},
		{
			name:      "declared group resolves to a different gid",
			users:     map[string]uint32{"skim": 3001, "park": 3004},
			groups:    map[string]uint32{"team-a": 19999},
			wantKind:  "group",
			wantName:  "team-a",
			wantCodes: []Code{IDMismatch},
		},
		{
			// The reservation exists to keep the number from being handed out. If
			// the name resolves, it was.
			name:      "reserved tombstone resolves",
			users:     map[string]uint32{"skim": 3001, "park": 3004, "oldhand": 3005},
			groups:    map[string]uint32{"team-a": 10001},
			wantKind:  "user",
			wantName:  "oldhand",
			wantCodes: []Code{TombstoneLive},
		},
		{
			name:      "undeclared user inside the band",
			users:     map[string]uint32{"skim": 3001, "park": 3004, "intruder": 3007},
			groups:    map[string]uint32{"team-a": 10001},
			wantKind:  "user",
			wantName:  "intruder",
			wantCodes: []Code{Undeclared},
		},
		{
			name:      "undeclared group inside the band",
			users:     map[string]uint32{"skim": 3001, "park": 3004},
			groups:    map[string]uint32{"team-a": 10001, "team-ghost": 10009},
			wantKind:  "group",
			wantName:  "team-ghost",
			wantCodes: []Code{Undeclared},
		},
		{
			// A hand-made group sharing a user's name but sitting on a real team
			// gid must NOT be waved through as "that's just the UPG".
			name:      "group named after a user but on a team gid",
			users:     map[string]uint32{"skim": 3001, "park": 3004},
			groups:    map[string]uint32{"team-a": 10001, "skim": 12000},
			wantKind:  "group",
			wantName:  "skim",
			wantCodes: []Code{Undeclared},
		},
		{
			// Two names on one number: each can read the other's files, and no
			// roster-side uniqueness check can see it, because the roster declares
			// only one of them.
			name:       "two names resolve to one uid",
			users:      map[string]uint32{"skim": 3001, "park": 3004, "shadow": 3001},
			groups:     map[string]uint32{"team-a": 10001},
			wantKind:   "user",
			wantName:   "shadow",
			wantCodes:  []Code{Collision, Undeclared},
			wantDetail: "skim",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep := runAudit(declared(), resolved(tt.users, tt.groups))
			if rep.OK() {
				t.Fatalf("expected findings, got a clean audit")
			}
			got := codesFor(rep, tt.wantKind, tt.wantName)
			sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
			want := append([]Code(nil), tt.wantCodes...)
			sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
			if strings.Join(asStrings(got), ",") != strings.Join(asStrings(want), ",") {
				t.Errorf("%s %q codes = %v, want %v\nreport:\n%s", tt.wantKind, tt.wantName, got, want, render(rep))
			}
			if tt.wantDetail != "" {
				found := false
				for _, f := range rep.Findings {
					if f.Name == tt.wantName && f.Code == Collision && strings.Contains(f.Detail, tt.wantDetail) {
						found = true
					}
				}
				if !found {
					t.Errorf("collision on %q should name %q:\n%s", tt.wantName, tt.wantDetail, render(rep))
				}
			}
		})
	}
}

func asStrings(cs []Code) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c)
	}
	return out
}

// A collision reports every participant, so each line stands alone in a log.
func TestCollisionReportsBothSides(t *testing.T) {
	rep := runAudit(declared(), resolved(
		map[string]uint32{"skim": 3001, "park": 3004, "shadow": 3001},
		map[string]uint32{"team-a": 10001},
	))

	for _, name := range []string{"skim", "shadow"} {
		if len(codesFor(rep, "user", name)) == 0 {
			t.Errorf("collision must be reported for %q too:\n%s", name, render(rep))
		}
	}
}

// A collision outside the managed band is somebody else's business — the audit
// must not report on ids usersync was never given authority over.
func TestIgnoresOutOfBandCollision(t *testing.T) {
	rep := runAudit(&roster.Roster{}, resolved(
		map[string]uint32{"alice": 1500, "bob": 1500},
		nil,
	))
	if !rep.OK() {
		t.Errorf("out-of-band ids must be ignored, got:\n%s", render(rep))
	}
}

// Findings come back in a stable order so the report can be diffed between runs
// (it is meant to be run from cron) without spurious churn from map iteration.
func TestDeterministicOrder(t *testing.T) {
	users := map[string]uint32{"zeta": 3009, "alpha": 3008, "skim": 100042, "shadow": 3001, "extra": 3001}
	groups := map[string]uint32{"team-a": 19999, "team-z": 10500}

	first := render(runAudit(declared(), resolved(users, groups)))
	for i := 0; i < 20; i++ {
		if got := render(runAudit(declared(), resolved(users, groups))); got != first {
			t.Fatalf("order is not stable across runs:\n%s\n---\n%s", first, got)
		}
	}
}
