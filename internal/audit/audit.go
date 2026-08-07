// Package audit compares the roster against what the system actually resolves,
// without changing anything.
//
// It exists for the state after a directory service takes the accounts over.
// From that point usersync must not create or modify anything — the directory
// owns the accounts — but the roster is still the ledger of which number belongs
// to whom, and nothing else checks that the directory agrees with it. Drift here
// is silent and expensive: files are owned by numeric uid, so a name that comes
// back pointing at a different number means the data and the identity have come
// apart, and on a filesystem with snapshots that cannot be corrected after the
// fact.
//
// The comparison is a pure function of (roster, scanned state) so every verdict
// is unit-testable without a directory, a domain, or root.
package audit

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lesomnus/usersync/internal/idrange"
	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/usersync/internal/state"
)

// Code is what is wrong with one declared or observed identity.
type Code string

const (
	// Missing: the roster declares the entry, but nothing resolves the name.
	// After a handover this means the directory has not got the account, so the
	// user cannot authenticate and their files have no owner that resolves.
	Missing Code = "missing"

	// IDMismatch: the name resolves, but to a different number than declared.
	// The most dangerous finding — the files stayed on the old number while the
	// name moved to a new one, so the person no longer owns their own data and
	// whoever is given the old number will.
	IDMismatch Code = "id-mismatch"

	// TombstoneLive: a reserved entry resolves to something. The reservation
	// exists precisely to keep that number from being handed to anyone else.
	TombstoneLive Code = "tombstone-live"

	// Undeclared: a name resolves to a number inside the managed band that the
	// roster does not declare. Someone allocated out of the reserved band without
	// going through the ledger.
	Undeclared Code = "undeclared"

	// Collision: two names resolve to the same number inside the managed band.
	// Both identities can read each other's files.
	Collision Code = "collision"
)

// Finding is one problem, about one name.
type Finding struct {
	Kind string `json:"kind"` // "user" or "group"
	Name string `json:"name"`
	Code Code   `json:"code"`

	// Want is the number the roster declares. Zero and meaningless for
	// Undeclared and Collision, which start from what was observed.
	Want uint32 `json:"want,omitempty"`
	// Got is the number the system resolves, valid only when HasGot.
	Got    uint32 `json:"got,omitempty"`
	HasGot bool   `json:"-"`

	// Detail names the other party for a Collision.
	Detail string `json:"detail,omitempty"`
}

// String renders one finding as a single reviewable line.
func (f Finding) String() string {
	switch f.Code {
	case Missing:
		return fmt.Sprintf("%-5s %-16s declared %d, but the name does not resolve", f.Kind, f.Name, f.Want)
	case IDMismatch:
		return fmt.Sprintf("%-5s %-16s declared %d, but resolves to %d — files stay on %d", f.Kind, f.Name, f.Want, f.Got, f.Want)
	case TombstoneLive:
		return fmt.Sprintf("%-5s %-16s is a reserved tombstone, but the name resolves to %d", f.Kind, f.Name, f.Got)
	case Undeclared:
		return fmt.Sprintf("%-5s %-16s resolves to %d inside the managed band but is not in the roster", f.Kind, f.Name, f.Got)
	case Collision:
		return fmt.Sprintf("%-5s %-16s shares number %d with %s", f.Kind, f.Name, f.Got, f.Detail)
	default:
		return fmt.Sprintf("%-5s %-16s %s", f.Kind, f.Name, f.Code)
	}
}

// Report is the outcome of one audit.
type Report struct {
	UsersChecked  int       `json:"users_checked"`
	GroupsChecked int       `json:"groups_checked"`
	Findings      []Finding `json:"findings"`
}

// OK reports whether the system agrees with the roster.
func (r Report) OK() bool { return len(r.Findings) == 0 }

// Run compares the roster against the scanned state.
//
// Lookups go through st.AllUsers/AllGroups — every name the scan saw, NOT the
// managed-range-filtered maps. That is the point: a declared user that has come
// back on an out-of-band number is exactly the drift worth catching, and the
// filtered maps would hide it as "missing".
func Run(ro *roster.Roster, st *state.State, cls *idrange.Classifier) Report {
	// Non-nil so a clean run marshals as `"findings": []` rather than `null` —
	// this report is meant to be consumed by cron and dashboards.
	rep := Report{Findings: []Finding{}}
	add := func(f Finding) { rep.Findings = append(rep.Findings, f) }

	declaredUsers := map[string]bool{}
	for _, u := range ro.Users {
		declaredUsers[u.Name] = true
		rep.UsersChecked++

		got, resolved := st.AllUsers[u.Name]
		if u.Status == roster.Reserved {
			// A tombstone declares that nobody holds this name or number.
			if resolved {
				add(Finding{Kind: "user", Name: u.Name, Code: TombstoneLive, Want: u.UID, Got: got, HasGot: true})
			}
			continue
		}
		switch {
		case !resolved:
			add(Finding{Kind: "user", Name: u.Name, Code: Missing, Want: u.UID})
		case got != u.UID:
			add(Finding{Kind: "user", Name: u.Name, Code: IDMismatch, Want: u.UID, Got: got, HasGot: true})
		}
	}

	declaredGroups := map[string]bool{}
	for _, g := range ro.Groups {
		declaredGroups[g.Name] = true
		rep.GroupsChecked++

		got, resolved := st.AllGroups[g.Name]
		switch {
		case !resolved:
			add(Finding{Kind: "group", Name: g.Name, Code: Missing, Want: g.GID})
		case got != g.GID:
			add(Finding{Kind: "group", Name: g.Name, Code: IDMismatch, Want: g.GID, Got: got, HasGot: true})
		}
	}

	// Anything resolving inside the reserved band that the ledger does not know
	// about — someone allocated from the band without going through the roster.
	//
	// User private groups do not show up here, and deliberately are not
	// special-cased away: a UPG's gid equals its uid, which Config.Validate keeps
	// disjoint from the team-group window, so it classifies as out-of-scope for
	// gids and never reaches this check. Excluding names that happen to match a
	// declared user would instead hide a real finding — a hand-made group that
	// shares a user's name but sits on a genuine team gid.
	for name, uid := range st.AllUsers {
		if cls.UID(uid) == idrange.Managed && !declaredUsers[name] {
			add(Finding{Kind: "user", Name: name, Code: Undeclared, Got: uid, HasGot: true})
		}
	}
	for name, gid := range st.AllGroups {
		if cls.GID(gid) == idrange.Managed && !declaredGroups[name] {
			add(Finding{Kind: "group", Name: name, Code: Undeclared, Got: gid, HasGot: true})
		}
	}

	rep.Findings = append(rep.Findings, collisions("user", st.AllUsers, cls.UID)...)
	rep.Findings = append(rep.Findings, collisions("group", st.AllGroups, cls.GID)...)

	sort.SliceStable(rep.Findings, func(i, j int) bool {
		a, b := rep.Findings[i], rep.Findings[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Code < b.Code
	})
	return rep
}

// collisions reports every number inside the managed band that more than one
// name resolves to. Two names on one number means two identities that can read
// each other's files, which no amount of correct roster bookkeeping would catch
// on its own — the roster's uniqueness check covers what it declares, not what
// the directory answers.
func collisions(kind string, all map[string]uint32, class func(uint32) idrange.Class) []Finding {
	byID := map[uint32][]string{}
	for name, id := range all {
		if class(id) == idrange.Managed {
			byID[id] = append(byID[id], name)
		}
	}
	var out []Finding
	for id, names := range byID {
		if len(names) < 2 {
			continue
		}
		sort.Strings(names)
		for i, n := range names {
			// Name the other party, so each line stands alone when read in a log.
			others := append(append([]string{}, names[:i]...), names[i+1:]...)
			out = append(out, Finding{
				Kind: kind, Name: n, Code: Collision,
				Got: id, HasGot: true,
				Detail: strings.Join(others, ", "),
			})
		}
	}
	return out
}
