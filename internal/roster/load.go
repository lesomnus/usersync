package roster

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/usersync/internal/idrange"
)

// NamePattern is the strict POSIX-portable account/group name: a lowercase
// letter or underscore, then lowercase/digit/underscore/hyphen, up to 32 chars.
// It is safe as a shadow-utils account name AND as a Samba smb.conf section
// name, and forbids a leading '-' (which the target CLI would read as a flag),
// whitespace, '/', '.', and control characters (smb.conf directive injection).
const NamePattern = `^[a-z_][a-z0-9_-]{0,31}$`

var reName = regexp.MustCompile(NamePattern)

// reservedSmbSection are Samba section names a group must not use, since a
// group name becomes a `[<name>]` share section: `global` would merge usersync
// directives into Samba's global defaults, `homes`/`printers` collide with
// Samba's special sections.
var reservedSmbSection = map[string]bool{"global": true, "homes": true, "printers": true}

func validName(s string) bool { return reName.MatchString(s) }

// ValidName reports whether s is a usersync-manageable account/group name (safe
// as a CLI arg and an smb.conf section). Exported so callers that take a name
// from argv or a system scan can guard it before it reaches an exec argument.
func ValidName(s string) bool { return validName(s) }

// hasControlOrNewline reports whether s contains any control character (incl.
// newline/CR/tab), which must never reach a command argument or smb.conf.
func hasControlOrNewline(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// Policy decides how an out-of-scope (neither managed nor protected) entry is
// handled during validation.
type Policy int

const (
	// PolicyError refuses the whole roster if any entry is out of scope.
	PolicyError Policy = iota
	// PolicySkip drops out-of-scope entries with a warning and continues.
	PolicySkip
)

// Skipped records an entry dropped by PolicySkip so it can be reported instead
// of silently vanishing.
type Skipped struct {
	Kind   string // "user" or "group"
	Name   string
	ID     uint32 // uid or gid
	Reason string
}

// Load strictly decodes a roster from r. Unknown keys and duplicate keys are
// rejected so typos surface immediately. It does not validate semantics; call
// Validate for that.
func Load(r io.Reader) (*Roster, error) {
	var ro Roster
	dec := yaml.NewDecoder(r, yaml.Strict())
	if err := dec.Decode(&ro); err != nil {
		if errors.Is(err, io.EOF) {
			return &ro, nil // empty document is a valid empty roster
		}
		return nil, fmt.Errorf("decode roster: %w", err)
	}
	return &ro, nil
}

// Validate checks the roster against the id classifier and referential rules,
// dropping out-of-scope entries when policy is PolicySkip. It mutates ro to
// remove skipped entries and returns the skipped list plus a combined error of
// all hard failures.
//
// Rules (plan.md §4):
//   - name and uid/gid must be unique across ALL entries (any status) — this is
//     what reserves a retired uid/name from reuse.
//   - full_name must not contain ',' or ':' (GECOS field separators).
//   - each user's supplementary group must be a defined group.
//   - Protected ids are always a hard error; out-of-scope ids error or skip.
// Validate DOES NOT MODIFY the receiver. It used to: it narrowed ro.Users and
// ro.Groups to the managed subset in place, which made "check this roster" and
// "throw away the parts I do not manage" the same call. That is fine for a
// process that validates and then exits, and a trap for anything that validates
// and then WRITES the file back — under `on_out_of_scope: skip` the entries it
// dropped would be gone from disk, and a dropped entry is a released uid
// reservation, which is the one thing this file exists to prevent.
//
// Callers that want the narrowed view ask for it: Validate reports, Managed
// selects.
func (ro *Roster) Validate(cls *idrange.Classifier, policy Policy) ([]Skipped, error) {
	var errs []error
	var skipped []Skipped

	// --- uniqueness across ALL declared entries (reuse guard: a retired uid/name
	// stays reserved and cannot be reused, so duplicates are rejected even across
	// entries that will later be dropped) ---
	seenGroupName := map[string]bool{}
	seenGID := map[uint32]string{}
	for _, g := range ro.Groups {
		if !validName(g.Name) {
			errs = append(errs, fmt.Errorf("invalid group name %q (must match %s)", g.Name, reName))
		}
		if reservedSmbSection[g.Name] {
			errs = append(errs, fmt.Errorf("group name %q is a reserved Samba section name", g.Name))
		}
		if hasControlOrNewline(g.Description) {
			errs = append(errs, fmt.Errorf("group %q description must be a single line (no control/newline chars)", g.Name))
		}
		if seenGroupName[g.Name] {
			errs = append(errs, fmt.Errorf("duplicate group name %q", g.Name))
		}
		seenGroupName[g.Name] = true
		if prev, ok := seenGID[g.GID]; ok {
			errs = append(errs, fmt.Errorf("duplicate gid %d (groups %q and %q)", g.GID, prev, g.Name))
		} else {
			seenGID[g.GID] = g.Name
		}
	}

	// Owners must be users this roster declares. An owner who is not would be
	// written into gshadow as a name the system may not resolve, and
	// `gpasswd -A` fails at apply — so catching it here turns a half-applied run
	// into a refusal to load.
	declaredUser := map[string]bool{}
	for _, u := range ro.Users {
		declaredUser[u.Name] = true
	}
	for _, g := range ro.Groups {
		seenOwner := map[string]bool{}
		for _, o := range g.Owners {
			if !validName(o) {
				errs = append(errs, fmt.Errorf("group %q owner %q is not a valid name (must match %s)", g.Name, o, reName))
				continue
			}
			if seenOwner[o] {
				errs = append(errs, fmt.Errorf("group %q lists owner %q twice", g.Name, o))
			}
			seenOwner[o] = true
			if !declaredUser[o] {
				errs = append(errs, fmt.Errorf("group %q owner %q is not a user in this roster", g.Name, o))
			}
		}
	}

	seenUserName := map[string]bool{}
	seenUID := map[uint32]string{}
	for _, u := range ro.Users {
		if !validName(u.Name) {
			errs = append(errs, fmt.Errorf("invalid user name %q (must match %s)", u.Name, reName))
		}
		if seenUserName[u.Name] {
			errs = append(errs, fmt.Errorf("duplicate user name %q", u.Name))
		}
		seenUserName[u.Name] = true
		if prev, ok := seenUID[u.UID]; ok {
			errs = append(errs, fmt.Errorf("duplicate uid %d (users %q and %q) — a retired uid stays reserved, it cannot be reused", u.UID, prev, u.Name))
		} else {
			seenUID[u.UID] = u.Name
		}
		if i := strings.IndexAny(u.FullName, ",:"); i >= 0 {
			errs = append(errs, fmt.Errorf("user %q full_name contains forbidden %q (GECOS separator)", u.Name, u.FullName[i]))
		}
		if hasControlOrNewline(u.FullName) {
			errs = append(errs, fmt.Errorf("user %q full_name must be a single line (no control/newline chars)", u.Name))
		}
	}

	// --- id classification: protected => always error; out-of-scope => error/skip ---
	keptGroups := ro.Groups[:0:0]
	keptGroupNames := map[string]bool{}
	for _, g := range ro.Groups {
		switch cls.GID(g.GID) {
		case idrange.Protected:
			errs = append(errs, fmt.Errorf("group %q gid %d is protected (< system_floor or reserved) — refusing", g.Name, g.GID))
		case idrange.OutOfScope:
			if policy == PolicySkip {
				skipped = append(skipped, Skipped{Kind: "group", Name: g.Name, ID: g.GID, Reason: "gid out of manage scope"})
			} else {
				errs = append(errs, fmt.Errorf("group %q gid %d is out of manage scope (use on_out_of_scope: skip to ignore)", g.Name, g.GID))
			}
		default: // Managed
			keptGroups = append(keptGroups, g)
			keptGroupNames[g.Name] = true
		}
	}

	keptUsers := ro.Users[:0:0]
	for _, u := range ro.Users {
		switch cls.UID(u.UID) {
		case idrange.Protected:
			errs = append(errs, fmt.Errorf("user %q uid %d is protected (< system_floor or reserved) — refusing", u.Name, u.UID))
		case idrange.OutOfScope:
			if policy == PolicySkip {
				skipped = append(skipped, Skipped{Kind: "user", Name: u.Name, ID: u.UID, Reason: "uid out of manage scope"})
			} else {
				errs = append(errs, fmt.Errorf("user %q uid %d is out of manage scope (use on_out_of_scope: skip to ignore)", u.Name, u.UID))
			}
		default: // Managed — validate its group references against the KEPT groups
			for _, gname := range u.Groups {
				if !keptGroupNames[gname] {
					errs = append(errs, fmt.Errorf("user %q references group %q that is not a managed group in this roster", u.Name, gname))
				}
			}
			keptUsers = append(keptUsers, u)
		}
	}

	if len(errs) > 0 {
		return skipped, errors.Join(errs...)
	}
	// Deliberately NOT assigned back onto ro. See the doc comment.
	_, _ = keptGroups, keptUsers
	return skipped, nil
}

// Managed returns a COPY of the roster narrowed to the entries this
// installation manages: protected and out-of-scope ids are dropped.
//
// Call it after Validate, on a roster Validate accepted. The reconciler works
// from this; anything that writes the roster back to disk must write the
// ORIGINAL, because the difference between the two is exactly the set of
// entries whose uid is reserved but not managed here.
func (ro *Roster) Managed(cls *idrange.Classifier) *Roster {
	out := &Roster{
		Groups: make([]Group, 0, len(ro.Groups)),
		Users:  make([]User, 0, len(ro.Users)),
	}
	for _, g := range ro.Groups {
		if cls.GID(g.GID) == idrange.Managed {
			out.Groups = append(out.Groups, g)
		}
	}
	for _, u := range ro.Users {
		if cls.UID(u.UID) == idrange.Managed {
			out.Users = append(out.Users, u)
		}
	}
	return out
}
