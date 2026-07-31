// Package state holds the actual system state usersync collects (from getent
// and pdbedit) and reconciles the desired roster against. It is a plain data
// package with no behavior, so it can be constructed by providers and consumed
// by the reconciler and reports without import cycles.
package state

// User is an actual unix user account within the managed range.
type User struct {
	Name     string
	UID      uint32
	GID      uint32   // primary group id
	Groups   []string // supplementary group names
	FullName string   // GECOS display name
	Home     string
	Shell    string

	// Home directory observation (via fsops.Stat), used to heal a missing or
	// drifted home. HomePerm folds the setgid bit in as 0o2000.
	HomeExists bool
	HomePerm   uint32
	HomeUID    uint32
	HomeGID    uint32
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
}

// New returns an empty State with initialized maps.
func New() *State {
	return &State{
		Users:  map[string]User{},
		Groups: map[string]Group{},
		Smb:    map[string]Smb{},
	}
}
