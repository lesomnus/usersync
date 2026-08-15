package cmd

import (
	"context"
	"encoding/json"

	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/xli"
)

// `usersync roster` prints the DECLARATION, where `export` prints what the
// system actually has.
//
// The distinction matters to a caller deciding whether someone is allowed to do
// something. `owners` is applied to /etc/gshadow, so the two normally agree —
// but "normally" is not a basis for an authorization check, and the roster is
// what an operator reviewed and committed. Asking the system instead would mean
// that anyone who could edit gshadow directly could grant themselves the
// delegation, which is a longer way round to the same place but a way round.
//
// It exists in JSON because the consumer is another program. darak has no YAML
// parser and deliberately no external Go dependencies, so this is the seam.
func NewCmdRoster() *xli.Command {
	return &xli.Command{
		Name:  "roster",
		Brief: "print the declared roster (not the system state — see `export`)",
		Synop: "Loads and validates roster.yaml and prints the entries this installation manages. Unlike `export`, which scans the system, this is the declaration as written — what an operator committed, which is what an authorization decision should rest on.",

		Flags: rosterFlags(),

		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			if err := applyCommonFlags(cmd, c); err != nil {
				return err
			}
			cls := c.Classifier()

			ro, skipped, err := loadRoster(cmd, c, cls)
			if err != nil {
				return err
			}
			warnSkipped(cmd, skipped)

			enc := json.NewEncoder(cmd)
			enc.SetIndent("", "  ")
			return enc.Encode(newRosterView(ro))
		}),
	}
}

// rosterView is the JSON shape. Written out rather than tagging the roster
// structs, so the YAML schema and this wire format can move independently —
// the YAML is a file people edit, and this is an interface another program
// parses.
type rosterView struct {
	Groups []groupView `json:"groups"`
	Users  []userView  `json:"users"`
}

type groupView struct {
	Name        string   `json:"name"`
	GID         uint32   `json:"gid"`
	Description string   `json:"description,omitempty"`
	Owners      []string `json:"owners"`
	Readers     []string `json:"readers"`
	// Anonymous is the folder's unauthenticated-access level: "none", "read", or
	// "write". darak reads this to decide which folders to show anonymous
	// visitors and whether the anonymous helper may write.
	Anonymous string `json:"anonymous"`
}

type userView struct {
	Name     string   `json:"name"`
	UID      uint32   `json:"uid"`
	FullName string   `json:"full_name,omitempty"`
	Groups   []string `json:"groups"`
	Status   string   `json:"status"`
}

func newRosterView(ro *roster.Roster) rosterView {
	v := rosterView{Groups: []groupView{}, Users: []userView{}}
	for _, g := range ro.Groups {
		v.Groups = append(v.Groups, groupView{
			Name:        g.Name,
			GID:         g.GID,
			Description: g.Description,
			Owners:      nonNilStrings(g.Owners),
			Readers:     nonNilStrings(g.Readers),
			Anonymous:   g.Anonymous.String(),
		})
	}
	for _, u := range ro.Users {
		v.Users = append(v.Users, userView{
			Name:     u.Name,
			UID:      u.UID,
			FullName: u.FullName,
			Groups:   nonNilStrings(u.Groups),
			Status:   u.Status.String(),
		})
	}
	return v
}

// nonNilStrings keeps an absent list from encoding as `null`, which every
// consumer then has to special-case.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
