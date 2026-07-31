package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/usersync/internal/idrange"
	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/samba"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
)

func NewCmdPurge() *xli.Command {
	return &xli.Command{
		Name:  "purge",
		Brief: "permanently delete a user (DANGEROUS): archive home, remove account + UPG",
		Synop: "Archives the home directory, then deletes the SMB account, the unix user, and its UPG. By default records a reserved tombstone in the roster so the uid is never reused.",

		Args: arg.Args{
			&arg.String{Name: "user", Brief: "the managed user to purge"},
		},
		Flags: flg.Flags{
			&flg.String{Name: "roster", Brief: "roster to write the reserved tombstone into (with --reserve)", Value: z.Ptr("roster.yaml")},
			&flg.Switch{Name: "yes", Alias: 'y', Brief: "skip the confirmation prompt"},
			&flg.Switch{Name: "reserve", Brief: "also write the tombstone into the roster (re-encodes it, dropping comments); default only prints a snippet to paste"},
		},

		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			if err := requireRoot(); err != nil {
				return err
			}
			unlock, err := lockRun()
			if err != nil {
				return err
			}
			defer unlock()

			c := use_config.Must(ctx)
			cls := c.Classifier()
			user := arg.MustGet[string](cmd, "user")

			uid, home, err := lookupUser(ctx, user)
			if err != nil {
				return err
			}
			if cls.UID(uid) != idrange.Managed {
				return fmt.Errorf("refusing to purge %q: uid %d is not in the managed range", user, uid)
			}

			if yes, _ := flg.Get[bool](cmd, "yes"); !yes {
				ok, err := confirm(cmd, fmt.Sprintf("Permanently purge user %q (uid %d, home %s)? [y/N] ", user, uid, home))
				if err != nil {
					return err
				}
				if !ok {
					cmd.Println("aborted")
					return nil
				}
			}

			runner := run.Exec{}

			// 1. archive home. If the home exists but archiving FAILS, abort before
			// deleting anything — deleting after a failed archive would lose data.
			archive, err := archiveHome(ctx, runner, user, home)
			if err != nil {
				return fmt.Errorf("home archive failed, refusing to delete %q (no data lost): %w", user, err)
			}
			if archive != "" {
				cmd.Printf("archived home -> %s\n", archive)
			}

			// 2. delete SMB account (best effort — may not exist).
			if err := samba.New(runner).Delete(ctx, user); err != nil {
				fmt.Fprintf(errW(cmd), "warning: smbpasswd -x failed (no SMB account?): %v\n", err)
			}

			// 3. delete unix user (removes home) and 4. its UPG.
			if _, err := runner.Run(ctx, "", "userdel", "-r", user); err != nil {
				return fmt.Errorf("userdel %s: %w", user, err)
			}
			if _, err := runner.Run(ctx, "", "groupdel", user); err != nil {
				fmt.Fprintf(errW(cmd), "note: groupdel %s: %v (UPG may have been removed by userdel)\n", user, err)
			}

			cmd.Printf("purged user %q (uid %d)\n", user, uid)

			// 5. reserve the uid so it is never reused. Print the snippet to paste
			// (safe — preserves the human-edited roster); only re-encode the file
			// when --reserve is explicitly requested.
			printReserveSnippet(cmd, user, uid)
			if reserve, _ := flg.Get[bool](cmd, "reserve"); reserve {
				path := "roster.yaml"
				if p, ok := flg.Get[string](cmd, "roster"); ok && p != "" {
					path = p
				}
				abs, _ := filepath.Abs(path)
				if err := reserveInRoster(path, user, uid); err != nil {
					fmt.Fprintf(errW(cmd), "warning: could not write reserved tombstone to %s: %v\n", abs, err)
				} else {
					cmd.Printf("wrote reserved tombstone for uid %d into %s (note: comments were not preserved)\n", uid, abs)
				}
			}
			return nil
		}),
	}
}

// lookupUser reads uid and home from getent passwd.
func lookupUser(ctx context.Context, user string) (uint32, string, error) {
	out, err := run.Exec{}.Run(ctx, "", "getent", "passwd", user)
	if err != nil || strings.TrimSpace(out) == "" {
		return 0, "", fmt.Errorf("user %q not found", user)
	}
	// name:x:uid:gid:gecos:home:shell — the GECOS field may contain a colon, so
	// anchor home/shell to the end rather than using a fixed index (a mis-parsed
	// home here would archive the wrong path yet still delete the real home).
	f := strings.Split(strings.TrimSpace(out), ":")
	if len(f) < 7 {
		return 0, "", fmt.Errorf("unexpected getent output for %q", user)
	}
	uid, err := strconv.ParseUint(f[2], 10, 32)
	if err != nil {
		return 0, "", fmt.Errorf("parse uid for %q: %w", user, err)
	}
	home := f[len(f)-2]
	return uint32(uid), home, nil
}

// archiveHome tars the home directory to <parent>/<user>-purged-<ts>.tar.gz.
func archiveHome(ctx context.Context, r run.Runner, user, home string) (string, error) {
	if _, err := os.Stat(home); err != nil {
		return "", nil // nothing to archive
	}
	parent := filepath.Dir(home)
	base := filepath.Base(home)
	ts := time.Now().Format("20060102-150405")
	archive := filepath.Join(parent, fmt.Sprintf("%s-purged-%s.tar.gz", user, ts))
	if _, err := r.Run(ctx, "", "tar", "czf", archive, "-C", parent, base); err != nil {
		return "", err
	}
	return archive, nil
}

// reserveInRoster loads the roster, sets the user's entry to reserved (adding it
// if absent), and writes it back. Note: this re-encodes the file and does not
// preserve comments.
func reserveInRoster(path, user string, uid uint32) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	ro, err := roster.Load(f)
	f.Close()
	if err != nil {
		return err
	}

	found := false
	for i := range ro.Users {
		if ro.Users[i].Name == user {
			ro.Users[i].Status = roster.Reserved
			ro.Users[i].UID = uid
			found = true
			break
		}
	}
	if !found {
		ro.Users = append(ro.Users, roster.User{Name: user, UID: uid, Status: roster.Reserved})
	}

	out, err := os.CreateTemp(filepath.Dir(path), ".roster-*.tmp")
	if err != nil {
		return err
	}
	tmp := out.Name()
	enc := yaml.NewEncoder(out, yaml.Indent(2), yaml.IndentSequence(true))
	if err := enc.Encode(ro); err != nil {
		enc.Close()
		out.Close()
		os.Remove(tmp)
		return err
	}
	enc.Close()
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func printReserveSnippet(cmd *xli.Command, user string, uid uint32) {
	cmd.Printf("\nTo prevent uid %d from being reused, add this to your roster.yaml:\n", uid)
	cmd.Printf("  - name: %s\n    uid: %d\n    status: reserved\n", user, uid)
}

func confirm(cmd *xli.Command, prompt string) (bool, error) {
	cmd.Print(prompt)
	var answer string
	if _, err := cmd.Scanln(&answer); err != nil {
		// EOF or empty line => treat as "no".
		return false, nil
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}
