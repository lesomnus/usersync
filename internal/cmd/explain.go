package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lesomnus/usersync/internal/fsops"
	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"
)

// NewCmdExplain answers "who can read this folder, and what makes that true".
//
// It exists because the answer stopped being written on the folder. While
// readers were POSIX ACL entries, `getfacl` told an operator the whole story for
// free; now the grant is a read-only mount, and the story is split between the
// roster (who is declared) and the mount table (what is actually in force). A
// tool that puts the two back together is not a convenience — without it this
// design is a step backwards in what an operator can find out.
func NewCmdExplain() *xli.Command {
	return &xli.Command{
		Name:  "explain",
		Brief: "show who can read a group's folder, and what grants it",
		Synop: "For each named group (default: every group that declares readers), prints the folder's mode and owner, the reader groups the roster declares, and whether each one's read-only view is actually mounted in THIS mount namespace. Reads only; needs no root.",

		Args: arg.Args{
			&arg.RestStrings{Name: "group", Brief: "groups to explain (default: those with readers)"},
		},
		Flags: append(rosterFlags(), &flg.Switch{Name: "json", Brief: "machine-readable result"}),

		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			if err := applyCommonFlags(cmd, c); err != nil {
				return err
			}
			ro, skipped, err := loadRoster(cmd, c, c.Classifier())
			if err != nil {
				return err
			}
			warnSkipped(cmd, skipped)

			names, _ := arg.Get[[]string](cmd, "group")
			groups, err := pickGroups(ro, names)
			if err != nil {
				return err
			}

			fs := fsops.OS{}
			// A failure here is reported rather than fatal: "the mount table could
			// not be read" is itself part of the answer, and printing the declared
			// side is still worth more than an error.
			views, viewsErr := fs.ReadReaderViews(c.Paths.Groups)

			out := make([]groupExplain, 0, len(groups))
			for _, g := range groups {
				out = append(out, explainGroup(fs, ro, c.Paths.Groups, g, views))
			}
			if jsonRequested(cmd) {
				enc := json.NewEncoder(cmd)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			if viewsErr != nil {
				fmt.Fprintf(errW(cmd), "warning: could not read the mount table, so no view is reported as mounted: %v\n", viewsErr)
			}
			for i, e := range out {
				if i > 0 {
					cmd.Println()
				}
				printExplain(cmd, e)
			}
			return nil
		}),
	}
}

// pickGroups resolves the named groups, or selects the ones worth explaining
// when none were named: those that grant a reader and those that are one.
func pickGroups(ro *roster.Roster, names []string) ([]roster.Group, error) {
	byName := map[string]roster.Group{}
	for _, g := range ro.Groups {
		byName[g.Name] = g
	}
	if len(names) > 0 {
		out := make([]roster.Group, 0, len(names))
		for _, n := range names {
			g, ok := byName[n]
			if !ok {
				return nil, fmt.Errorf("no group %q in the roster", n)
			}
			out = append(out, g)
		}
		return out, nil
	}

	involved := map[string]bool{}
	for _, g := range ro.Groups {
		if len(g.Readers) > 0 {
			involved[g.Name] = true
			for _, r := range g.Readers {
				involved[r] = true
			}
		}
	}
	var out []roster.Group
	for _, g := range ro.Groups {
		if involved[g.Name] {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type readerExplain struct {
	Name    string `json:"name"`
	GID     uint32 `json:"gid"`
	View    string `json:"view"`
	Mounted bool   `json:"mounted"`
}

type groupExplain struct {
	Name    string `json:"name"`
	GID     uint32 `json:"gid"`
	Folder  string `json:"folder"`
	Exists  bool   `json:"exists"`
	Perm    uint32 `json:"perm"`
	OwnerID uint32 `json:"owner_uid"`
	GroupID uint32 `json:"owner_gid"`
	Members int    `json:"members"`
	// Anonymous is the world-facing level; it grants read to everyone, which is
	// a bigger statement than any reader group and so belongs in this answer.
	Anonymous string `json:"anonymous"`
	// Readers are the groups this one grants read-only, and Reads are the groups
	// that grant it read-only — both directions of the same relation, because an
	// operator asks it both ways.
	Readers []readerExplain `json:"readers"`
	Reads   []string        `json:"reads"`
	// LegacyACLGIDs are reader grants still written on the folder by an older
	// version. They are a second way in and apply removes them, so a non-empty
	// list here means this folder has not converged.
	LegacyACLGIDs []uint32 `json:"legacy_acl_gids,omitempty"`
}

func explainGroup(fs fsops.OS, ro *roster.Roster, groupsBase string, g roster.Group, views map[string][]string) groupExplain {
	folder := filepath.Join(groupsBase, g.Name)
	e := groupExplain{
		Name:      g.Name,
		GID:       g.GID,
		Folder:    folder,
		Members:   len(ro.GroupMembership(g)),
		Anonymous: g.Anonymous.String(),
	}
	e.Exists, e.Perm, e.OwnerID, e.GroupID = fs.Stat(folder)

	gidOf := map[string]uint32{}
	for _, other := range ro.Groups {
		gidOf[other.Name] = other.GID
		for _, r := range other.Readers {
			if r == g.Name {
				e.Reads = append(e.Reads, other.Name)
			}
		}
	}
	sort.Strings(e.Reads)

	mounted := map[string]bool{}
	for _, r := range views[g.Name] {
		mounted[r] = true
	}
	for _, r := range g.Readers {
		e.Readers = append(e.Readers, readerExplain{
			Name:    r,
			GID:     gidOf[r],
			View:    fsops.ViewPath(groupsBase, g.Name, r),
			Mounted: mounted[r],
		})
	}
	sort.Slice(e.Readers, func(i, j int) bool { return e.Readers[i].Name < e.Readers[j].Name })

	if e.Exists {
		if gids, err := fs.ReadReaderGIDs(folder); err == nil {
			e.LegacyACLGIDs = gids
		}
	}
	return e
}

func printExplain(cmd *xli.Command, e groupExplain) {
	cmd.Printf("%s  gid=%d  %s\n", e.Name, e.GID, e.Folder)
	if e.Exists {
		cmd.Printf("  folder   mode=%04o owner=%d:%d, %d member(s)\n", e.Perm, e.OwnerID, e.GroupID, e.Members)
	} else {
		cmd.Printf("  folder   MISSING — apply has not created it, %d member(s)\n", e.Members)
	}
	if e.Anonymous != "none" {
		cmd.Printf("  world    anonymous %s — every signed-in user and the web's anonymous identity can read this\n", e.Anonymous)
	}

	switch {
	case len(e.Readers) == 0:
		cmd.Println("  readers  none declared")
	default:
		cmd.Println("  readers  declared in the roster, granted by a read-only view of this folder:")
		for _, r := range e.Readers {
			status := "mounted"
			if !r.Mounted {
				// Not an error on its own: a view is per mount namespace, so a
				// command run outside the serving container is expected to see none.
				status = "NOT MOUNTED — nobody in this group can read it from here"
			}
			cmd.Printf("    %-24s gid=%-6d %s   %s\n", r.Name, r.GID, r.View, status)
		}
	}
	if len(e.Reads) > 0 {
		cmd.Printf("  reads    this group is a reader of: %s\n", strings.Join(e.Reads, ", "))
	}
	if len(e.LegacyACLGIDs) > 0 {
		gids := make([]string, len(e.LegacyACLGIDs))
		for i, g := range e.LegacyACLGIDs {
			gids[i] = fmt.Sprint(g)
		}
		cmd.Printf("  acl      gid(s) %s still granted on the folder by an older version — `apply` clears these\n", strings.Join(gids, ", "))
	}
}
