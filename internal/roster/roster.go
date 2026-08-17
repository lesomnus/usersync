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

// Anonymous is the level of unauthenticated ("anonymous") access a group's
// folder grants over the web. The zero value is AnonNone, so an omitted
// `anonymous` key means the folder is private to its members and readers.
//
// It governs one thing only: the folder's "other" (world) permission bits, the
// third mode class. AnonNone leaves them closed (2770); AnonRead opens read+
// traverse (2775); AnonWrite opens read+write (2777, a fully public folder).
// The kernel then enforces the result over every path — the web's anonymous
// helper reaches exactly the world-accessible folders and nothing else, and a
// logged-in roster user reaches them the same way an ordinary "other" would.
//
// SMB is deliberately NOT widened to guests: an anonymous folder stays
// `guest ok = no`, so the anonymous (non-account) identity has no way to mount
// it — anonymous access is web-only — while any roster user can still mount it
// because the open "other" bits admit them (smbconf.go).
type Anonymous int

const (
	// AnonNone: no anonymous access; folder is 2770 (members write, readers read).
	AnonNone Anonymous = iota
	// AnonRead: anyone, even not signed in, may READ the folder (2775, o+rx).
	AnonRead
	// AnonWrite: anyone may READ and WRITE — a fully public folder (2777, o+rwx).
	AnonWrite
)

func (a Anonymous) String() string {
	switch a {
	case AnonNone:
		return "none"
	case AnonRead:
		return "read"
	case AnonWrite:
		return "write"
	default:
		return fmt.Sprintf("Anonymous(%d)", int(a))
	}
}

// ParseAnonymous maps a YAML scalar to an Anonymous. Empty and "none" both map
// to AnonNone so the field is optional.
func ParseAnonymous(s string) (Anonymous, error) {
	switch strings.TrimSpace(s) {
	case "", "none":
		return AnonNone, nil
	case "read":
		return AnonRead, nil
	case "write":
		return AnonWrite, nil
	default:
		return AnonNone, fmt.Errorf("invalid anonymous %q (want none|read|write)", s)
	}
}

// UnmarshalYAML implements goccy's BytesUnmarshaler so `anonymous: read` parses
// directly to the enum.
func (a *Anonymous) UnmarshalYAML(b []byte) error {
	raw := strings.Trim(strings.TrimSpace(string(b)), `"'`)
	v, err := ParseAnonymous(raw)
	if err != nil {
		return err
	}
	*a = v
	return nil
}

// MarshalYAML encodes an Anonymous as its lowercase name.
func (a Anonymous) MarshalYAML() ([]byte, error) {
	return []byte(a.String()), nil
}

// FolderPerm is the directory mode a group's folder must have for this level,
// with the setgid bit folded in as 0o2000 (matching state.Group.FolderPerm, so
// the reconciler compares desired against observed directly). Only the "other"
// nibble changes with the level; owner and group stay rwx/rwx.
func (a Anonymous) FolderPerm() uint32 {
	switch a {
	case AnonRead:
		return 0o2775
	case AnonWrite:
		return 0o2777
	default:
		return 0o2770
	}
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

	// Readers are OTHER groups whose members may READ this group's folder but
	// not write it. Each name must be a group declared in the same roster;
	// membership in a reader group is expressed the ordinary way, through
	// `users[].groups`, so nothing new manages people.
	//
	// It is groups rather than a list of users on purpose. mode bits have three
	// classes — owner, this group, other — with no room to split "writes" from
	// "reads" inside one group, and opening `other` would publish the folder to
	// everyone. The split lives in a POSIX ACL instead: the writer group keeps
	// its rwx, and each reader group is granted r-x, on the folder AND as a
	// default entry so files created afterwards inherit it. The kernel enforces
	// the result, over both the web path and SMB, exactly as it enforces the
	// mode bits (nas-design.md ADR-1).
	//
	// This does NOT reopen the self-service-sharing non-goal. That was refused
	// for the group explosion of granting arbitrary user sets access to
	// arbitrary folders — O(2^n). A reader group per team is O(teams): a second
	// named axis on a team that already exists, not an arbitrary combination.
	//
	// Enforcement needs POSIX ACLs, which not every filesystem stores. Where the
	// backend cannot, apply REFUSES rather than declaring a reader whose access
	// nothing enforces — a folder that looks read-restricted and is not is worse
	// than an error at apply time.
	Readers []string `yaml:"readers,omitempty"`

	// Anonymous opens this group's folder to unauthenticated web visitors. See
	// the Anonymous type. `read` makes it world-readable (any roster user OR an
	// anonymous web visitor may read; roster members still gate writes), `write`
	// makes it fully public (anyone may read and write). It is web-facing access
	// via open "other" bits, NOT an SMB guest share — the anonymous identity can
	// never mount over SMB. A reader group and anonymous access are mutually
	// exclusive (load.go): a read-only reader is meaningless on a folder already
	// open to the world.
	Anonymous Anonymous `yaml:"anonymous,omitempty"`

	// All, when true, makes this group contain EVERY active managed user, without
	// listing each in `users[].groups`. usersync maintains the membership: an
	// account added to the roster joins automatically, one disabled or reserved
	// leaves. It is the counterpart of Anonymous, on the other side of the
	// anonymous line — where Anonymous opens the folder's "other" bits to the
	// world (the nobody identity included), an `all` group is a real POSIX group
	// nobody is in, so used as a reader it grants read to every signed-in user and
	// to no anonymous visitor. That distinction cannot be a mode bit (there is no
	// "other except nobody"), which is why it is a group.
	//
	// The point is to name "everyone" once. `readers: [staff]` on any folder, with
	// staff declared `all: true`, is read for all registered users with no
	// per-user upkeep — the tedious alternative being to add the group to all of
	// them by hand and to remember every new hire. The group is otherwise ordinary
	// (it has a gid and a folder of its own), so its folder doubles as a
	// company-wide shared space, or is simply left unused.
	All bool `yaml:"all,omitempty"`
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
