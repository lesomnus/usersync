package roster

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/usersync/internal/idrange"
)

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
func (ro *Roster) Validate(cls *idrange.Classifier, policy Policy) ([]Skipped, error) {
	var errs []error
	var skipped []Skipped

	// --- uniqueness across ALL declared entries (reuse guard: a retired uid/name
	// stays reserved and cannot be reused, so duplicates are rejected even across
	// entries that will later be dropped) ---
	seenGroupName := map[string]bool{}
	seenGID := map[uint32]string{}
	for _, g := range ro.Groups {
		if g.Name == "" {
			errs = append(errs, fmt.Errorf("group with empty name"))
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

	seenUserName := map[string]bool{}
	seenUID := map[uint32]string{}
	for _, u := range ro.Users {
		if u.Name == "" {
			errs = append(errs, fmt.Errorf("user with empty name"))
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
	ro.Groups = keptGroups
	ro.Users = keptUsers
	return skipped, nil
}
