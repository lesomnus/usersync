package cmd

import (
	"context"

	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/smbconf"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
)

func NewCmdShares() *xli.Command {
	return &xli.Command{
		Name:  "shares",
		Brief: "generate smb.conf share definitions from the roster",
		Synop: "Renders the [homes] + per-team [<team>] share block from the roster groups. By default prints it (dry-run); with --write it splices the block into smb.conf between markers (validating with testparm and keeping a .bak), and --reload reloads smbd.",

		Flags: flg.Flags{
			&flg.String{Name: "roster", Brief: "path to roster.yaml", Value: z.Ptr("roster.yaml")},
			&flg.String{Name: "smb-conf", Brief: "smb.conf path to edit", Value: z.Ptr("/etc/samba/smb.conf")},
			&flg.Switch{Name: "write", Brief: "splice the block into smb.conf (default: print only)"},
			&flg.Switch{Name: "reload", Brief: "reload smbd after writing (implies --write)"},
		},

		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			cls := c.Classifier()

			ro, skipped, err := loadRoster(cmd, c, cls)
			if err != nil {
				return err
			}
			warnSkipped(cmd, skipped)

			write, _ := flg.Get[bool](cmd, "write")
			reload, _ := flg.Get[bool](cmd, "reload")
			write = write || reload

			if !write {
				cmd.Print(smbconf.Render(ro.Groups, c.Paths.Groups))
				return nil
			}

			if err := requireRoot(); err != nil {
				return err
			}
			unlock, err := lockRun()
			if err != nil {
				return err
			}
			defer unlock()

			path := "/etc/samba/smb.conf"
			if p, ok := flg.Get[string](cmd, "smb-conf"); ok && p != "" {
				path = p
			}
			changed, err := smbconf.Apply(ctx, path, c.Paths.Groups, ro.Groups, run.Exec{}, reload)
			if err != nil {
				return err
			}
			if !changed {
				cmd.Println("smb.conf already up to date")
				return nil
			}
			cmd.Printf("updated %s (original backed up to %s.bak)\n", path, path)
			if reload {
				cmd.Println("reloaded smbd")
			} else {
				cmd.Println("run `smbcontrol smbd reload-config` (or --reload) to apply")
			}
			return nil
		}),
	}
}
