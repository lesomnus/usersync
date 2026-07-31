package cmd

import (
	"context"

	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"
)

func NewCmdPasswd() *xli.Command {
	return &xli.Command{
		Name:  "passwd",
		Brief: "print a user's seed-derived initial SMB password",
		Synop: "Recomputes the deterministic initial password usersync sets when it creates a user, so an admin can deliver it or reset the account back to it (smbpasswd -a). Needs only the seed (seed_file / USERSYNC_SEED). NOTE: this is the INITIAL password — if the user has since changed it, this will not match.",

		Args: arg.Args{
			&arg.String{Name: "user", Brief: "the user to derive the initial password for"},
		},
		Flags: flg.Flags{
			&flg.String{Name: "seed-file", Brief: "seed file (or env USERSYNC_SEED)"},
		},

		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			flg.VisitP(cmd, "seed-file", &c.SeedFile)

			der, err := deriver(c)
			if err != nil {
				return err
			}
			cmd.Println(der.InitPW(arg.MustGet[string](cmd, "user")))
			return nil
		}),
	}
}
