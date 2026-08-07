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

// Presence in the roster — at ANY status — is what reserves the uid, so it is
// the precondition detach checks before releasing a local account.
func TestRosterDeclares(t *testing.T) {
	ro := &roster.Roster{Users: []roster.User{
		{Name: "skim", UID: 3001},
		{Name: "park", UID: 3004, Status: roster.Disabled},
		{Name: "oldhand", UID: 3005, Status: roster.Reserved},
	}}
	for _, tt := range []struct {
		user string
		want bool
	}{
		{"skim", true},
		// A non-active entry reserves the uid just as firmly as an active one, so
		// it satisfies the precondition too.
		{"park", true},
		{"oldhand", true},
		{"nobody", false},
		{"", false},
	} {
		if got := rosterDeclares(ro, tt.user); got != tt.want {
			t.Errorf("rosterDeclares(%q) = %v, want %v", tt.user, got, tt.want)
		}
	}
}
