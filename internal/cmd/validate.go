package cmd

import (
	"context"
	"encoding/json"

	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
)

func NewCmdValidate() *xli.Command {
	return &xli.Command{
		Name:  "validate",
		Brief: "validate the config and roster without touching the system (no root, no getent)",
		Synop: "Loads usersync.yaml and roster.yaml and runs every static check (config ranges/paths, strict decode, name/id validation, uniqueness, team references, scope policy). Exits non-zero on any error. Ideal as a CI or pre-commit gate — it never contacts the system.",

		Flags: flg.Flags{
			&flg.String{Name: "roster", Brief: "path to roster.yaml", Value: z.Ptr("roster.yaml")},
			&flg.Switch{Name: "json", Brief: "machine-readable result"},
			&flg.Switch{Name: "skip-out-of-scope", Brief: "treat out-of-scope entries as warnings, not errors"},
			&flg.String{Name: "home-base", Brief: "home directory root (overrides config paths.home)"},
			&flg.String{Name: "groups-base", Brief: "group folder root (overrides config paths.groups)"},
		},

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

			if jsonRequested(cmd) {
				type sk struct {
					Kind   string `json:"kind"`
					Name   string `json:"name"`
					ID     uint32 `json:"id"`
					Reason string `json:"reason"`
				}
				out := struct {
					OK      bool `json:"ok"`
					Users   int  `json:"users"`
					Groups  int  `json:"groups"`
					Skipped []sk `json:"skipped"`
				}{OK: true, Users: len(ro.Users), Groups: len(ro.Groups)}
				for _, s := range skipped {
					out.Skipped = append(out.Skipped, sk{s.Kind, s.Name, s.ID, s.Reason})
				}
				enc := json.NewEncoder(cmd)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}

			warnSkipped(cmd, skipped)
			cmd.Printf("OK: config and roster valid — %d users, %d groups", len(ro.Users), len(ro.Groups))
			if len(skipped) > 0 {
				cmd.Printf(", %d skipped (out of scope)", len(skipped))
			}
			cmd.Println()
			return nil
		}),
	}
}
