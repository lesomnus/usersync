package idexport

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"

	"github.com/lesomnus/usersync/internal/roster"
)

func sample() *roster.Roster {
	// Deliberately unsorted, and covering every status.
	return &roster.Roster{
		Groups: []roster.Group{
			{Name: "team-b", GID: 10002, Description: "Planning team"},
			{Name: "team-a", GID: 10001},
		},
		Users: []roster.User{
			{Name: "park", UID: 3004, FullName: "Minjun Park", Status: roster.Disabled},
			{Name: "skim", UID: 3001, FullName: "Sunghyun Kim", Groups: []string{"team-a"}},
			{Name: "oldhand", UID: 3005, FullName: "Retired User", Status: roster.Reserved},
		},
	}
}

func parseCSV(t *testing.T, s string) [][]string {
	t.Helper()
	rows, err := csv.NewReader(strings.NewReader(s)).ReadAll()
	if err != nil {
		t.Fatalf("output is not valid CSV: %v\n%s", err, s)
	}
	return rows
}

func TestCSV(t *testing.T) {
	var buf bytes.Buffer
	if err := CSV(&buf, sample(), "/research/home", "/usr/sbin/nologin"); err != nil {
		t.Fatalf("CSV: %v", err)
	}
	rows := parseCSV(t, buf.String())

	want := [][]string{
		CSVHeader,
		{"group", "team-a", "", "10001", "", ""},
		{"group", "team-b", "", "10002", "", ""},
		{"user", "park", "3004", "3004", "/research/home/park", "/usr/sbin/nologin"},
		{"user", "skim", "3001", "3001", "/research/home/skim", "/usr/sbin/nologin"},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d:\n%s", len(rows), len(want), buf.String())
	}
	for i := range want {
		if strings.Join(rows[i], "|") != strings.Join(want[i], "|") {
			t.Errorf("row %d = %v, want %v", i, rows[i], want[i])
		}
	}
}

// A user's private group has gid == uid. If that equality does not survive into
// the directory, every file in the home keeps a group id that resolves to
// nothing, so the two columns must always agree.
func TestCSVUserGidMirrorsUid(t *testing.T) {
	var buf bytes.Buffer
	if err := CSV(&buf, sample(), "/research/home", "/usr/sbin/nologin"); err != nil {
		t.Fatal(err)
	}
	for _, r := range parseCSV(t, buf.String())[1:] {
		if r[0] != "user" {
			continue
		}
		if r[2] != r[3] {
			t.Errorf("%s: uid_number %s != gid_number %s", r[1], r[2], r[3])
		}
	}
}

// A reserved entry has no account here and will have none in the directory, so
// there is nothing to seed. Disabled users DO have an account and a home, so
// their numbers must be carried over or their files lose their owner.
func TestReservedIsSkippedButDisabledIsNot(t *testing.T) {
	var buf bytes.Buffer
	if err := CSV(&buf, sample(), "/h", "/s"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "oldhand") {
		t.Error("a reserved tombstone has no account to seed and must not be exported")
	}
	if !strings.Contains(buf.String(), "park") {
		t.Error("a disabled user still owns files and must be exported")
	}

	var ldif bytes.Buffer
	if err := LDIF(&ldif, sample(), "DC=corp,DC=example,DC=com"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ldif.String(), "oldhand") {
		t.Error("ldif must skip reserved entries too")
	}
	if !strings.Contains(ldif.String(), "park") {
		t.Error("ldif must include disabled users")
	}
}

func TestLDIF(t *testing.T) {
	var buf bytes.Buffer
	if err := LDIF(&buf, sample(), " OU=Research,DC=corp,DC=example,DC=com "); err != nil {
		t.Fatalf("LDIF: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"dn: CN=team-a,OU=Research,DC=corp,DC=example,DC=com",
		"dn: CN=skim,OU=Research,DC=corp,DC=example,DC=com",
		"changetype: modify",
		"replace: uidNumber\nuidNumber: 3001",
		"replace: gidNumber\ngidNumber: 3001",
		"replace: gidNumber\ngidNumber: 10001",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ldif missing %q\n%s", want, out)
		}
	}
	// A group record carries only gidNumber; it must not claim a uidNumber.
	teamA := out[strings.Index(out, "dn: CN=team-a"):]
	teamA = teamA[:strings.Index(teamA, "\ndn: ")]
	if strings.Contains(teamA, "uidNumber") {
		t.Errorf("group record must not set uidNumber:\n%s", teamA)
	}
	// Multi-attribute records separate their changes with a lone "-".
	if !strings.Contains(out, "uidNumber: 3001\n-\nreplace: gidNumber") {
		t.Errorf("user record must separate the two replaces with '-':\n%s", out)
	}
}

func TestLDIFRejectsBadBaseDN(t *testing.T) {
	for name, dn := range map[string]string{
		"empty":      "",
		"whitespace": "   ",
		// The base DN is operator input written straight into the output. A newline
		// would terminate the record and let the remainder be read as further LDIF
		// directives against whatever DN the attacker chose.
		"newline":        "DC=corp\ndn: CN=Administrator,CN=Users,DC=corp",
		"carriage":       "DC=corp\rdn: CN=x",
		"control-char":   "DC=corp\x00",
		"vertical-space": "DC=corp\vDC=x",
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := LDIF(&buf, sample(), dn); err == nil {
				t.Errorf("base DN %q must be rejected, got:\n%s", dn, buf.String())
			}
		})
	}
}

// Both renderers must be deterministic so their output can be reviewed in a diff
// and re-generated without spurious churn.
func TestDeterministicOrder(t *testing.T) {
	render := func(ro *roster.Roster) (string, string) {
		var c, l bytes.Buffer
		if err := CSV(&c, ro, "/h", "/s"); err != nil {
			t.Fatal(err)
		}
		if err := LDIF(&l, ro, "DC=x"); err != nil {
			t.Fatal(err)
		}
		return c.String(), l.String()
	}

	c1, l1 := render(sample())

	// Same content, different declaration order.
	shuffled := sample()
	shuffled.Groups[0], shuffled.Groups[1] = shuffled.Groups[1], shuffled.Groups[0]
	shuffled.Users[0], shuffled.Users[2] = shuffled.Users[2], shuffled.Users[0]
	c2, l2 := render(shuffled)

	if c1 != c2 {
		t.Errorf("CSV depends on roster order:\n%s\n---\n%s", c1, c2)
	}
	if l1 != l2 {
		t.Errorf("LDIF depends on roster order:\n%s\n---\n%s", l1, l2)
	}
}

// The exporters must not mutate the roster they are handed: `export` renders the
// same value more than once in some flows, and a caller reusing it afterwards
// must see what it passed in.
func TestExportersDoNotMutateInput(t *testing.T) {
	ro := sample()
	firstGroup, firstUser := ro.Groups[0].Name, ro.Users[0].Name

	var buf bytes.Buffer
	if err := CSV(&buf, ro, "/h", "/s"); err != nil {
		t.Fatal(err)
	}
	if err := LDIF(&buf, ro, "DC=x"); err != nil {
		t.Fatal(err)
	}

	if ro.Groups[0].Name != firstGroup || ro.Users[0].Name != firstUser {
		t.Errorf("input roster was reordered: groups[0]=%s users[0]=%s, want %s/%s",
			ro.Groups[0].Name, ro.Users[0].Name, firstGroup, firstUser)
	}
}
