// Package report renders the reconcile output for humans (Text) and machines
// (JSON), and derives a run's summary counts and process exit code. It reads
// only reconcile.Action and roster.Skipped, performs no I/O beyond writing to
// the supplied writer, and holds no state, so it is trivially unit-testable.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/lesomnus/usersync/internal/reconcile"
	"github.com/lesomnus/usersync/internal/roster"
)

// Result is everything a reconcile pass produced that a report needs: whether
// it was a dry run (plan) or an apply, the computed actions, and the entries
// the loader skipped as out of scope.
type Result struct {
	DryRun  bool
	Actions []reconcile.Action
	Skipped []roster.Skipped
	// Errors, when set (apply), is index-aligned with Actions: a non-nil entry
	// means that action failed to execute. nil/empty for a dry-run plan.
	Errors []error
}

// actionErr returns the execution error for action i, or nil.
func (r Result) actionErr(i int) error {
	if i < len(r.Errors) {
		return r.Errors[i]
	}
	return nil
}

// Summary counts the actions keyed by their Kind.String(), plus a "skip" entry
// holding len(Skipped). Kinds with no actions are absent; "skip" is always
// present (0 when nothing was skipped).
func Summary(r Result) map[string]int {
	m := map[string]int{}
	for _, a := range r.Actions {
		m[a.Kind.String()]++
	}
	m["skip"] = len(r.Skipped)
	return m
}

// ExitCode is 0 for a clean run and non-zero when any action is a Refuse (a
// mismatch or guard violation that requires manual intervention).
func ExitCode(r Result) int {
	for _, a := range r.Actions {
		if a.Kind.Class() == reconcile.Refuse {
			return 1
		}
	}
	return 0
}

// Text writes a human-readable report to w: a header line, one line per action
// prefixed by a class glyph, one line per skipped entry, and a summary line.
func Text(w io.Writer, r Result) error {
	var b strings.Builder
	if r.DryRun {
		b.WriteString("PLAN (dry-run)\n")
	} else {
		b.WriteString("APPLY\n")
	}
	for i, a := range r.Actions {
		fmt.Fprintf(&b, "%s %s %s%s", glyph(a.Kind), a.Kind, a.Name, details(a))
		if err := r.actionErr(i); err != nil {
			fmt.Fprintf(&b, "  ✗ FAILED: %v", err)
		}
		b.WriteByte('\n')
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(&b, "· skip %s %s (%s)\n", s.Kind, s.Name, s.Reason)
	}
	b.WriteString(summaryLine(r))
	b.WriteByte('\n')

	_, err := io.WriteString(w, b.String())
	return err
}

// JSON writes a machine-readable report to w as a single indented object.
func JSON(w io.Writer, r Result) error {
	doc := jsonResult{
		DryRun:  r.DryRun,
		Actions: make([]jsonAction, 0, len(r.Actions)),
		Skipped: make([]jsonSkipped, 0, len(r.Skipped)),
		Summary: Summary(r),
	}
	for i, a := range r.Actions {
		ja := jsonAction{
			Kind:   a.Kind.String(),
			Name:   a.Name,
			UID:    a.UID,
			GID:    a.GID,
			Groups: a.Groups,
			Status: a.Status.String(),
			Reason: a.Reason,
		}
		if err := r.actionErr(i); err != nil {
			ja.Error = err.Error()
		}
		doc.Actions = append(doc.Actions, ja)
	}
	for _, s := range r.Skipped {
		doc.Skipped = append(doc.Skipped, jsonSkipped{
			Kind:   s.Kind,
			Name:   s.Name,
			ID:     s.ID,
			Reason: s.Reason,
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

type jsonAction struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	UID    uint32   `json:"uid"`
	GID    uint32   `json:"gid"`
	Groups []string `json:"groups"`
	Status string   `json:"status"`
	Reason string   `json:"reason"`
	Error  string   `json:"error,omitempty"`
}

type jsonSkipped struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	ID     uint32 `json:"id"`
	Reason string `json:"reason"`
}

type jsonResult struct {
	DryRun  bool           `json:"dry_run"`
	Actions []jsonAction   `json:"actions"`
	Skipped []jsonSkipped  `json:"skipped"`
	Summary map[string]int `json:"summary"`
}

// glyph is the single-rune prefix that conveys an action's flavour at a glance:
// '+' creates/adds/enables, '~' updates, '-' removes (disable/orphan-user),
// '!' refuses (manual), '·' is an orphan-group notice.
func glyph(k reconcile.Kind) string {
	switch k {
	case reconcile.CreateGroup, reconcile.CreateUser, reconcile.CreateUserDisabled,
		reconcile.AddSmb, reconcile.EnableUser, reconcile.EnsureHome:
		return "+"
	case reconcile.UpdateUserGroups, reconcile.SetGroupAdmins, reconcile.SetGroupReaders:
		return "~"
	case reconcile.DisableUser:
		return "-"
	case reconcile.RefuseGroup, reconcile.RefuseUser:
		return "!"
	case reconcile.OrphanGroup, reconcile.OrphanUser, reconcile.ReservedPresent:
		return "·"
	default:
		return "?"
	}
}

// isGroupKind reports whether the action targets a group (so its id is a gid).
func isGroupKind(k reconcile.Kind) bool {
	switch k {
	case reconcile.CreateGroup, reconcile.RefuseGroup, reconcile.OrphanGroup,
		reconcile.SetGroupAdmins, reconcile.SetGroupReaders:
		return true
	default:
		return false
	}
}

// details renders the trailing "uid=…/gid=… groups=[…] (reason)" fragment for
// an action line, with a single leading space (empty fields are omitted).
func details(a reconcile.Action) string {
	var parts []string
	if isGroupKind(a.Kind) {
		parts = append(parts, fmt.Sprintf("gid=%d", a.GID))
	} else {
		parts = append(parts, fmt.Sprintf("uid=%d", a.UID))
	}
	if len(a.Groups) > 0 {
		parts = append(parts, "groups=["+strings.Join(a.Groups, ",")+"]")
	}
	if a.Kind == reconcile.SetGroupReaders {
		gids := make([]string, len(a.ReaderGIDs))
		for i, g := range a.ReaderGIDs {
			gids[i] = fmt.Sprintf("%d", g)
		}
		parts = append(parts, "readers=["+strings.Join(gids, ",")+"]")
	}
	s := " " + strings.Join(parts, " ")
	if a.Reason != "" {
		s += " (" + a.Reason + ")"
	}
	return s
}

// summaryLine renders "Summary: k=v k=v" with keys sorted for determinism.
func summaryLine(r Result) string {
	m := Summary(r)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, m[k])
	}
	return "Summary: " + strings.Join(parts, " ")
}
