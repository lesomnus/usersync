package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/smbconf"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
)

// `usersync watch` is the boot-and-hot-reload loop as a command.
//
// It applies the roster now, then reconciles again each time the file changes —
// the same cycle the container entrypoint used to drive from bash + inotifywait,
// owned by usersync instead so the reconcile loop is ONE implementation with one
// set of guarantees (validate-before-apply, the run lock, degrade-not-die on a
// missing quota backend). With --reload-smb it also rewrites smb.conf and reloads
// smbd each cycle — what an SMB-server container runs.
//
// Change detection is a poll of the roster's stat, not a filesystem-notify
// dependency, keeping the single-static-binary property: a ConfigMap update
// repoints the file to a new inode, so its mtime changes and the poll catches
// it. A roster that fails to load is logged and skipped, leaving the last good
// state serving — the same non-fatal stance the bash watcher had.
func NewCmdWatch() *xli.Command {
	return &xli.Command{
		Name:  "watch",
		Brief: "apply the roster, then reconcile on every change (long-running)",
		Synop: "For a container: applies the roster now and re-applies each time it changes, keeping the last good state when a change does not load. --reload-smb also rewrites smb.conf and reloads smbd each cycle.",

		Flags: append(commonFlags(),
			&flg.Switch{Name: "reload-smb", Brief: "also rewrite smb.conf and reload smbd each cycle"},
			&flg.String{Name: "smb-conf", Brief: "smb.conf path to edit (with --reload-smb)", Value: z.Ptr("/etc/samba/smb.conf")},
			&flg.String{Name: "interval", Brief: "how often to poll the roster for changes", Value: z.Ptr("2s")},
		),

		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			if err := applyCommonFlags(cmd, c); err != nil {
				return err
			}
			if err := requireManageMode(c, "watch"); err != nil {
				return err
			}
			if err := requireRoot(); err != nil {
				return err
			}

			reloadSmb, _ := flg.Get[bool](cmd, "reload-smb")
			interval := 2 * time.Second
			if s, ok := flg.Get[string](cmd, "interval"); ok && s != "" {
				d, err := time.ParseDuration(s)
				if err != nil {
					return fmt.Errorf("watch: invalid --interval %q: %w", s, err)
				}
				interval = d
			}
			rosterPath := rosterFilePath(cmd)
			smbConfPath := "/etc/samba/smb.conf"
			if p, ok := flg.Get[string](cmd, "smb-conf"); ok && p != "" {
				smbConfPath = p
			}

			// One reconcile cycle: apply the roster, then (optionally) the shares.
			// Each cycle takes the run lock so a concurrent `usersync member` edit or
			// a manual apply cannot interleave. A cycle that fails is logged, not
			// fatal — the watcher keeps the last good state and tries the next change.
			cycle := func() {
				unlock, err := lockRun()
				if err != nil {
					fmt.Fprintf(errW(cmd), "watch: could not take the run lock: %v\n", err)
					return
				}
				defer unlock()
				if err := runApplyOnce(ctx, cmd, c); err != nil {
					fmt.Fprintf(errW(cmd), "watch: not applied, kept last good state: %v\n", err)
					return
				}
				if reloadSmb {
					ro, _, err := loadRoster(cmd, c, c.Classifier())
					if err != nil {
						fmt.Fprintf(errW(cmd), "watch: shares not updated: %v\n", err)
						return
					}
					if _, err := smbconf.Apply(ctx, smbConfPath, c.Paths.Home, c.Paths.Groups, ro.Groups, run.Exec{}, true); err != nil {
						fmt.Fprintf(errW(cmd), "watch: smb.conf/reload failed: %v\n", err)
					}
				}
			}

			cmd.Println("watch: initial apply")
			cycle()

			last := rosterSig(rosterPath)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-ticker.C:
					if sig := rosterSig(rosterPath); sig != last {
						last = sig
						cmd.Println("watch: roster changed")
						cycle()
					}
				}
			}
		}),
	}
}

// rosterSig is a cheap change signature: mtime + size of the roster, following
// symlinks (a ConfigMap swap repoints the file to a new inode with a new mtime,
// so this changes). An unreadable roster returns a distinct value, so its later
// reappearance reads as a change.
func rosterSig(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())
}

// rosterFilePath resolves the roster path from --roster, matching loadRoster.
func rosterFilePath(cmd *xli.Command) string {
	if p, ok := flg.Get[string](cmd, "roster"); ok && p != "" {
		return p
	}
	return "roster.yaml"
}
