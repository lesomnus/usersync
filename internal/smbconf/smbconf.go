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

// oneLine collapses any CR/LF to a space so a value can never splice a new
// directive or section into the managed block.
func oneLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}

// Render builds the managed block (markers included) from the roster groups. It
// emits a single [homes] section plus one [<team>] section per group, sorted by
// name for deterministic output. homeBase and groupsBase are the home and group
// folder roots.
func Render(groups []roster.Group, homeBase, groupsBase string) string {
	gs := append([]roster.Group(nil), groups...)
	sort.Slice(gs, func(i, j int) bool { return gs[i].Name < gs[j].Name })

	var b strings.Builder
	b.WriteString(BeginMarker + "\n")
	b.WriteString("# Managed by usersync — do not edit between these markers.\n")

	b.WriteString("\n[homes]\n")
	b.WriteString("   comment = Home Directories\n")
	// Without an explicit path, [homes] serves the home recorded in passwd.
	// usersync sets that itself (useradd -d), so the two agree today — but that
	// is an unstated dependency between two things that can be changed
	// independently, and if paths.home is ever repointed without a re-apply the
	// share silently keeps serving the old location.
	fmt.Fprintf(&b, "   path = %s\n", oneLine(filepath.Join(homeBase, "%U")))
	b.WriteString("   browseable = no\n")
	b.WriteString("   read only = no\n")
	b.WriteString("   valid users = %S\n")
	// A home is private, so nothing beneath it should carry group or other bits.
	// Without a mask here the section inherits the global default (0744/0755),
	// which leaves those bits set — contained by the 0700 home today, but it also
	// means a file created over SMB and one created through the web get different
	// modes, and the two paths are supposed to agree. See scripts/verify-samba-modes.sh
	// for how these numbers were checked against a real smbd.
	b.WriteString("   create mask = 0600\n")
	b.WriteString("   directory mask = 0700\n")

	for _, g := range gs {
		// Defense-in-depth: names/descriptions are validated at roster load, but
		// never let a stray newline/CR splice a directive into the config file.
		name := oneLine(g.Name)
		comment := oneLine(g.Description)
		if comment == "" {
			comment = name + " shared"
		}
		fmt.Fprintf(&b, "\n[%s]\n", name)
		fmt.Fprintf(&b, "   comment = %s\n", comment)
		fmt.Fprintf(&b, "   path = %s\n", oneLine(filepath.Join(groupsBase, name)))
		b.WriteString("   browseable = yes\n")
		b.WriteString("   read only = no\n")
		fmt.Fprintf(&b, "   valid users = @%s\n", name)
		fmt.Fprintf(&b, "   force group = %s\n", name)
		// Samba computes a new object's mode as (base & mask) | force, where base
		// comes from the DOS attributes — SMB carries no unix mode. The base is
		// 0666 for files (0766 once the archive attribute maps onto the owner
		// execute bit) and 0777 for directories, so the mask alone already yields
		// 0660 and 0770 here. Only `force directory mode` earns its place, and it
		// earns it for one specific reason:
		//
		// A mask can only CLEAR bits, so it can never restore setgid. If the
		// parent directory ever loses its setgid bit, every folder created beneath
		// it over SMB comes out 0770 with no setgid, and files created inside
		// those then take the creator's own group instead of the team's — the
		// teammate-cannot-write failure, one level down and invisible from the
		// client. Forcing 2770 puts setgid back at any depth regardless of the
		// parent's state.
		//
		// `force create mode` is deliberately NOT set: it changes nothing under
		// this configuration, and it would override the DOS-attribute mapping in
		// one that maps read-only onto the permission bits. Both claims were
		// checked against a real smbd — see scripts/verify-samba-modes.sh.
		b.WriteString("   create mask = 0660\n")
		b.WriteString("   directory mask = 2770\n")
		b.WriteString("   force directory mode = 2770\n")
		// A file created over SMB must inherit the folder's default POSIX ACL, so
		// a reader group declared on the team (roster.Group.Readers) can read what
		// somebody uploads through the explorer, exactly as it can read what the
		// web path writes. With this off, whether the inherited entry survives is
		// left to the filesystem; with it on, Samba is told to preserve it. It is
		// harmless where the create mask already keeps the entry effective and
		// necessary where it would not — so it is set unconditionally, because the
		// safe default costs nothing and a team gains a reader without the share
		// being regenerated. Measure against the deployment's smbd at cutover; see
		// scripts/verify-samba-modes.sh.
		b.WriteString("   inherit acls = yes\n")
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
func Apply(ctx context.Context, confPath, homeBase, groupsBase string, groups []roster.Group, r run.Runner, reload bool) (changed bool, err error) {
	orig, err := os.ReadFile(confPath)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", confPath, err)
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(confPath); err == nil {
		mode = fi.Mode().Perm()
	}
	updated := Splice(string(orig), Render(groups, homeBase, groupsBase))
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

	if err := os.WriteFile(confPath+".bak", orig, mode); err != nil {
		return false, fmt.Errorf("write backup: %w", err)
	}
	if err := os.WriteFile(confPath, []byte(updated), mode); err != nil {
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
