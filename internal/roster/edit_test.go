package roster

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The real file's shape: comments in every position that matters, blank-line
// grouping, flow sequences, a trailing inline comment, and both non-active
// lifecycle states. Membership lives on the group, in `members`.
const sample = `# roster.yaml — desired users/groups. Version-controlled; edit then ` + "`usersync apply`" + `.

# Shared (team) groups. Membership is declared here, in ` + "`members`" + `.
groups:
  - name: team-a
    gid: 10001
    description: Perception team
    members: [skim, jlee]      # who is on the team

  - name: team-b
    gid: 10002
    description: Planning team
    members: [jlee]

  - name: team-c
    gid: 10003
    description: Ops team       # no members -> empty team

# Users. ` + "`name`" + ` unique; ` + "`uid`" + ` within the user range.
users:
  - name: skim
    uid: 3001
    full_name: Sunghyun Kim
  - name: jlee
    uid: 3002
    full_name: Jiwon Lee
  - name: ychoi
    uid: 3003
    full_name: Yuna Choi

  # Lifecycle: omit ` + "`status`" + ` for active.
  - name: park
    uid: 3004
    status: disabled
  - name: oldhand
    uid: 3005
    status: reserved
`

func parse(t *testing.T, src string) *Document {
	t.Helper()
	d, err := ParseDocument([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// lineDelta counts inserted and deleted lines with an LCS, so a single
// insertion reads as one change rather than as every following line shifting.
func lineDelta(a, b string) (added, removed []string) {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	n, m := len(al), len(bl)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case al[i] == bl[j]:
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			removed = append(removed, al[i])
			i++
		default:
			added = append(added, bl[j])
			j++
		}
	}
	removed = append(removed, al[i:]...)
	added = append(added, bl[j:]...)
	return added, removed
}

// PRINTING NORMALIZES TRAILING-COMMENT ALIGNMENT, and nothing here can prevent
// it: goccy's printer emits a single space before a trailing comment regardless
// of the padding in the source. So the FIRST machine write to a hand-aligned
// roster re-aligns its inline comments -- once, and never again.
//
// That is the entire residual churn of this approach: the two hand-aligned
// inline comments, against the many lines a struct round-trip rewrites. It is
// pinned here so that if the library ever stops doing it, the change is noticed
// rather than stumbled upon.
func TestPrintingNormalizesTrailingCommentAlignment(t *testing.T) {
	d := parse(t, sample)

	added, removed := lineDelta(sample, d.String())

	if len(added) != 2 || len(removed) != 2 {
		t.Fatalf("printing an untouched document changed +%d/-%d lines, want +2/-2", len(added), len(removed))
	}
	for _, line := range added {
		if !strings.Contains(line, " # ") {
			t.Errorf("a non-comment line changed on a bare print: %q", line)
		}
	}
}

// The whole reason this package exists. Adding one person to one team must touch
// one line -- not reformat the file, not drop the comments, not expand the flow
// sequences. A diff nobody can read is not an account-management record, however
// complete it is.
func TestAddMemberChangesExactlyOneLine(t *testing.T) {
	d := parse(t, sample)

	// team-c has no members key, so the membership is one added line.
	changed, err := d.AddMember("team-c", "ychoi")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("AddMember reported no change")
	}

	// Measured against a PRINT of the unedited document, so the comment
	// realignment above is held constant and what is counted is the edit alone.
	baseline := parse(t, sample).String()
	added, removed := lineDelta(baseline, d.String())

	if len(added) != 1 || len(removed) != 0 {
		t.Fatalf("adding one member changed +%d/-%d lines, want +1/-0:\n+%s\n-%s",
			len(added), len(removed), strings.Join(added, "\n+"), strings.Join(removed, "\n-"))
	}
	if !strings.Contains(added[0], "ychoi") {
		t.Errorf("the added line is not the membership: %q", added[0])
	}
}

// Everything the struct round-trip destroys has to survive: comments in three
// positions, blank lines, flow style, key order.
func TestEditPreservesCommentsAndLayout(t *testing.T) {
	d := parse(t, sample)
	if _, err := d.AddMember("team-b", "skim"); err != nil {
		t.Fatal(err)
	}
	got := d.String()

	for _, want := range []string{
		"# roster.yaml — desired users/groups",
		"# Shared (team) groups.",
		"# Lifecycle: omit",
		"# no members -> empty team",
		"description: Perception team",
		"status: reserved",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %q", want)
		}
	}
	// Flow style stays flow style (sorted on edit).
	if !strings.Contains(got, "members: [jlee, skim]") {
		t.Errorf("flow sequence was expanded:\n%s", got)
	}
	// Blank-line grouping survives.
	if !strings.Contains(got, "members: [jlee, skim]\n\n  - name: team-c") {
		t.Errorf("blank line between groups was deleted:\n%s", got)
	}
}

func TestAddAndRemoveAreIdempotent(t *testing.T) {
	d := parse(t, sample)

	if changed, err := d.AddMember("team-a", "skim"); err != nil || changed {
		t.Fatalf("adding an existing membership: changed=%v err=%v", changed, err)
	}
	if changed, err := d.RemoveMember("team-b", "skim"); err != nil || changed {
		t.Fatalf("removing an absent membership: changed=%v err=%v", changed, err)
	}
	// Both reported no change, so a caller writes nothing at all -- which is the
	// property that matters: an unchanged roster is never reopened.
	baseline := parse(t, sample).String()
	if got := d.String(); got != baseline {
		added, removed := lineDelta(baseline, got)
		t.Errorf("a no-op edit altered the document:\n+%s\n-%s",
			strings.Join(added, "\n+"), strings.Join(removed, "\n-"))
	}
}

// Removing a team's last member must leave an empty team, written the way a
// person writes one -- no `members: []` residue.
func TestRemoveLastMemberDropsTheKey(t *testing.T) {
	d := parse(t, sample)

	if _, err := d.RemoveMember("team-b", "jlee"); err != nil {
		t.Fatal(err)
	}
	got := d.String()
	if strings.Contains(got, "members: []") {
		t.Errorf("left an empty members key:\n%s", got)
	}
	members, err := d.Members("team-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 0 {
		t.Errorf("members = %v, want none", members)
	}
}

// The edited document must still be a valid roster -- the editor is not allowed
// to produce something the boot sequence will refuse.
func TestEditedDocumentStillLoads(t *testing.T) {
	d := parse(t, sample)
	if _, err := d.AddMember("team-c", "ychoi"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RemoveMember("team-a", "jlee"); err != nil {
		t.Fatal(err)
	}

	ro, err := Load(strings.NewReader(d.String()))
	if err != nil {
		t.Fatalf("edited roster no longer loads: %v", err)
	}
	byName := map[string][]string{}
	for _, g := range ro.Groups {
		byName[g.Name] = g.Members
	}
	if got := byName["team-c"]; len(got) != 1 || got[0] != "ychoi" {
		t.Errorf("team-c members = %v, want [ychoi]", got)
	}
	if got := byName["team-a"]; len(got) != 1 || got[0] != "skim" {
		t.Errorf("team-a members = %v, want [skim]", got)
	}
	// The reserved tombstone is still there. An edit that released a uid
	// reservation would be the worst possible outcome of this feature.
	found := false
	for _, u := range ro.Users {
		if u.Name == "oldhand" && u.Status == Reserved && u.UID == 3005 {
			found = true
		}
	}
	if !found {
		t.Error("the reserved entry for uid 3005 did not survive the edit")
	}
}

func TestUnknownGroupIsRefused(t *testing.T) {
	d := parse(t, sample)

	if _, err := d.AddMember("nogroup", "skim"); !errors.Is(err, ErrNoSuchGroup) {
		t.Errorf("AddMember(unknown group) = %v, want ErrNoSuchGroup", err)
	}
	if _, err := d.Members("nogroup"); !errors.Is(err, ErrNoSuchGroup) {
		t.Errorf("Members(unknown group) = %v, want ErrNoSuchGroup", err)
	}
}

// A file whose shape this editor does not recognise is refused rather than
// rewritten. The roster is hand-editable by design; an unfamiliar structure is
// far more likely to be deliberate than to be corruption.
func TestUneditableShapesAreRefused(t *testing.T) {
	for name, src := range map[string]string{
		"no groups key":       "users:\n  - name: skim\n    uid: 3001\n",
		"groups is a mapping": "groups:\n  team-a:\n    gid: 10001\n",
		"two documents":       sample + "---\ngroups: []\n",
	} {
		if _, err := ParseDocument([]byte(src)); !errors.Is(err, ErrNotEditable) {
			t.Errorf("%s: err = %v, want ErrNotEditable", name, err)
		}
	}
}

func TestWriteFilePreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roster.yaml")
	if err := os.WriteFile(path, []byte(sample), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := WriteFile(path, []byte("groups: []\n")); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// os.CreateTemp makes 0600 files; without an explicit chmod the roster would
	// quietly become unreadable to everyone but its owner.
	if got := fi.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %04o, want 0640", got)
	}
	// And no temp files left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
}
