package cmd

import (
	"context"
	"sort"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/state"
	"github.com/lesomnus/xli"
)

func NewCmdExport() *xli.Command {
	return &xli.Command{
		Name:  "export",
		Brief: "print the current system state (managed range) as a roster.yaml",
		Synop: "Scans the actual users/groups within the managed range and prints an equivalent roster.yaml to stdout. Bootstraps a roster from an already-configured server. Feeding the output back to `plan` should yield zero actions.",

		Flags: commonFlags(),

		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			applyCommonFlags(cmd, c)
			cls := c.Classifier()

			actual, err := collectActual(ctx, c, run.Exec{}, cls, true, errW(cmd))
			if err != nil {
				return err
			}

			ro := stateToRoster(actual)
			enc := yaml.NewEncoder(cmd, yaml.Indent(2), yaml.IndentSequence(true))
			defer enc.Close()
			return enc.Encode(ro)
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
