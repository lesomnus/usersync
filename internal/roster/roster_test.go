package roster

import (
	"bytes"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/usersync/internal/idrange"
)

func testClassifier() *idrange.Classifier {
	return idrange.New(idrange.Config{
		SystemFloor: 1000,
		UID:         idrange.Set{Manage: idrange.Range{Min: 3000, Max: 6999}},
		GID:         idrange.Set{Manage: idrange.Range{Min: 7000, Max: 7999}},
	})
}

const sampleYAML = `groups:
  - name: team-a
    gid: 7001
    description: Perception team
    members: [skim]
users:
  - name: skim
    uid: 3001
    full_name: Sunghyun Kim
  - name: park
    uid: 3004
    status: disabled
  - name: oldhand
    uid: 3005
    status: reserved
`

func TestLoadAndValidate(t *testing.T) {
	ro, err := Load(strings.NewReader(sampleYAML))
	if err != nil {
		t.Fatal(err)
	}
	skipped, err := ro.Validate(testClassifier(), PolicyError)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Errorf("unexpected skipped: %v", skipped)
	}
	if len(ro.Users) != 3 || len(ro.Groups) != 1 {
		t.Fatalf("got %d users, %d groups", len(ro.Users), len(ro.Groups))
	}
	byName := map[string]User{}
	for _, u := range ro.Users {
		byName[u.Name] = u
	}
	if byName["park"].Status != Disabled {
		t.Errorf("park status = %v, want disabled", byName["park"].Status)
	}
	if byName["oldhand"].Status != Reserved {
		t.Errorf("oldhand status = %v, want reserved", byName["oldhand"].Status)
	}
	if byName["skim"].Status != Active {
		t.Errorf("skim status = %v, want active (default)", byName["skim"].Status)
	}
}

func TestStatusRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		s    Status
		want string
	}{{Active, "active"}, {Disabled, "disabled"}, {Reserved, "reserved"}} {
		b, err := yaml.Marshal(tc.s)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(b)); got != tc.want {
			t.Errorf("Marshal(%v) = %q, want %q", tc.s, got, tc.want)
		}
		var back Status
		if err := yaml.Unmarshal([]byte(tc.want), &back); err != nil {
			t.Fatal(err)
		}
		if back != tc.s {
			t.Errorf("Unmarshal(%q) = %v, want %v", tc.want, back, tc.s)
		}
	}
}

func TestActiveStatusOmittedOnEncode(t *testing.T) {
	// An active user (zero status) should not emit a status key.
	ro := &Roster{Users: []User{{Name: "skim", UID: 3001}}}
	var buf bytes.Buffer
	if err := yaml.NewEncoder(&buf).Encode(ro); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "status") {
		t.Errorf("active user should omit status, got:\n%s", buf.String())
	}
}

func TestStrictRejectsUnknownKey(t *testing.T) {
	_, err := Load(strings.NewReader("users:\n  - name: x\n    uid: 3001\n    xyz: 1\n"))
	if err == nil {
		t.Fatal("expected strict decode to reject unknown key")
	}
}

func TestInvalidStatusRejected(t *testing.T) {
	_, err := Load(strings.NewReader("users:\n  - name: x\n    uid: 3001\n    status: frozen\n"))
	if err == nil {
		t.Fatal("expected invalid status to be rejected")
	}
}

func TestDuplicateUIDRejected(t *testing.T) {
	// Reuse guard: two users cannot share a uid (even across statuses).
	y := "users:\n  - name: a\n    uid: 3001\n  - name: b\n    uid: 3001\n    status: reserved\n"
	ro, err := Load(strings.NewReader(y))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.Validate(testClassifier(), PolicyError); err == nil {
		t.Fatal("expected duplicate uid to be rejected")
	} else if !strings.Contains(err.Error(), "duplicate uid") {
		t.Errorf("error = %v, want duplicate uid", err)
	}
}

func TestDuplicateNameRejected(t *testing.T) {
	y := "users:\n  - name: a\n    uid: 3001\n  - name: a\n    uid: 3002\n"
	ro, _ := Load(strings.NewReader(y))
	if _, err := ro.Validate(testClassifier(), PolicyError); err == nil {
		t.Fatal("expected duplicate name rejection")
	}
}

func TestUndefinedMemberRejected(t *testing.T) {
	// A group listing a member who is not a declared user is a refusal to load —
	// the "이름이 없으면 실패" rule, symmetric to an undeclared owner.
	y := "groups:\n  - name: team-a\n    gid: 7001\n    members: [ghost]\nusers:\n  - name: a\n    uid: 3001\n"
	ro, err := Load(strings.NewReader(y))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.Validate(testClassifier(), PolicyError); err == nil {
		t.Fatal("expected a member that is not a declared user to be rejected")
	}
}

func TestFullNameSeparatorsRejected(t *testing.T) {
	for _, bad := range []string{"Kim, S", "a:b"} {
		ro := &Roster{Users: []User{{Name: "a", UID: 3001, FullName: bad}}}
		if _, err := ro.Validate(testClassifier(), PolicyError); err == nil {
			t.Errorf("full_name %q should be rejected", bad)
		}
	}
}

func TestProtectedAlwaysRejected(t *testing.T) {
	// A uid below the floor is protected; skip policy must NOT rescue it.
	ro := &Roster{Users: []User{{Name: "sys", UID: 500}}}
	if _, err := ro.Validate(testClassifier(), PolicySkip); err == nil {
		t.Fatal("protected uid must be a hard error even with PolicySkip")
	}
}

func TestOutOfScopeErrorVsSkip(t *testing.T) {
	mk := func() *Roster {
		return &Roster{Users: []User{
			{Name: "ok", UID: 3001},
			{Name: "legacy", UID: 2000}, // out of manage scope
		}}
	}
	// PolicyError: whole roster refused.
	if _, err := mk().Validate(testClassifier(), PolicyError); err == nil {
		t.Fatal("out-of-scope with PolicyError must fail")
	}
	// PolicySkip: legacy dropped + reported, ok kept.
	ro := mk()
	skipped, err := ro.Validate(testClassifier(), PolicySkip)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0].Name != "legacy" {
		t.Fatalf("skipped = %v, want [legacy]", skipped)
	}
	// Validate REPORTS; it does not edit. The distinction is load-bearing: a
	// caller that validates and then writes the file back would otherwise erase
	// every out-of-scope entry from disk, and an erased entry is a released uid
	// reservation -- the one failure this file exists to prevent.
	if len(ro.Users) != 2 {
		t.Fatalf("Validate modified the roster: users = %v, want both still present", ro.Users)
	}
	// Managed is where the narrowing happens, and it works on a copy.
	m := ro.Managed(testClassifier())
	if len(m.Users) != 1 || m.Users[0].Name != "ok" {
		t.Fatalf("Managed users = %v, want [ok]", m.Users)
	}
	if len(ro.Users) != 2 {
		t.Fatalf("Managed modified its receiver: users = %v", ro.Users)
	}
}

func TestInvalidNamesRejected(t *testing.T) {
	// A LEADING dot is still refused even though a dot is now allowed inside a
	// name: `useradd .` and a `[.]` share are both nonsense, and `..` would name
	// a directory that is not the one meant.
	for _, bad := range []string{"Team A", "1skim", "UPPER", "-x", "a/b", "x[y]", "a b", "x\ny", "", ".hidden", ".."} {
		ro := &Roster{Users: []User{{Name: bad, UID: 3001}}}
		if _, err := ro.Validate(testClassifier(), PolicyError); err == nil {
			t.Errorf("user name %q must be rejected", bad)
		}
		rg := &Roster{Groups: []Group{{Name: bad, GID: 7001}}}
		if _, err := rg.Validate(testClassifier(), PolicyError); err == nil {
			t.Errorf("group name %q must be rejected", bad)
		}
	}
	// valid names pass the name check.
	ok := &Roster{Groups: []Group{{Name: "team-a", GID: 7001}}, Users: []User{{Name: "s_kim", UID: 3001}}}
	if _, err := ok.Validate(testClassifier(), PolicyError); err != nil {
		t.Errorf("valid names must pass: %v", err)
	}
}

// `firstname.lastname` is what most organisations call people, and it is what a
// directory service hands over when one arrives.
//
// The dot was excluded on smb.conf-injection grounds that do not survive
// checking: a dot cannot close a `[...]` section — only a newline can, and that
// is refused separately (TestNewlineInjectionRejected) as well as by
// shadow-utils itself. Verified on Debian trixie / Samba 4.x: `groupadd team.a`
// succeeds, `[team.a]` passes testparm, the share mounts, and a file written
// into it comes out group `team.a` with setgid intact.
func TestDottedNamesAccepted(t *testing.T) {
	for _, ok := range []string{"minseok.yang", "seunghyun.hwang", "team.a", "a.b.c"} {
		ru := &Roster{Users: []User{{Name: ok, UID: 3001}}}
		if _, err := ru.Validate(testClassifier(), PolicyError); err != nil {
			t.Errorf("user name %q must be accepted: %v", ok, err)
		}
		rg := &Roster{Groups: []Group{{Name: ok, GID: 7001}}}
		if _, err := rg.Validate(testClassifier(), PolicyError); err != nil {
			t.Errorf("group name %q must be accepted: %v", ok, err)
		}
	}
}

func TestNewlineInjectionRejected(t *testing.T) {
	// A group description with a newline could inject an smb.conf directive.
	rg := &Roster{Groups: []Group{{Name: "team-a", GID: 7001, Description: "x\n[public]\n path = /"}}}
	if _, err := rg.Validate(testClassifier(), PolicyError); err == nil {
		t.Error("group description with a newline must be rejected")
	}
	// full_name with a newline likewise.
	ru := &Roster{Users: []User{{Name: "skim", UID: 3001, FullName: "a\nb"}}}
	if _, err := ru.Validate(testClassifier(), PolicyError); err == nil {
		t.Error("full_name with a newline must be rejected")
	}
}

func TestReservedSmbSectionNameRejected(t *testing.T) {
	for _, bad := range []string{"global", "homes", "printers"} {
		ro := &Roster{Groups: []Group{{Name: bad, GID: 7001}}}
		if _, err := ro.Validate(testClassifier(), PolicyError); err == nil {
			t.Errorf("group named %q (reserved Samba section) must be rejected", bad)
		}
	}
}

// An owner is a delegation, so it has to name someone this roster knows. A name
// that is not declared here would be written into gshadow as something the
// system may not resolve, and `gpasswd -A` fails at apply — by which point the
// run is half done.
func TestOwnersMustBeDeclaredUsers(t *testing.T) {
	mk := func(owners ...string) *Roster {
		return &Roster{
			Groups: []Group{{Name: "team-a", GID: 7001, Owners: owners}},
			Users:  []User{{Name: "alice", UID: 3001, Groups: []string{"team-a"}}},
		}
	}

	if _, err := mk("alice").Validate(testClassifier(), PolicyError); err != nil {
		t.Fatalf("a declared user must be an acceptable owner: %v", err)
	}
	// An owner need NOT be a member: a manager who is not on the team is a real
	// arrangement, and gshadow does not require membership either.
	ro := &Roster{
		Groups: []Group{{Name: "team-a", GID: 7001, Owners: []string{"boss"}}},
		Users:  []User{{Name: "boss", UID: 3002}},
	}
	if _, err := ro.Validate(testClassifier(), PolicyError); err != nil {
		t.Errorf("an owner who is not a member must be allowed: %v", err)
	}

	for _, bad := range [][]string{
		{"nobody"},         // not in this roster
		{"alice", "alice"}, // listed twice
		{"Bad Name"},       // not a valid account name
	} {
		if _, err := mk(bad...).Validate(testClassifier(), PolicyError); err == nil {
			t.Errorf("owners %v were accepted", bad)
		}
	}
}

func TestReadersMustBeDeclaredGroups(t *testing.T) {
	ro := &Roster{
		Groups: []Group{{Name: "perception", GID: 7001, Readers: []string{"ghost-ro"}}},
	}
	if _, err := ro.Validate(testClassifier(), PolicyError); err == nil {
		t.Fatal("a reader naming an undeclared group was accepted")
	}
}

func TestAnonymousParse(t *testing.T) {
	for in, want := range map[string]Anonymous{
		"": AnonNone, "none": AnonNone, "read": AnonRead, "write": AnonWrite,
	} {
		got, err := ParseAnonymous(in)
		if err != nil || got != want {
			t.Errorf("ParseAnonymous(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseAnonymous("public"); err == nil {
		t.Error("an invalid anonymous level was accepted")
	}
	for a, perm := range map[Anonymous]uint32{AnonNone: 0o2770, AnonRead: 0o2775, AnonWrite: 0o2777} {
		if a.FolderPerm() != perm {
			t.Errorf("%s.FolderPerm() = %04o, want %04o", a, a.FolderPerm(), perm)
		}
	}
}

func TestAnonymousAndReadersAreExclusive(t *testing.T) {
	ro := &Roster{Groups: []Group{
		{Name: "pub", GID: 7001, Anonymous: AnonRead, Readers: []string{"pub-ro"}},
		{Name: "pub-ro", GID: 7011},
	}}
	if _, err := ro.Validate(testClassifier(), PolicyError); err == nil {
		t.Fatal("an anonymous group that also lists readers was accepted")
	}
	// Anonymous alone is fine.
	ok := &Roster{Groups: []Group{{Name: "pub", GID: 7001, Anonymous: AnonWrite}}}
	if _, err := ok.Validate(testClassifier(), PolicyError); err != nil {
		t.Fatalf("a plain anonymous group was rejected: %v", err)
	}
}

func TestReadersRejectSelfAndDuplicates(t *testing.T) {
	self := &Roster{Groups: []Group{{Name: "perception", GID: 7001, Readers: []string{"perception"}}}}
	if _, err := self.Validate(testClassifier(), PolicyError); err == nil {
		t.Error("a group listing itself as a reader was accepted")
	}
	dup := &Roster{Groups: []Group{
		{Name: "perception", GID: 7001, Readers: []string{"ro", "ro"}},
		{Name: "ro", GID: 7011},
	}}
	if _, err := dup.Validate(testClassifier(), PolicyError); err == nil {
		t.Error("a duplicate reader was accepted")
	}
}

func TestReadersAcceptedWhenDeclared(t *testing.T) {
	ro := &Roster{Groups: []Group{
		{Name: "perception", GID: 7001, Readers: []string{"perception-ro"}},
		{Name: "perception-ro", GID: 7011},
	}}
	if _, err := ro.Validate(testClassifier(), PolicyError); err != nil {
		t.Fatalf("a valid reader declaration was rejected: %v", err)
	}
}
