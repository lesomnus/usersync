package smbconf

import (
	"strconv"
	"strings"
	"testing"

	"github.com/lesomnus/usersync/internal/roster"
)

func groups() []roster.Group {
	return []roster.Group{
		{Name: "team-b", GID: 10002, Description: "Planning team"},
		{Name: "team-a", GID: 10001},
	}
}

func TestRender(t *testing.T) {
	out := Render(groups(), "/research/home", "/research/groups")
	for _, want := range []string{
		BeginMarker, EndMarker,
		"[homes]", "valid users = %S",
		"[team-a]", "path = /research/groups/team-a", "valid users = @team-a",
		"force group = team-a", "directory mask = 2770",
		"[team-b]", "comment = Planning team", "path = /research/groups/team-b",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered block missing %q\n%s", want, out)
		}
	}
	// deterministic name order: team-a before team-b.
	if strings.Index(out, "[team-a]") > strings.Index(out, "[team-b]") {
		t.Error("sections must be sorted by name")
	}
	// default comment when none given.
	if !strings.Contains(out, "comment = team-a shared") {
		t.Error("team-a should get the default comment")
	}
}

// sections parses the rendered block into section -> key -> value so a test can
// reason about the emitted directives rather than string-matching them.
func sections(block string) map[string]map[string]string {
	out := map[string]map[string]string{}
	cur := ""
	for _, ln := range strings.Split(block, "\n") {
		s := strings.TrimSpace(ln)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
			cur = strings.Trim(s, "[]")
			out[cur] = map[string]string{}
			continue
		}
		k, v, ok := strings.Cut(s, "=")
		if !ok || cur == "" {
			continue
		}
		out[cur][strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// Samba's mode arithmetic for a newly created object:
//
//	mode = (base & mask) | force
//
// SMB carries no unix mode, so "base" is not a client request — it is what smbd
// derives from the DOS attributes in unix_mode(). These are the observed values,
// not guesses; scripts/verify-samba-modes.sh reproduces them against a real
// smbd, and a share with no mask at all lands on 0744/2755, which is only
// consistent with these bases and the 0744/0755 defaults.
const (
	baseFile    = 0o666 // rw for all three classes
	baseFileArc = 0o766 // ...plus the archive attribute mapped onto owner-execute
	baseDir     = 0o777
)

func sambaMode(base, mask, force uint32) uint32 { return base&mask | force }

// octalOr returns a directive's value, or dflt when the directive is absent.
// `force create mode` and `force directory mode` both default to 0000.
func octalOr(t *testing.T, sec map[string]string, key string, dflt uint32) uint32 {
	t.Helper()
	if _, ok := sec[key]; !ok {
		return dflt
	}
	return octal(t, sec, key)
}

func octal(t *testing.T, sec map[string]string, key string) uint32 {
	t.Helper()
	v, ok := sec[key]
	if !ok {
		t.Fatalf("missing directive %q in %v", key, sec)
	}
	n, err := strconv.ParseUint(v, 8, 32)
	if err != nil {
		t.Fatalf("directive %q = %q is not octal: %v", key, v, err)
	}
	return uint32(n)
}

// The generated directives must land every new object on one known mode, for
// every client, starting from the bases smbd actually uses.
func TestGeneratedModesAreExact(t *testing.T) {
	secs := sections(Render(groups(), "/research/home", "/research/groups"))

	for _, tt := range []struct {
		section              string
		wantFile, wantFolder uint32
	}{
		{"homes", 0o600, 0o700},   // private: no group or other bits, ever
		{"team-a", 0o660, 0o2770}, // shared: group-writable, setgid, no other bits
	} {
		sec, ok := secs[tt.section]
		if !ok {
			t.Fatalf("rendered block has no [%s] section", tt.section)
		}
		fMask := octal(t, sec, "create mask")
		fForce := octalOr(t, sec, "force create mode", 0)
		dMask := octal(t, sec, "directory mask")
		dForce := octalOr(t, sec, "force directory mode", 0)

		for _, base := range []uint32{baseFile, baseFileArc} {
			if got := sambaMode(base, fMask, fForce); got != tt.wantFile {
				t.Errorf("[%s] file from base %04o = %04o, want %04o", tt.section, base, got, tt.wantFile)
			}
		}
		if got := sambaMode(baseDir, dMask, dForce); got != tt.wantFolder {
			t.Errorf("[%s] folder from base %04o = %04o, want %04o", tt.section, baseDir, got, tt.wantFolder)
		}
	}

	// Spelled out as invariants, so a future edit that loosens one fails here
	// with the reason rather than just a changed number.
	team := secs["team-a"]
	if m := octal(t, team, "create mask"); m&0o060 != 0o060 {
		t.Errorf("team files must stay group-writable, got create mask %04o", m)
	}
	if m := octal(t, team, "create mask"); m&0o007 != 0 {
		t.Errorf("team files must not be reachable by other, got create mask %04o", m)
	}
	home := secs["homes"]
	if m := octal(t, home, "create mask"); m&0o077 != 0 {
		t.Errorf("home files must be owner-only, got create mask %04o", m)
	}
	if m := octal(t, home, "directory mask"); m&0o077 != 0 {
		t.Errorf("home folders must be owner-only, got directory mask %04o", m)
	}
}

// The one thing a mask cannot do is put a bit back. If the parent directory ever
// loses its setgid bit, the kernel has nothing to propagate, so every folder
// created beneath it over SMB comes out without setgid too — and files created
// inside those take the creator's own group rather than the team's, which is the
// teammate-cannot-write failure one level down, invisible from the client.
//
// `force directory mode` is the only directive here that repairs that, and this
// pins the reason it exists. Verified against a real smbd with a deliberately
// un-setgid parent: mask alone yields 0770, with the force it is 2770
// (scripts/verify-samba-modes.sh).
func TestForceDirectoryModeRestoresSetgid(t *testing.T) {
	team := sections(Render(groups(), "/research/home", "/research/groups"))["team-a"]
	mask := octal(t, team, "directory mask")
	force := octalOr(t, team, "force directory mode", 0)

	const parentLostSetgid = 0o777 // whatever smbd derives, with no setgid to inherit

	if got := sambaMode(parentLostSetgid, mask, 0); got&0o2000 != 0 {
		t.Fatalf("test premise is wrong: the mask alone already yields setgid (%04o)", got)
	}
	if got := sambaMode(parentLostSetgid, mask, force); got&0o2000 == 0 {
		t.Errorf("a new team folder must get setgid even when the parent lost it, got %04o", got)
	}
}

// `force create mode` is deliberately absent. It changes nothing under this
// configuration — the file base is 0666, so `create mask = 0660` already yields
// 0660 — and it would override the DOS-attribute mapping in a configuration that
// maps read-only onto the permission bits. A directive that does nothing is a
// directive that misleads whoever reads the config next.
func TestNoForceCreateMode(t *testing.T) {
	for name, sec := range sections(Render(groups(), "/research/home", "/research/groups")) {
		if v, ok := sec["force create mode"]; ok {
			t.Errorf("[%s] sets force create mode = %s; the mask alone already pins the file mode", name, v)
		}
	}
}

func TestSpliceAppendsWhenAbsent(t *testing.T) {
	existing := "[global]\n   workgroup = WORKGROUP\n"
	block := Render(groups(), "/h", "/g")
	out := Splice(existing, block)
	if !strings.HasPrefix(out, existing) {
		t.Error("existing content must be preserved at the top")
	}
	if !strings.Contains(out, BeginMarker) || !strings.Contains(out, EndMarker) {
		t.Error("block must be appended")
	}
}

func TestSpliceReplacesAndIsIdempotent(t *testing.T) {
	existing := "[global]\n   workgroup = WORKGROUP\n"
	first := Splice(existing, Render(groups()[:1], "/h", "/g")) // only team-b
	second := Splice(first, Render(groups(), "/h", "/g"))       // now team-a + team-b

	// The [global] section survives, and there is exactly one managed block.
	if strings.Count(second, BeginMarker) != 1 || strings.Count(second, EndMarker) != 1 {
		t.Fatalf("must keep exactly one managed block:\n%s", second)
	}
	if !strings.Contains(second, "[global]") {
		t.Error("content outside the block must be preserved")
	}
	if !strings.Contains(second, "[team-a]") {
		t.Error("replacement must reflect the new groups")
	}
	// Idempotent: splicing the same block again changes nothing.
	third := Splice(second, Render(groups(), "/h", "/g"))
	if third != second {
		t.Error("splicing the same block must be idempotent")
	}
}

func TestSpliceRemovesStaleShare(t *testing.T) {
	// A group dropped from the roster must disappear from the managed block.
	with := Splice("[global]\n", Render(groups(), "/h", "/g"))
	without := Splice(with, Render(groups()[1:], "/h", "/g")) // only team-a
	if strings.Contains(without, "[team-b]") {
		t.Error("dropped group's share must be removed on re-splice")
	}
	if !strings.Contains(without, "[team-a]") {
		t.Error("remaining group must stay")
	}
}

func TestRenderStripsNewlines(t *testing.T) {
	// Defense-in-depth: even a newline-bearing name/description must not create a
	// new section/directive in the rendered block.
	g := []roster.Group{{Name: "team-a", GID: 10001, Description: "x\n[public]\n   path = /\n   guest ok = yes"}}
	out := Render(g, "/research/home", "/research/groups")
	// No injected section header or directive may appear at the start of a line;
	// the payload must be collapsed onto the single `comment =` line.
	for _, ln := range strings.Split(out, "\n") {
		s := strings.TrimSpace(ln)
		if strings.HasPrefix(s, "[public]") || strings.HasPrefix(s, "guest ok") {
			t.Fatalf("newline injection leaked a directive onto its own line:\n%s", out)
		}
	}
}
