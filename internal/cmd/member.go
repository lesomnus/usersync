package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
)

// `usersync member` edits team membership in roster.yaml.
//
// It exists so that something other than a person can change the roster without
// destroying it. The obvious way to do that from another program is to decode
// the YAML, change a field and re-encode, and the result is a file with no
// comments, no blank lines, expanded flow sequences and reordered keys — which
// still describes the same accounts, and is no longer a record anyone reads.
// See internal/roster/edit.go.
//
// The command changes only the declaration. It does not touch the system:
// `usersync apply` is what converges it, and keeping the two separate is what
// lets a caller preview with `plan` in between, exactly as a person would.

func NewCmdMember() *xli.Command {
	return &xli.Command{
		Name:  "member",
		Brief: "add or remove a user's team membership in the roster (does not apply)",
		Synop: "Edits roster.yaml in place, preserving comments, blank lines and layout — a membership change touches the one line that declares it. " +
			"The result is validated before it is written, so an edit that would make the roster unloadable is refused rather than saved. " +
			"Nothing on the system changes until `usersync apply` runs.",

		Commands: xli.Commands{
			memberOp("add", "add <user> to <group>"),
			memberOp("remove", "remove <user> from <group>"),
		},
	}
}

func memberOp(op, brief string) *xli.Command {
	return &xli.Command{
		Name:  op,
		Brief: brief,

		Args: arg.Args{
			&arg.String{Name: "user", Brief: "the user to change"},
			&arg.String{Name: "group", Brief: "the team group"},
		},
		Flags: append(rosterFlags(),
			&flg.Switch{Name: "json", Brief: "machine-readable result"},
		),

		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			if err := applyCommonFlags(cmd, c); err != nil {
				return err
			}
			// In audit mode a directory owns the accounts. Editing the roster
			// would not be undone by anything — which is precisely why it must be
			// refused: the roster is the ledger of what the directory is supposed
			// to hold, and changing it here would make the two disagree silently.
			if err := requireManageMode(c, "edit membership"); err != nil {
				return err
			}

			user := arg.MustGet[string](cmd, "user")
			group := arg.MustGet[string](cmd, "group")
			if !roster.ValidName(user) || !roster.ValidName(group) {
				return fmt.Errorf("invalid name (must match %s)", roster.NamePattern)
			}

			path := "roster.yaml"
			if p, ok := flg.Get[string](cmd, "roster"); ok && p != "" {
				path = p
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return z.Err(err, "read roster %q", path)
			}

			doc, err := roster.ParseDocument(src)
			if err != nil {
				return err
			}

			// The group has to be one this roster declares. Without the check the
			// edit would write a membership that `usersync apply` then rejects, so
			// the failure would land on the next boot instead of on this caller.
			if err := declaresGroup(src, group); err != nil {
				return err
			}

			var changed bool
			switch op {
			case "add":
				changed, err = doc.AddGroup(user, group)
			default:
				changed, err = doc.RemoveGroup(user, group)
			}
			if err != nil {
				return err
			}

			out := doc.String()

			// VALIDATE BEFORE WRITING. The boot sequence refuses to start on a
			// roster that does not load, so an unvalidated write here turns a bad
			// request into a server that will not come up — possibly not until the
			// next restart, weeks later, with nothing connecting the two.
			cls := c.Classifier()
			policy, err := c.Policy()
			if err != nil {
				return err
			}
			edited, err := roster.Load(strings.NewReader(out))
			if err != nil {
				return fmt.Errorf("refusing to write: the edited roster does not load: %w", err)
			}
			if _, err := edited.Validate(cls, policy); err != nil {
				return fmt.Errorf("refusing to write: the edited roster is invalid: %w", err)
			}

			if changed {
				if err := roster.WriteFile(path, []byte(out)); err != nil {
					return z.Err(err, "write roster %q", path)
				}
			}

			groups, _ := doc.Groups(user)
			if jsonRequested(cmd) {
				enc := json.NewEncoder(cmd)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"user":    user,
					"group":   group,
					"changed": changed,
					"groups":  groups,
				})
			}
			if !changed {
				cmd.Printf("no change: %s is already %s %s\n", user, alreadyWord(op), group)
				return nil
			}
			cmd.Printf("%s: %s -> [%s]\n", user, opPast(op), strings.Join(groups, ", "))
			cmd.Println("run `usersync apply` to converge the system")
			return nil
		}),
	}
}

func alreadyWord(op string) string {
	if op == "add" {
		return "in"
	}
	return "not in"
}

func opPast(op string) string {
	if op == "add" {
		return "added to"
	}
	return "removed from"
}

// declaresGroup reports whether the roster declares the group.
//
// Read from the same bytes the edit is made against rather than from a
// separately-loaded roster, so the two cannot disagree.
func declaresGroup(src []byte, group string) error {
	ro, err := roster.Load(strings.NewReader(string(src)))
	if err != nil {
		return err
	}
	for _, g := range ro.Groups {
		if g.Name == group {
			return nil
		}
	}
	return fmt.Errorf("%w: %q is not declared in the roster", roster.ErrNoSuchGroup, group)
}
