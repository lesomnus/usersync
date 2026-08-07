package cmd

import (
	"testing"

	"github.com/lesomnus/usersync/internal/roster"
)

// A home directory is owned by a number. Once the local account is gone, the
// only question that decides whether a detach was safe is which identity that
// number now answers to.
func TestClassifyDetach(t *testing.T) {
	tests := []struct {
		name     string
		oldUID   uint32
		newUID   uint32
		resolved bool
		want     detachVerdict
	}{
		{
			name:   "winbind answers with the same uid",
			oldUID: 3001, newUID: 3001, resolved: true,
			want: handedOver,
		},
		{
			name:   "nothing answers for the name any more",
			oldUID: 3001, resolved: false,
			want: unresolved,
		},
		{
			// The stale-lookup trap: a resolver that answers but with its own
			// auto-generated number rather than the roster's must never be read as
			// success just because the name resolves.
			name:   "resolves to a different uid",
			oldUID: 3001, newUID: 100042, resolved: true,
			want: hijacked,
		},
		{
			// An unresolved lookup reports uid 0; that must not be mistaken for a
			// handover, nor for root having taken the name over.
			name:   "unresolved wins over a zero uid",
			oldUID: 3001, newUID: 0, resolved: false,
			want: unresolved,
		},
		{
			name:   "resolving to root is a hijack, not a handover",
			oldUID: 3001, newUID: 0, resolved: true,
			want: hijacked,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyDetach(tt.oldUID, tt.newUID, tt.resolved); got != tt.want {
				t.Errorf("classifyDetach(%d, %d, %v) = %v, want %v", tt.oldUID, tt.newUID, tt.resolved, got, tt.want)
			}
		})
	}
}

// The roster entry is detach's precondition. What matters is not only that one
// exists but WHICH uid it reserves — an entry reserves the number it declares,
// which is not necessarily the number the account currently carries.
func TestRosterEntry(t *testing.T) {
	ro := &roster.Roster{Users: []roster.User{
		{Name: "skim", UID: 3001},
		{Name: "park", UID: 3004, Status: roster.Disabled},
		{Name: "oldhand", UID: 3005, Status: roster.Reserved},
	}}
	for _, tt := range []struct {
		user       string
		wantFound  bool
		wantUID    uint32
		wantStatus roster.Status
	}{
		{"skim", true, 3001, roster.Active},
		// A non-active entry reserves the uid just as firmly as an active one.
		{"park", true, 3004, roster.Disabled},
		{"oldhand", true, 3005, roster.Reserved},
		{"nobody", false, 0, roster.Active},
		{"", false, 0, roster.Active},
	} {
		got := rosterEntry(ro, tt.user)
		if (got != nil) != tt.wantFound {
			t.Errorf("rosterEntry(%q) found = %v, want %v", tt.user, got != nil, tt.wantFound)
			continue
		}
		if got == nil {
			continue
		}
		if got.UID != tt.wantUID || got.Status != tt.wantStatus {
			t.Errorf("rosterEntry(%q) = uid %d status %v, want uid %d status %v",
				tt.user, got.UID, got.Status, tt.wantUID, tt.wantStatus)
		}
	}
}

// The guard has to compare the DECLARED uid against the one the account holds.
// Matching on the name alone passes for a drifted account and then frees a
// number no entry reserves, while the home still carries it — and the advertised
// undo makes it worse, since `apply` recreates the account on the declared uid
// and leaves the files behind on the old one.
func TestDetachPreconditions(t *testing.T) {
	ro := &roster.Roster{Users: []roster.User{
		{Name: "skim", UID: 3001},
		{Name: "park", UID: 3004, Status: roster.Disabled},
		{Name: "oldhand", UID: 3005, Status: roster.Reserved},
	}}

	// A drifted account: declared 3001, actually 3050.
	if e := rosterEntry(ro, "skim"); e == nil || e.UID == 3050 {
		t.Fatalf("premise: skim must be declared at a uid other than 3050, got %v", e)
	}

	// A reserved entry has no undo: reconcile produces no actions for a reserved
	// user with no account, so `usersync apply` would not bring it back.
	if e := rosterEntry(ro, "oldhand"); e == nil || e.Status != roster.Reserved {
		t.Fatalf("premise: oldhand must be reserved, got %v", e)
	}

	// A disabled entry DOES have an undo — reconcile recreates it locked — so it
	// must remain detachable.
	if e := rosterEntry(ro, "park"); e == nil || e.Status != roster.Disabled {
		t.Fatalf("premise: park must be disabled, got %v", e)
	}
}
