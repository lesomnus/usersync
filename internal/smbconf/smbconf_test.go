package smbconf

import (
	"strings"
	"testing"

	"github.com/lesomnus/usersync/internal/roster"
)

func groups() []roster.Group {
	return []roster.Group{
		{Name: "team-b", GID: 7002, Description: "Planning team"},
		{Name: "team-a", GID: 7001},
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
	g := []roster.Group{{Name: "team-a", GID: 7001, Description: "x\n[public]\n   path = /\n   guest ok = yes"}}
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
