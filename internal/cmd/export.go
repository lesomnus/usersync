package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/usersync/internal/executor"
	"github.com/lesomnus/usersync/internal/idexport"
	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/state"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
)

func NewCmdExport() *xli.Command {
	return &xli.Command{
		Name:  "export",
		Brief: "print the current system state (managed range) as a roster.yaml, csv, or ldif",
		Synop: "Scans the actual users/groups within the managed range and prints them to stdout. " +
			"The default `roster` format bootstraps a roster from an already-configured server; feeding it back to `plan` should yield zero actions. " +
			"`csv` and `ldif` instead render the RFC2307 id assignments, to seed a directory service with the uid/gid numbers already in use here rather than letting it invent new ones.",

		// rosterFlags, not commonFlags: --format already picks the output shape and
		// export derives no passwords, so --json/--seed-file would only be noise.
		Flags: append(rosterFlags(),
			&flg.String{Name: "format", Brief: "roster (default) | csv | ldif", Value: z.Ptr("roster")},
			&flg.String{Name: "base-dn", Brief: "base DN for --format ldif (e.g. OU=Research,DC=corp,DC=example,DC=com)"},
		),

		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			if err := applyCommonFlags(cmd, c); err != nil {
				return err
			}
			format, _ := flg.Get[string](cmd, "format")
			baseDN, _ := flg.Get[string](cmd, "base-dn")
			// Reject a bad format (or a missing base DN) before scanning, so an
			// invalid invocation fails immediately rather than after the work.
			switch format {
			case "", "roster", "csv":
			case "ldif":
				if strings.TrimSpace(baseDN) == "" {
					return fmt.Errorf("--format ldif needs --base-dn (e.g. OU=Research,DC=corp,DC=example,DC=com)")
				}
			default:
				return fmt.Errorf("invalid --format %q (want roster|csv|ldif)", format)
			}
			cls := c.Classifier()

			actual, err := collectActual(ctx, c, run.Exec{}, cls, true, errW(cmd))
			if err != nil {
				return err
			}

			// Carry over what the SYSTEM CANNOT REPORT. Two things live only in the
			// roster: a group's description (no unix group carries one) and a
			// `status: reserved` entry (there is no account to scan — being absent is
			// exactly what reserved means). Exporting without them turns the
			// documented bootstrap into a lossy round-trip that silently frees
			// reserved uids for reuse.
			prior := priorRoster(cmd, format)
			ro := stateToRoster(actual, prior)
			switch format {
			case "csv":
				return idexport.CSV(cmd, ro, c.Paths.Home, executor.DefaultShell)
			case "ldif":
				return idexport.LDIF(cmd, ro, baseDN)
			default:
				enc := yaml.NewEncoder(cmd, yaml.Indent(2), yaml.IndentSequence(true))
				defer enc.Close()
				return enc.Encode(ro)
			}
		}),
	}
}

// priorRoster reads the roster named by --roster so the export can carry
// roster-only fields forward. A missing or unreadable file is not an error —
// bootstrapping from a server that has no roster yet is the documented first
// use — but it IS worth a word on stderr, because the other way to get here is
// `usersync export > roster.yaml`, where the shell truncates the file before
// this process ever opens it. That invocation cannot be made lossless from
// inside; it can only be pointed out.
func priorRoster(cmd *xli.Command, format string) *roster.Roster {
	if format != "" && format != "roster" {
		return nil // csv/ldif render numbers only; nothing roster-only survives there
	}
	path := "roster.yaml"
	if p, ok := flg.Get[string](cmd, "roster"); ok && p != "" {
		path = p
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(errW(cmd),
			"note: no roster at %s to carry group descriptions and `status: reserved` entries from.\n"+
				"  If you ran `usersync export > %s`, the shell emptied it before this ran — export to a\n"+
				"  new file and diff instead: `usersync export > roster.new.yaml && diff roster.yaml roster.new.yaml`\n",
			path, path)
		return nil
	}
	defer f.Close()

	ro, err := roster.Load(f)
	if err != nil {
		fmt.Fprintf(errW(cmd), "note: %s did not parse (%v); exporting without it\n", path, err)
		return nil
	}
	return ro
}

// stateToRoster converts collected actual state into a declarative roster,
// deterministically ordered by name. A user whose SMB account is disabled is
// exported as status: disabled; one with no SMB account stays active.
//
// prior (may be nil) supplies the fields no scan can observe: group
// descriptions, and reserved tombstones, which have no account by definition.
func stateToRoster(st *state.State, prior *roster.Roster) *roster.Roster {
	descs := map[string]string{}
	reserved := map[string]roster.User{}
	if prior != nil {
		for _, g := range prior.Groups {
			descs[g.Name] = g.Description
		}
		for _, u := range prior.Users {
			if u.Status == roster.Reserved {
				reserved[u.Name] = u
			}
		}
	}

	ro := &roster.Roster{}

	for _, g := range st.Groups {
		// Owners come from the SCAN, not from the prior roster: unlike a
		// description, /etc/gshadow really does hold them, so exporting the
		// system's answer is what makes `export | diff - roster.yaml` mean
		// something for this field. A backend that cannot tell (no gshadow)
		// leaves them absent rather than emitting an empty list, which would
		// read as "this team has no owners".
		var owners []string
		if g.AdminsKnown {
			owners = g.Admins
		}
		ro.Groups = append(ro.Groups, roster.Group{
			Name:        g.Name,
			GID:         g.GID,
			Description: descs[g.Name],
			Owners:      owners,
		})
	}
	sort.Slice(ro.Groups, func(i, j int) bool { return ro.Groups[i].Name < ro.Groups[j].Name })

	seen := map[string]bool{}
	for _, u := range st.Users {
		user := roster.User{
			Name:     u.Name,
			UID:      u.UID,
			FullName: u.FullName,
			Groups:   append([]string(nil), u.Groups...),
		}
		sort.Strings(user.Groups)
		if sm, ok := st.Smb[u.Name]; ok && !sm.Enabled {
			user.Status = roster.Disabled
		}
		seen[u.Name] = true
		ro.Users = append(ro.Users, user)
	}
	// A reserved entry that the scan did not see is not stale — that is the
	// normal, correct state for one. Dropping it would hand the uid back to the
	// next `apply`, which is the single failure this project exists to prevent.
	// One that DID resolve is a different matter: the roster says the number is
	// retired and the system disagrees, so let the scan win and let `audit`
	// report the conflict rather than exporting a contradiction.
	for name, u := range reserved {
		if !seen[name] {
			ro.Users = append(ro.Users, u)
		}
	}
	sort.Slice(ro.Users, func(i, j int) bool { return ro.Users[i].Name < ro.Users[j].Name })

	return ro
}
