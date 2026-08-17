// Package state holds the actual system state usersync collects (from getent
// and pdbedit) and reconciles the desired roster against. It is a plain data
// package with no behavior, so it can be constructed by providers and consumed
// by the reconciler and reports without import cycles.
package state

// User is an actual unix user account within the managed range.
type User struct {
	Name        string
	UID         uint32
	GID         uint32   // primary group id
	Groups      []string // MANAGED supplementary group names (compared against the roster)
	ExtraGroups []string // supplementary memberships in NON-managed groups, preserved on update
	FullName    string   // GECOS display name
	Home        string
	Shell       string

	// Home directory observation (via fsops.Stat), used to heal a missing or
	// drifted home. HomePerm folds the setgid bit in as 0o2000.
	HomeExists bool
	HomePerm   uint32
	HomeUID    uint32
	HomeGID    uint32

	// Quota is the per-uid byte limit the quota backend currently enforces on this
	// account, 0 if none. It is only meaningful when State.QuotaEnforced is true;
	// otherwise the reconciler leaves quotas untouched.
	Quota uint64
}

// Group is an actual unix group within the managed range.
type Group struct {
	Name string
	GID  uint32

	// Group folder observation (via fsops.Stat). FolderPerm folds the setgid
	// bit in as 0o2000 (so a correct folder reads 0o2770).
	FolderExists bool
	FolderPerm   uint32
	FolderGID    uint32

	// Admins is the group's administrator list from /etc/gshadow, sorted.
	//
	// AdminsKnown separates "this group has no administrators" from "this
	// backend cannot tell": busybox and pw have no gshadow at all, and reporting
	// their silence as an empty list would make every declared owner look like
	// drift on every run.
	Admins      []string
	AdminsKnown bool

	// ReaderGIDs are the gids granted a read-only ACL entry on the folder,
	// sorted, as read back by getfacl. Compared against the roster's declared
	// reader groups to detect ACL drift. ReadersKnown separates "no readers"
	// from "the ACL could not be read" (a missing folder, or a filesystem with
	// no ACL support), so silence is never mistaken for "no readers declared".
	ReaderGIDs   []uint32
	ReadersKnown bool
}

// Smb is an actual SMB (tdbsam) account and whether it is enabled.
type Smb struct {
	Name    string
	Enabled bool
}

// State is the collected actual system state, keyed by name.
type State struct {
	Users  map[string]User
	Groups map[string]Group
	Smb    map[string]Smb

	// AllUsers/AllGroups map EVERY scanned account name (managed OR not) to its
	// id. They let the reconciler refuse creating a managed entry whose name
	// collides with a pre-existing out-of-range account/group (which the managed
	// Users/Groups maps hide), so a create never lands on someone else's account.
	AllUsers  map[string]uint32
	AllGroups map[string]uint32

	// QuotaEnforced is true when a working quota backend was probed during Collect.
	// The reconciler only proposes quota actions when it is set, so on a system
	// with no quota backend (or one that is down) declared quotas are simply not
	// acted on rather than churned.
	QuotaEnforced bool
}

// New returns an empty State with initialized maps.
func New() *State {
	return &State{
		Users:     map[string]User{},
		Groups:    map[string]Group{},
		Smb:       map[string]Smb{},
		AllUsers:  map[string]uint32{},
		AllGroups: map[string]uint32{},
	}
}
