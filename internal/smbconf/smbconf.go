// Package smbconf generates the Samba share definitions for the managed groups
// and splices them into smb.conf between marker lines, validating the result
// with testparm and (optionally) reloading smbd. This is the plan.md §9 Phase-2
// feature: a new team group added to the roster gets its `[<team>]` share
// without hand-editing smb.conf.
package smbconf

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/usersync/internal/run"
)

const (
	// BeginMarker and EndMarker delimit the usersync-managed block in smb.conf.
	BeginMarker = "# >>> usersync-shares >>>"
	EndMarker   = "# <<< usersync-shares <<<"
)

// Render builds the managed block (markers included) from the roster groups. It
// emits a single [homes] section plus one [<team>] section per group, sorted by
// name for deterministic output. groupsBase is the group folder root.
func Render(groups []roster.Group, groupsBase string) string {
	gs := append([]roster.Group(nil), groups...)
	sort.Slice(gs, func(i, j int) bool { return gs[i].Name < gs[j].Name })

	var b strings.Builder
	b.WriteString(BeginMarker + "\n")
	b.WriteString("# Managed by usersync — do not edit between these markers.\n")

	b.WriteString("\n[homes]\n")
	b.WriteString("   comment = Home Directories\n")
	b.WriteString("   browseable = no\n")
	b.WriteString("   read only = no\n")
	b.WriteString("   valid users = %S\n")

	for _, g := range gs {
		comment := g.Description
		if comment == "" {
			comment = g.Name + " shared"
		}
		fmt.Fprintf(&b, "\n[%s]\n", g.Name)
		fmt.Fprintf(&b, "   comment = %s\n", comment)
		fmt.Fprintf(&b, "   path = %s\n", filepath.Join(groupsBase, g.Name))
		b.WriteString("   browseable = yes\n")
		b.WriteString("   read only = no\n")
		fmt.Fprintf(&b, "   valid users = @%s\n", g.Name)
		fmt.Fprintf(&b, "   force group = %s\n", g.Name)
		b.WriteString("   create mask = 0660\n")
		b.WriteString("   directory mask = 2770\n")
	}

	b.WriteString(EndMarker + "\n")
	return b.String()
}

// Splice returns existing with the managed block replaced by block. If existing
// has no managed block, block is appended (separated by a blank line). Content
// outside the markers is preserved verbatim. It is idempotent: splicing the same
// block twice yields the same result.
func Splice(existing, block string) string {
	if !strings.HasSuffix(block, "\n") {
		block += "\n"
	}
	begin := strings.Index(existing, BeginMarker)
	end := strings.Index(existing, EndMarker)
	if begin >= 0 && end > begin {
		tail := end + len(EndMarker)
		if tail < len(existing) && existing[tail] == '\n' {
			tail++
		}
		return existing[:begin] + block + existing[tail:]
	}
	out := existing
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if out != "" {
		out += "\n"
	}
	return out + block
}

// Validate runs `testparm -s <path>` and returns an error if the config is invalid.
func Validate(ctx context.Context, r run.Runner, path string) error {
	if _, err := r.Run(ctx, "", "testparm", "-s", path); err != nil {
		return err
	}
	return nil
}

// Reload asks smbd to re-read its configuration (no daemon restart).
func Reload(ctx context.Context, r run.Runner) error {
	_, err := r.Run(ctx, "", "smbcontrol", "smbd", "reload-config")
	return err
}

// Apply splices the rendered block into the file at confPath. It validates the
// spliced result with testparm BEFORE touching the real file; on success it
// backs the original up to confPath+".bak", writes the new content, and (if
// reload) reloads smbd — restoring the original if the reload fails. It reports
// whether the file changed. A no-op (block already current) makes no writes.
func Apply(ctx context.Context, confPath, groupsBase string, groups []roster.Group, r run.Runner, reload bool) (changed bool, err error) {
	orig, err := os.ReadFile(confPath)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", confPath, err)
	}
	updated := Splice(string(orig), Render(groups, groupsBase))
	if updated == string(orig) {
		return false, nil
	}

	// Validate the candidate before modifying the live file.
	tmp, err := os.CreateTemp(filepath.Dir(confPath), ".smb.conf-*.tmp")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(updated); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := Validate(ctx, r, tmpName); err != nil {
		return false, fmt.Errorf("testparm rejected the generated smb.conf: %w", err)
	}

	if err := os.WriteFile(confPath+".bak", orig, 0o644); err != nil {
		return false, fmt.Errorf("write backup: %w", err)
	}
	if err := os.WriteFile(confPath, []byte(updated), 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", confPath, err)
	}
	if reload {
		if err := Reload(ctx, r); err != nil {
			_ = os.WriteFile(confPath, orig, 0o644) // restore
			return false, fmt.Errorf("reload failed (original restored): %w", err)
		}
	}
	return true, nil
}
