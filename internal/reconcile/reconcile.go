// Package reconcile is the pure core: given the desired roster and the actual
// system state, it computes the ordered list of actions that converge the
// system to the roster. It performs no I/O and executes nothing, so it is fully
// unit-testable without root. See plan.md §5 for the state table.
package reconcile

import (
	"fmt"
	"slices"
	"sort"

	"github.com/lesomnus/usersync/internal/idrange"
	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/usersync/internal/state"
)

func reasonf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// mergeGroups returns the sorted, de-duplicated union of the desired managed
// groups and the preserved non-managed memberships, so a supplementary-group
// update replaces the set WITHOUT dropping memberships in protected/out-of-scope
// groups (e.g. docker, sudo).
func mergeGroups(desired, preserved []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range desired {
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	for _, g := range preserved {
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	sort.Strings(out)
	return out
}

// homeDrifted reports whether a present user's home directory is missing or has
// drifted from the desired 0700 owned by its UPG (uid == gid == UID).
func homeDrifted(u roster.User, cur state.User) bool {
	return !cur.HomeExists || cur.HomePerm != 0o700 || cur.HomeUID != u.UID || cur.HomeGID != u.UID
}

// folderDrifted reports whether a present group's folder is missing or has
// drifted from the desired 2770 setgid owned by the group.
func folderDrifted(g roster.Group, cur state.Group) bool {
	return !cur.FolderExists || cur.FolderPerm != 0o2770 || cur.FolderGID != g.GID
}

// Kind is the type of a reconcile action.
type Kind int

const (
	// --- groups ---
	CreateGroup    Kind = iota // group absent: groupadd + create folder
	RefuseGroup                // gid mismatch or guard violation (manual)
	OrphanGroup                // managed group not in roster: report only, never deleted
	SetGroupAdmins             // declared owners differ from /etc/gshadow: gpasswd -A
	// --- users ---
	CreateUser         // active, absent: full create + SMB enable
	CreateUserDisabled // disabled, absent: create locked + SMB disabled
	UpdateUserGroups   // supplementary groups differ: usermod -G (replace)
	AddSmb             // user present but no SMB account: smbpasswd -a + enable
	EnableUser         // SMB disabled but should be active: smbpasswd -e
	DisableUser        // SMB active but should be off: smbpasswd -d
	EnsureHome         // present user whose home directory is missing: (re)create it
	RefuseUser         // uid mismatch or guard violation (manual)
	OrphanUser         // managed user not in roster: auto-disable (home kept)
	ReservedPresent    // reserved status but the account still exists: standing notice
)

// Class groups kinds by how a report and the idempotency check treat them.
type Class int

const (
	// Change: apply performs a mutation. Idempotency means zero Change actions.
	Change Class = iota
	// Refuse: needs manual intervention; makes apply exit non-zero.
	Refuse
	// Notice: informational (orphan), no mutation.
	Notice
)

// Class reports how the kind is classified.
func (k Kind) Class() Class {
	switch k {
	case RefuseGroup, RefuseUser:
		return Refuse
	case OrphanGroup, OrphanUser, ReservedPresent:
		return Notice
	default:
		return Change
	}
}

func (k Kind) String() string {
	switch k {
	case CreateGroup:
		return "create-group"
	case RefuseGroup:
		return "refuse-group"
	case OrphanGroup:
		return "orphan-group"
	case SetGroupAdmins:
		return "set-group-admins"
	case CreateUser:
		return "create-user"
	case CreateUserDisabled:
		return "create-user-disabled"
	case UpdateUserGroups:
		return "update-user-groups"
	case AddSmb:
		return "add-smb"
	case EnableUser:
		return "enable-user"
	case DisableUser:
		return "disable-user"
	case EnsureHome:
		return "ensure-home"
	case RefuseUser:
		return "refuse-user"
	case OrphanUser:
		return "orphan-user"
	case ReservedPresent:
		return "reserved-present"
	default:
		return "unknown"
	}
}

// Action is a single reconcile step. It carries enough for both report
// rendering and provider/samba dispatch, but nothing config-specific (home
// paths and shell are supplied by the executor).
type Action struct {
	Kind     Kind
	Name     string
	UID      uint32
	GID      uint32
	FullName string
	Groups   []string // desired supplementary groups (create / update)
	Status   roster.Status
	HasSmb   bool   // create*: an SMB account already exists — do not reset its password
	Reason   string // for refuse / orphan / status context
}

// Reconcile computes the actions to converge actual to desired. The classifier
// is a defense-in-depth guard: any entry that is not Managed is refused even if
// the loader should have filtered it. The output is deterministic (groups then
// users, each sorted by name, then orphans sorted by name).
func Reconcile(desired *roster.Roster, actual *state.State, cls *idrange.Classifier) []Action {
	var out []Action

	desiredGroups := append([]roster.Group(nil), desired.Groups...)
	sort.Slice(desiredGroups, func(i, j int) bool { return desiredGroups[i].Name < desiredGroups[j].Name })
	desiredUsers := append([]roster.User(nil), desired.Users...)
	sort.Slice(desiredUsers, func(i, j int) bool { return desiredUsers[i].Name < desiredUsers[j].Name })

	desiredGroupNames := map[string]bool{}
	desiredUserNames := map[string]bool{}
	for _, g := range desiredGroups {
		desiredGroupNames[g.Name] = true
	}
	for _, u := range desiredUsers {
		desiredUserNames[u.Name] = true
	}

	// Group-administrator actions, held back until after the users exist.
	var ownerActions []Action

	// Reverse indexes to detect a desired id already held by a DIFFERENT name
	// (would make the create's useradd/groupadd fail cryptically).
	uidOwner := map[uint32]string{}
	for n, u := range actual.Users {
		uidOwner[u.UID] = n
	}
	gidOwner := map[uint32]string{}
	for n, g := range actual.Groups {
		gidOwner[g.GID] = n
	}

	// --- groups ---
	for _, g := range desiredGroups {
		if cls.GID(g.GID) != idrange.Managed {
			out = append(out, Action{Kind: RefuseGroup, Name: g.Name, GID: g.GID, Reason: "gid not in managed range"})
			continue
		}
		cur, ok := actual.Groups[g.Name]
		switch {
		case !ok:
			if _, exists := actual.AllGroups[g.Name]; exists {
				out = append(out, Action{Kind: RefuseGroup, Name: g.Name, GID: g.GID,
					Reason: "a group with this name already exists outside the managed range — reconcile it manually"})
				break
			}
			if owner, held := gidOwner[g.GID]; held && owner != g.Name {
				out = append(out, Action{Kind: RefuseGroup, Name: g.Name, GID: g.GID,
					Reason: reasonf("gid %d already held by group %q", g.GID, owner)})
				break
			}
			out = append(out, Action{Kind: CreateGroup, Name: g.Name, GID: g.GID, Reason: g.Description})
		case cur.GID != g.GID:
			out = append(out, Action{Kind: RefuseGroup, Name: g.Name, GID: g.GID,
				Reason: reasonf("gid %d desired, %d actual — change is manual", g.GID, cur.GID)})
		case folderDrifted(g, cur):
			// group exists but its folder is missing or its perms/owner drifted
			// (or a partial create): CreateGroup's dispatch is idempotent for the
			// group and re-ensures the folder to 2770 setgid.
			out = append(out, Action{Kind: CreateGroup, Name: g.Name, GID: g.GID, Reason: "group folder missing or wrong perms"})
		}
		// Owners are compared separately from the create/folder cases above,
		// because a group can be entirely correct and still have the wrong
		// administrators — and because the answer is "unknown" on a backend with
		// no gshadow, which must not read as "they are all missing".
		//
		// A group about to be created has no administrators yet, so declared
		// owners are drift by definition.
		//
		// COLLECTED, not appended: `gpasswd -A` needs both the group AND every
		// named user to exist, and the users are created by the loop below. On a
		// fresh system, emitting these here fails with "user does not exist" and
		// the delegation lands only if someone runs apply a second time.
		switch {
		case !ok && len(g.Owners) > 0:
			ownerActions = append(ownerActions, Action{Kind: SetGroupAdmins, Name: g.Name, GID: g.GID,
				Groups: slices.Clone(g.Owners),
				Reason: reasonf("owners %v declared on a new group", g.Owners)})
		case ok && cur.AdminsKnown && adminsDrifted(g, cur):
			ownerActions = append(ownerActions, Action{Kind: SetGroupAdmins, Name: g.Name, GID: g.GID,
				Groups: slices.Clone(g.Owners),
				Reason: reasonf("owners %v declared, %v actual", g.Owners, cur.Admins)})
		}
		// present, gid matches, folder correct, owners agree => no-op.
	}

	// --- users ---
	for _, u := range desiredUsers {
		out = append(out, reconcileUser(u, actual, cls, uidOwner, gidOwner)...)
	}

	// --- group administrators (after the users they name) ---
	out = append(out, ownerActions...)

	// --- orphan groups (managed, present, not desired) ---
	for _, name := range sortedGroupNames(actual.Groups) {
		if desiredGroupNames[name] {
			continue
		}
		g := actual.Groups[name]
		if cls.GID(g.GID) != idrange.Managed {
			continue // out of scope / protected: not ours to notice
		}
		out = append(out, Action{Kind: OrphanGroup, Name: name, GID: g.GID, Reason: "absent from roster; not deleted"})
	}

	// --- orphan users (managed, present, not desired) => auto-disable ---
	for _, name := range sortedUserNames(actual.Users) {
		if desiredUserNames[name] {
			continue
		}
		au := actual.Users[name]
		if cls.UID(au.UID) != idrange.Managed {
			continue
		}
		// Always surface an orphan as a standing Notice (report-only, so it is safe
		// even for an odd scanned name) — a hand-created account must not be a
		// monitoring blind spot. Only DISABLE (an exec) when the name is a valid
		// account name; a name like "-x" must never reach an exec argument.
		out = append(out, Action{Kind: OrphanUser, Name: name, UID: au.UID,
			Reason: "absent from roster; home kept. Prefer status: disabled/reserved to keep uid reserved"})
		if roster.ValidName(name) {
			if sm, ok := actual.Smb[name]; ok && sm.Enabled {
				out = append(out, Action{Kind: DisableUser, Name: name, UID: au.UID, Reason: "orphan: absent from roster"})
			}
		}
	}

	return out
}

func reconcileUser(u roster.User, actual *state.State, cls *idrange.Classifier, uidOwner, gidOwner map[uint32]string) []Action {
	if cls.UID(u.UID) != idrange.Managed {
		return []Action{{Kind: RefuseUser, Name: u.Name, UID: u.UID, Status: u.Status, Reason: "uid not in managed range"}}
	}
	cur, present := actual.Users[u.Name]
	sm, hasSmb := actual.Smb[u.Name]

	// uid mismatch is refused regardless of status.
	if present && cur.UID != u.UID {
		return []Action{{Kind: RefuseUser, Name: u.Name, UID: u.UID, Status: u.Status,
			Reason: reasonf("uid %d desired, %d actual — change is manual", u.UID, cur.UID)}}
	}

	// Collisions on the create path. Refuse instead of silently mutating someone
	// else's account or letting useradd/groupadd fail cryptically.
	if !present && u.Status != roster.Reserved {
		// A pre-existing account with this name but an out-of-range uid is hidden
		// from managed Users, so `present` is false — a create would land on it
		// (usermod -G / -L on the wrong account). Refuse.
		if _, exists := actual.AllUsers[u.Name]; exists {
			return []Action{{Kind: RefuseUser, Name: u.Name, UID: u.UID, Status: u.Status,
				Reason: "an account with this name already exists outside the managed range — reconcile it manually"}}
		}
		// A pre-existing group with this name whose gid != uid can't be the UPG.
		if gid, exists := actual.AllGroups[u.Name]; exists && gid != u.UID {
			return []Action{{Kind: RefuseUser, Name: u.Name, UID: u.UID, Status: u.Status,
				Reason: reasonf("a group named %q (gid %d) already exists; the UPG needs gid == uid %d", u.Name, gid, u.UID)}}
		}
		if owner, ok := uidOwner[u.UID]; ok && owner != u.Name {
			return []Action{{Kind: RefuseUser, Name: u.Name, UID: u.UID, Status: u.Status,
				Reason: reasonf("uid %d already held by %q — reserve or purge it before reusing", u.UID, owner)}}
		}
		if owner, ok := gidOwner[u.UID]; ok && owner != u.Name {
			return []Action{{Kind: RefuseUser, Name: u.Name, UID: u.UID, Status: u.Status,
				Reason: reasonf("UPG gid %d already held by group %q", u.UID, owner)}}
		}
	}

	switch u.Status {
	case roster.Reserved:
		// Desired: no managed account. Never create, never delete. If an account
		// lingers, emit a standing Notice so the operator keeps seeing it; if its
		// SMB is still enabled, also disable it.
		if !present {
			return nil
		}
		out := []Action{{Kind: ReservedPresent, Name: u.Name, UID: u.UID, Status: u.Status,
			Reason: "reserved: account present; purge to fully remove"}}
		if hasSmb && sm.Enabled {
			out = append(out, Action{Kind: DisableUser, Name: u.Name, UID: u.UID, Status: u.Status, Reason: "status: reserved"})
		}
		return out

	case roster.Disabled:
		if !present {
			return []Action{{Kind: CreateUserDisabled, Name: u.Name, UID: u.UID, FullName: u.FullName,
				Groups: u.Groups, Status: u.Status, HasSmb: hasSmb, Reason: "disabled"}}
		}
		var out []Action
		if !sameGroupSet(u.Groups, cur.Groups) {
			out = append(out, Action{Kind: UpdateUserGroups, Name: u.Name, UID: u.UID, Groups: mergeGroups(u.Groups, cur.ExtraGroups), Status: u.Status})
		}
		if homeDrifted(u, cur) {
			out = append(out, Action{Kind: EnsureHome, Name: u.Name, UID: u.UID, Status: u.Status, Reason: "home directory missing or wrong perms"})
		}
		if hasSmb && sm.Enabled {
			out = append(out, Action{Kind: DisableUser, Name: u.Name, UID: u.UID, Status: u.Status, Reason: "status: disabled"})
		}
		return out

	default: // Active
		if !present {
			return []Action{{Kind: CreateUser, Name: u.Name, UID: u.UID, FullName: u.FullName,
				Groups: u.Groups, Status: u.Status, HasSmb: hasSmb}}
		}
		var out []Action
		if !sameGroupSet(u.Groups, cur.Groups) {
			out = append(out, Action{Kind: UpdateUserGroups, Name: u.Name, UID: u.UID, Groups: mergeGroups(u.Groups, cur.ExtraGroups), Status: u.Status})
		}
		if homeDrifted(u, cur) {
			out = append(out, Action{Kind: EnsureHome, Name: u.Name, UID: u.UID, Status: u.Status, Reason: "home directory missing or wrong perms"})
		}
		switch {
		case !hasSmb:
			out = append(out, Action{Kind: AddSmb, Name: u.Name, UID: u.UID, Status: u.Status, Reason: "no SMB account"})
		case !sm.Enabled:
			out = append(out, Action{Kind: EnableUser, Name: u.Name, UID: u.UID, Status: u.Status, Reason: "re-activate"})
		}
		return out
	}
}

// sameGroupSet compares two supplementary-group lists as sets (order/dup-insensitive).
func sameGroupSet(a, b []string) bool {
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	as = dedupSorted(as)
	bs = dedupSorted(bs)
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func dedupSorted(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

func sortedGroupNames(m map[string]state.Group) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func sortedUserNames(m map[string]state.User) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// adminsDrifted compares the declared owners with what gshadow holds.
//
// Order-insensitive: gpasswd writes them sorted, but a hand edit need not, and
// re-running gpasswd because someone typed the same two names the other way
// round would make `plan` permanently dirty.
func adminsDrifted(g roster.Group, cur state.Group) bool {
	want := slices.Clone(g.Owners)
	have := slices.Clone(cur.Admins)
	slices.Sort(want)
	slices.Sort(have)
	return !slices.Equal(want, have)
}
