package cmd

import (
	"context"
	"fmt"
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

		Flags: append(commonFlags(),
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

			ro := stateToRoster(actual)
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

// stateToRoster converts collected actual state into a declarative roster,
// deterministically ordered by name. A user whose SMB account is disabled is
// exported as status: disabled; one with no SMB account stays active.
func stateToRoster(st *state.State) *roster.Roster {
	ro := &roster.Roster{}

	for _, g := range st.Groups {
		ro.Groups = append(ro.Groups, roster.Group{Name: g.Name, GID: g.GID})
	}
	sort.Slice(ro.Groups, func(i, j int) bool { return ro.Groups[i].Name < ro.Groups[j].Name })

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
		ro.Users = append(ro.Users, user)
	}
	sort.Slice(ro.Users, func(i, j int) bool { return ro.Users[i].Name < ro.Users[j].Name })

	return ro
}
