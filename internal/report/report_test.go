package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lesomnus/usersync/internal/reconcile"
	"github.com/lesomnus/usersync/internal/roster"
)

// mixed is a Result exercising one create, one update, one refuse, one orphan,
// and one skip.
func mixed(dryRun bool) Result {
	return Result{
		DryRun: dryRun,
		Actions: []reconcile.Action{
			{Kind: reconcile.CreateUser, Name: "alice", UID: 3001, Groups: []string{"staff", "dev"}, Status: roster.Active},
			{Kind: reconcile.UpdateUserGroups, Name: "bob", UID: 3002, Groups: []string{"staff"}, Status: roster.Active},
			{Kind: reconcile.RefuseUser, Name: "carol", UID: 500, Status: roster.Active, Reason: "uid not in managed range"},
			{Kind: reconcile.OrphanUser, Name: "dave", UID: 3003, Reason: "absent from roster; SMB disabled, home kept"},
		},
		Skipped: []roster.Skipped{
			{Kind: "user", Name: "eve", ID: 70000, Reason: "uid out of manage scope"},
		},
	}
}

func TestText(t *testing.T) {
	var buf bytes.Buffer
	if err := Text(&buf, mixed(true)); err != nil {
		t.Fatalf("Text: %v", err)
	}
	out := buf.String()

	wants := []string{
		"PLAN (dry-run)",
		"+ create-user alice uid=3001 groups=[staff,dev]",
		"~ update-user-groups bob uid=3002 groups=[staff]",
		"! refuse-user carol uid=500 (uid not in managed range)",
		"· orphan-user dave uid=3003",
		"· skip user eve (uid out of manage scope)",
		"Summary:",
		"create-user=1",
		"update-user-groups=1",
		"refuse-user=1",
		"orphan-user=1",
		"skip=1",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("Text output missing %q\n---\n%s", w, out)
		}
	}

	// The header switches to APPLY when it is not a dry run.
	buf.Reset()
	if err := Text(&buf, mixed(false)); err != nil {
		t.Fatalf("Text: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "APPLY") || strings.Contains(got, "PLAN") {
		t.Errorf("non-dry-run header wrong:\n%s", got)
	}
}

func TestJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := JSON(&buf, mixed(true)); err != nil {
		t.Fatalf("JSON: %v", err)
	}

	var got struct {
		DryRun  bool `json:"dry_run"`
		Actions []struct {
			Kind   string   `json:"kind"`
			Name   string   `json:"name"`
			UID    uint32   `json:"uid"`
			Groups []string `json:"groups"`
			Status string   `json:"status"`
			Reason string   `json:"reason"`
		} `json:"actions"`
		Skipped []struct {
			Kind   string `json:"kind"`
			Name   string `json:"name"`
			ID     uint32 `json:"id"`
			Reason string `json:"reason"`
		} `json:"skipped"`
		Summary map[string]int `json:"summary"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}

	if !got.DryRun {
		t.Error("dry_run should be true")
	}
	if len(got.Actions) != 4 {
		t.Errorf("actions len = %d, want 4", len(got.Actions))
	}
	if len(got.Skipped) != 1 {
		t.Errorf("skipped len = %d, want 1", len(got.Skipped))
	}
	if got.Actions[0].Kind != "create-user" || got.Actions[0].Name != "alice" || got.Actions[0].UID != 3001 {
		t.Errorf("first action = %+v", got.Actions[0])
	}
	if got.Skipped[0].ID != 70000 {
		t.Errorf("skipped id = %d, want 70000", got.Skipped[0].ID)
	}
	want := map[string]int{
		"create-user":        1,
		"update-user-groups": 1,
		"refuse-user":        1,
		"orphan-user":        1,
		"skip":               1,
	}
	for k, v := range want {
		if got.Summary[k] != v {
			t.Errorf("summary[%q] = %d, want %d", k, got.Summary[k], v)
		}
	}
}

func TestExitCode(t *testing.T) {
	// A Refuse action present => non-zero.
	if code := ExitCode(mixed(true)); code == 0 {
		t.Errorf("ExitCode with a refuse = %d, want non-zero", code)
	}

	// No Refuse action => zero, even with changes and orphans present.
	clean := Result{
		Actions: []reconcile.Action{
			{Kind: reconcile.CreateUser, Name: "alice", UID: 3001},
			{Kind: reconcile.OrphanGroup, Name: "oldteam", GID: 7001, Reason: "absent from roster; not deleted"},
			{Kind: reconcile.OrphanUser, Name: "dave", UID: 3003},
		},
		Skipped: []roster.Skipped{{Kind: "user", Name: "eve", ID: 70000, Reason: "out of scope"}},
	}
	if code := ExitCode(clean); code != 0 {
		t.Errorf("ExitCode without a refuse = %d, want 0", code)
	}
}
