// Package state holds the actual system state usersync collects (from getent
// and pdbedit) and reconciles the desired roster against. It is a plain data
// package with no behavior, so it can be constructed by providers and consumed
// by the reconciler and reports without import cycles.
package state

// User is an actual unix user account within the managed range.
type User struct {
	Name       string
	UID        uint32
	GID        uint32   // primary group id
	Groups     []string // supplementary group names
	FullName   string   // GECOS display name
	Home       string
	Shell      string
	HomeExists bool // whether the managed home directory is present
}

// Group is an actual unix group within the managed range.
type Group struct {
	Name         string
	GID          uint32
	FolderExists bool // whether the managed group folder is present
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
