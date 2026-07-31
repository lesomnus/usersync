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
