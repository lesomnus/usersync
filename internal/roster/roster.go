// Package roster defines the declared desired state (users and groups) and its
// YAML loading and validation. It mirrors proto/usersync/roster.proto and is
// kept protojson-compatible: snake_case keys, and a lowercase Status enum.
package roster

import (
	"fmt"
	"strings"
)

// Status is the lifecycle of a user entry. The zero value is Active, so an
// omitted `status` key means active (see plan.md §4 "생명주기"). Whatever the
// status, an entry's presence keeps its uid+name reserved from reuse.
type Status int

const (
	// Active: account exists, SMB enabled.
	Active Status = iota
	// Disabled: account + home kept, SMB disabled (reversible); uid stays reserved.
	Disabled
	// Reserved: no managed account; uid + name reserved to block reuse (tombstone).
	Reserved
)

func (s Status) String() string {
	switch s {
	case Active:
		return "active"
	case Disabled:
		return "disabled"
	case Reserved:
		return "reserved"
	default:
		return fmt.Sprintf("Status(%d)", int(s))
	}
}

// ParseStatus maps a YAML scalar to a Status. Empty and "active" both map to
// Active so the field is optional.
func ParseStatus(s string) (Status, error) {
	switch strings.TrimSpace(s) {
	case "", "active":
		return Active, nil
	case "disabled":
		return Disabled, nil
	case "reserved":
		return Reserved, nil
	default:
		return Active, fmt.Errorf("invalid status %q (want active|disabled|reserved)", s)
	}
}

// UnmarshalYAML implements goccy's BytesUnmarshaler so `status: disabled` parses
// directly to the enum.
func (s *Status) UnmarshalYAML(b []byte) error {
	// A plain scalar arrives unquoted, but be defensive about quoting.
	raw := strings.Trim(strings.TrimSpace(string(b)), `"'`)
	v, err := ParseStatus(raw)
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// MarshalYAML implements goccy's BytesMarshaler so a Status encodes as its
// lowercase name (matching the proto enum value).
func (s Status) MarshalYAML() ([]byte, error) {
	return []byte(s.String()), nil
}

// Group is a shared (team) group and its backing folder.
type Group struct {
	Name        string `yaml:"name"`
	GID         uint32 `yaml:"gid"`
	Description string `yaml:"description,omitempty"`

	// Owners are the group's administrators — the people who may add and remove
	// its members.
	//
	// Not an invention: POSIX already has this, in the third field of
	// /etc/gshadow, and `gpasswd -A` is what sets it. An administrator there can
	// run `gpasswd -a`/`-d` on that group WITHOUT being root, which is exactly
	// the delegation being declared. usersync applies it, so the roster and the
	// system can be compared (`usersync audit`) instead of the roster asserting
	// something nothing enforces.
	//
	// Where the users have /usr/sbin/nologin and a locked unix password, no
	// member can run gpasswd, so the gshadow entry is a RECORD rather than an
	// access control — enforcement is whatever front end people actually reach.
	// It is still worth writing where POSIX keeps it: `getent gshadow` then
	// agrees with the roster, and an on-prem AD carries the same fact as a
	// group's `managedBy`.
	//
	// Only shadow-utils has gshadow. busybox and pw have no equivalent, so this
	// is reported as unsupported on those backends rather than silently dropped.
	//
	// Note what it does NOT change: the roster still owns the membership list.
	// An administrator who runs `gpasswd -a` adds a member, and the next
	// `usersync apply` takes them back out, because `users[].groups` is the
	// desired state and apply replaces the set exactly. The delegation is real
	// but the durable path is an edit to this file — which is what
	// `usersync member` is for.
	Owners []string `yaml:"owners,omitempty"`
}

// User is a managed SMB-only user.
type User struct {
	Name     string   `yaml:"name"`
	UID      uint32   `yaml:"uid"`
	FullName string   `yaml:"full_name,omitempty"`
	Groups   []string `yaml:"groups,omitempty"`
	Status   Status   `yaml:"status,omitempty"`
}

// Roster is the complete declared desired state.
type Roster struct {
	Groups []Group `yaml:"groups,omitempty"`
	Users  []User  `yaml:"users,omitempty"`
}
