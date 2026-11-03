package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/lesomnus/usersync/internal/cmd/config"
	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/smbconf"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
)

// `usersync watch` is the boot-and-hot-reload loop as a command.
//
// It applies the roster now, then reconciles again each time it or usersync.yaml
// changes — the same cycle the container entrypoint used to drive from bash +
// inotifywait, owned by usersync instead so the reconcile loop is ONE implementation with one
// set of guarantees (validate-before-apply, the run lock, degrade-not-die on a
// missing quota backend). With --reload-smb it also rewrites smb.conf and reloads
// smbd each cycle — what an SMB-server container runs.
//
// Change detection is a poll of the two files' stat, not a filesystem-notify
// dependency, keeping the single-static-binary property: a ConfigMap update
// repoints the file to a new inode, so its mtime changes and the poll catches
// it. A roster that fails to load is logged and skipped, leaving the last good
// state serving — the same non-fatal stance the bash watcher had.
func NewCmdWatch() *xli.Command {
	return &xli.Command{
		Name:  "watch",
		Brief: "apply the roster, then reconcile on every change (long-running)",
		Synop: "For a container: applies the roster now and re-applies each time it or usersync.yaml changes, keeping the last good state when a change does not load. A config that leaves manage mode stops it. --reload-smb also rewrites smb.conf and reloads smbd each cycle.",

		Flags: append(applyFlags(),
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
			// The two roles are exclusive: --reload-smb is the SMB server (owns
			// smb.conf, tdbsam, folders), --nss-only is the web pod (POSIX accounts
			// only). Asking for both is a misconfigured entrypoint, not a mode.
			if nssOnly, _ := flg.Get[bool](cmd, "nss-only"); nssOnly && reloadSmb {
				return fmt.Errorf("watch: --nss-only and --reload-smb are mutually exclusive (web pod vs SMB server)")
			}
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
					if _, err := smbconf.Apply(ctx, smbConfPath, c.Paths.Home, c.Paths.Groups, c.Paths.Views, ro.Groups, run.Exec{}, true); err != nil {
						fmt.Fprintf(errW(cmd), "watch: smb.conf/reload failed: %v\n", err)
					}
				}
			}

			cmd.Println("watch: initial apply")
			cycle()

			// usersync.yaml is watched too, and reloaded in place. A ConfigMap
			// updates both files at once, and a config change read only at start
			// would wait for the next restart while looking applied — as
			// `legacy_views: false` did (usersync#13). With the default config
			// (no file) there is nothing to watch.
			cfgPath := c.Path()
			last, lastCfg := fileSig(rosterPath), fileSig(cfgPath)
			// The otel section as written, to tell a real change from the
			// defaults Build filled into c.Otel at start.
			var otelAsRead config.OtelConfig
			if cfgPath != "" {
				if raw, err := config.ReadFromFile(cfgPath); err == nil {
					otelAsRead = raw.Otel
				}
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-ticker.C:
					changed := false
					if cfgPath != "" {
						if sig := fileSig(cfgPath); sig != lastCfg {
							lastCfg = sig
							if otelChanged, err := reloadConfig(cmd, c, &otelAsRead); err != nil {
								if errors.Is(err, errModeChanged) {
									return err
								}
								fmt.Fprintf(errW(cmd), "watch: config not reloaded, kept last good config: %v\n", err)
							} else {
								cmd.Println("watch: config changed")
								if otelChanged {
									fmt.Fprintln(errW(cmd), "watch: the otel section changed; it takes effect on restart")
								}
								changed = true
							}
						}
					}
					if sig := fileSig(rosterPath); sig != last {
						last = sig
						cmd.Println("watch: roster changed")
						changed = true
					}
					if changed {
						cycle()
					}
				}
			}
		}),
	}
}

// errModeChanged stops watch: a config that leaves manage mode is not one watch
// can keep running under, so it exits and the supervisor starts what the new
// mode calls for.
var errModeChanged = errors.New("watch: mode is no longer manage; exiting so it restarts under the new config")

// reloadConfig reads usersync.yaml again, the way the process did at start
// (defaults, validation, the command line's overrides), and swaps it into c,
// which every later cycle reads. A config that does not load leaves c as it was.
// The otel section is the exception: its pipeline was built at start, so c keeps
// the running one, and otelChanged reports a change to it against otelAsRead —
// the section as last written, not c.Otel, which Build filled with defaults —
// so the caller can say a restart is needed, once per change.
func reloadConfig(cmd *xli.Command, c *config.Config, otelAsRead *config.OtelConfig) (otelChanged bool, err error) {
	nc, err := config.ReadFromFile(c.Path())
	if err != nil {
		return false, err
	}
	if err := nc.Evaluate(); err != nil {
		return false, err
	}
	if err := applyCommonFlags(cmd, nc); err != nil {
		return false, err
	}
	if err := requireManageMode(nc, "watch"); err != nil {
		return false, fmt.Errorf("%w: %v", errModeChanged, err)
	}
	if !reflect.DeepEqual(nc.Otel, *otelAsRead) {
		otelChanged = true
		*otelAsRead = nc.Otel
	}
	nc.Otel = c.Otel
	*c = *nc
	return otelChanged, nil
}

// fileSig is a cheap change signature: mtime + size of a file, following
// symlinks (a ConfigMap swap repoints the file to a new inode with a new mtime,
// so this changes). An unreadable file returns a distinct value, so its later
// reappearance reads as a change.
func fileSig(path string) string {
	if path == "" {
		return ""
	}
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
