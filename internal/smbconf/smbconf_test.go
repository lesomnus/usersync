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
	out := Render(groups(), "/research/groups")
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

// sambaMode mirrors Samba's mode arithmetic for a newly created object:
//
//	mode = (requested & mask) | force
//
// "requested" is what the server derives from the client's DOS attributes; SMB
// itself carries no unix mode, so a Windows or macOS client has no way to ask
// for one.
func sambaMode(requested, mask, force uint32) uint32 { return (requested&mask | force) }

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

// The generated masks must pin the resulting mode exactly, for every client.
// This is the regression that motivated the force modes: with `create mask`
// alone, a Windows client (which requests 0644/0755 from DOS attributes) creates
// team files at 0644&0660 = 0640 and folders at 0755&2770 = 0750 — present and
// readable, but not group-writable, so a teammate silently gets read-only on
// everything and no client-side symptom explains why.
func TestGeneratedMasksPinTheMode(t *testing.T) {
	secs := sections(Render(groups(), "/research/groups"))

	// A spread of plausible "requested" modes: a Windows client's DOS-attribute
	// derivation, a permissive umask, a restrictive one, and the degenerate ends.
	requests := []uint32{0o000, 0o600, 0o640, 0o644, 0o664, 0o700, 0o755, 0o775, 0o777}

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
		fMask, fForce := octal(t, sec, "create mask"), octal(t, sec, "force create mode")
		dMask, dForce := octal(t, sec, "directory mask"), octal(t, sec, "force directory mode")

		for _, req := range requests {
			if got := sambaMode(req, fMask, fForce); got != tt.wantFile {
				t.Errorf("[%s] file from requested %04o = %04o, want %04o", tt.section, req, got, tt.wantFile)
			}
			if got := sambaMode(req, dMask, dForce); got != tt.wantFolder {
				t.Errorf("[%s] folder from requested %04o = %04o, want %04o", tt.section, req, got, tt.wantFolder)
			}
		}
	}

	// Spelled out as invariants, so a future edit that loosens a mask fails here
	// with the reason rather than just a changed number.
	team := secs["team-a"]
	if m := octal(t, team, "force create mode"); m&0o060 != 0o060 {
		t.Errorf("team files must be group-writable, got force create mode %04o", m)
	}
	if m := octal(t, team, "create mask"); m&0o007 != 0 {
		t.Errorf("team files must not be reachable by other, got create mask %04o", m)
	}
	if m := octal(t, team, "force directory mode"); m&0o2000 == 0 {
		t.Errorf("team folders must keep setgid so new files inherit the team group, got %04o", m)
	}
	home := secs["homes"]
	if m := octal(t, home, "create mask"); m&0o077 != 0 {
		t.Errorf("home files must be owner-only, got create mask %04o", m)
	}
}

func TestSpliceAppendsWhenAbsent(t *testing.T) {
	existing := "[global]\n   workgroup = WORKGROUP\n"
	block := Render(groups(), "/g")
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
	first := Splice(existing, Render(groups()[:1], "/g")) // only team-b
	second := Splice(first, Render(groups(), "/g"))       // now team-a + team-b

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
	third := Splice(second, Render(groups(), "/g"))
	if third != second {
		t.Error("splicing the same block must be idempotent")
	}
}

func TestSpliceRemovesStaleShare(t *testing.T) {
	// A group dropped from the roster must disappear from the managed block.
	with := Splice("[global]\n", Render(groups(), "/g"))
	without := Splice(with, Render(groups()[1:], "/g")) // only team-a
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
	out := Render(g, "/research/groups")
	// No injected section header or directive may appear at the start of a line;
	// the payload must be collapsed onto the single `comment =` line.
	for _, ln := range strings.Split(out, "\n") {
		s := strings.TrimSpace(ln)
		if strings.HasPrefix(s, "[public]") || strings.HasPrefix(s, "guest ok") {
			t.Fatalf("newline injection leaked a directive onto its own line:\n%s", out)
		}
	}
}
