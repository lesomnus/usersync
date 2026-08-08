package cmd

import (
	"testing"

	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/usersync/internal/state"
)

// scanned builds the actual state a Scan would return: two live users and one
// team group. Note what is NOT here — a description, and any trace of a
// reserved user. Neither is observable from the system, which is the whole
// reason export has to merge.
func scanned() *state.State {
	return &state.State{
		Groups: map[string]state.Group{
			"team-a": {Name: "team-a", GID: 10001},
		},
		Users: map[string]state.User{
			"skim": {Name: "skim", UID: 3001, FullName: "Sunghyun Kim", Groups: []string{"team-a"}},
			"jlee": {Name: "jlee", UID: 3002, FullName: "Jiwon Lee"},
		},
		Smb: map[string]state.Smb{
			"skim": {Name: "skim", Enabled: true},
			"jlee": {Name: "jlee", Enabled: true},
		},
	}
}

func find(t *testing.T, ro *roster.Roster, name string) roster.User {
	t.Helper()
	for _, u := range ro.Users {
		if u.Name == name {
			return u
		}
	}
	t.Fatalf("user %q missing from export; roster has %d users", name, len(ro.Users))
	return roster.User{}
}

// The documented bootstrap is `export`, review, commit. If export drops the
// reserved tombstones, the very next `apply` is free to hand uid 3005 to a new
// hire — and the files uid 3005 still owns silently become theirs. That is the
// one failure this project exists to prevent, so it gets a test.
func TestExportKeepsReservedTombstones(t *testing.T) {
	prior := &roster.Roster{
		Users: []roster.User{
			{Name: "oldhand", UID: 3005, FullName: "Retired User", Status: roster.Reserved},
		},
	}

	ro := stateToRoster(scanned(), prior)

	u := find(t, ro, "oldhand")
	if u.Status != roster.Reserved || u.UID != 3005 {
		t.Errorf("oldhand = %+v, want uid 3005 reserved", u)
	}
	if got := find(t, ro, "skim").Status; got != roster.Active {
		t.Errorf("skim status = %v, want active", got)
	}
}

// A group description exists nowhere but the roster, and `shares` renders it as
// the SMB comment. Losing it on export means the next `shares --write` quietly
// rewrites smb.conf with a generated placeholder.
func TestExportKeepsGroupDescriptions(t *testing.T) {
	prior := &roster.Roster{
		Groups: []roster.Group{{Name: "team-a", GID: 10001, Description: "Perception team"}},
	}

	ro := stateToRoster(scanned(), prior)

	if len(ro.Groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(ro.Groups))
	}
	if got := ro.Groups[0].Description; got != "Perception team" {
		t.Errorf("description = %q, want %q", got, "Perception team")
	}
}

// Without a prior roster — the genuine first-time bootstrap — export must still
// work and simply have nothing to carry.
func TestExportWithoutPriorRoster(t *testing.T) {
	ro := stateToRoster(scanned(), nil)

	if len(ro.Users) != 2 || len(ro.Groups) != 1 {
		t.Fatalf("got %d users / %d groups, want 2 / 1", len(ro.Users), len(ro.Groups))
	}
	if got := ro.Groups[0].Description; got != "" {
		t.Errorf("description = %q, want empty", got)
	}
}

// A name declared reserved that nonetheless resolves is a contradiction. The
// scan wins (it is what the system will actually do), and `audit` is the place
// that reports the disagreement — export must not emit the name twice.
func TestExportPrefersScanOverStaleReservation(t *testing.T) {
	prior := &roster.Roster{
		Users: []roster.User{{Name: "skim", UID: 9999, Status: roster.Reserved}},
	}

	ro := stateToRoster(scanned(), prior)

	n := 0
	for _, u := range ro.Users {
		if u.Name == "skim" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("skim appears %d times, want 1", n)
	}
	if u := find(t, ro, "skim"); u.UID != 3001 || u.Status == roster.Reserved {
		t.Errorf("skim = %+v, want the scanned uid 3001 and not reserved", u)
	}
}
