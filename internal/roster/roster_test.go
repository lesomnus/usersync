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
users:
  - name: skim
    uid: 3001
    full_name: Sunghyun Kim
    groups: [team-a]
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

func TestUndefinedGroupRejected(t *testing.T) {
	y := "users:\n  - name: a\n    uid: 3001\n    groups: [ghost]\n"
	ro, _ := Load(strings.NewReader(y))
	if _, err := ro.Validate(testClassifier(), PolicyError); err == nil {
		t.Fatal("expected undefined group rejection")
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
	if len(ro.Users) != 1 || ro.Users[0].Name != "ok" {
		t.Fatalf("kept users = %v, want [ok]", ro.Users)
	}
}
