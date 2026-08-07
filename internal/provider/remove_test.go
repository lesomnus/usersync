package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/lesomnus/usersync/internal/run"
)

// backends enumerates every Provider implementation so the invariants below are
// checked against all of them rather than only the one in production today.
func backends() map[string]func(run.Runner) Provider {
	return map[string]func(run.Runner) Provider{
		"shadow-utils": NewShadowUtils,
		"busybox":      NewBusybox,
		"pw":           NewPw,
	}
}

// withEtc points a backend at a fixture instead of the real /etc.
func withEtc(p Provider, etc string) Provider {
	switch v := p.(type) {
	case *shadowUtils:
		v.etc = etc
	case *busybox:
		v.etc = etc
	case *pw:
		v.etc = etc
	default:
		panic("withEtc: unknown backend")
	}
	return p
}

// localDB writes a fixture /etc/passwd and /etc/group.
func localDB(t *testing.T, passwd, group string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"passwd": passwd, "group": group} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func deleteVerbs(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		for _, verb := range []string{"userdel", "groupdel", "deluser", "delgroup"} {
			if strings.Contains(c, verb) {
				out = append(out, c)
			}
		}
	}
	return out
}

// RemoveAccount exists to release a NAME while the DATA stays put: the files in
// the home are owned by a numeric uid and must survive untouched so a directory
// service can pick that number back up. Every backend's delete command has an
// opt-in flag that would also erase the home (`userdel -r`, `deluser
// --remove-home`, `pw userdel -r`) — passing one would silently turn a reversible
// handover into permanent data loss, with no other symptom until someone looks
// for their files. No backend may ever emit one.
func TestRemoveAccountNeverDeletesTheHome(t *testing.T) {
	homeDestroying := []string{"-r", "-R", "--remove", "--remove-home", "--remove-all-files"}
	etc := localDB(t,
		"alice:x:3001:3001:Alice:/research/home/alice:/usr/sbin/nologin\n",
		"alice:x:3001:\n")

	for name, newProvider := range backends() {
		t.Run(name, func(t *testing.T) {
			fake := &run.Fake{}
			p := withEtc(newProvider(fake), etc)
			if err := p.RemoveAccount(context.Background(), "alice"); err != nil {
				t.Fatalf("RemoveAccount: %v", err)
			}
			if len(fake.Calls) == 0 {
				t.Fatal("RemoveAccount issued no commands at all")
			}
			for _, c := range fake.Calls {
				for _, a := range c.Args {
					if slices.Contains(homeDestroying, a) {
						t.Errorf("%q passes %q — RemoveAccount must leave the home directory on disk", c, a)
					}
				}
			}
		})
	}
}

// The exact commands each backend issues, so a backend edit that changes them
// has to say so here.
func TestRemoveAccountCommands(t *testing.T) {
	want := map[string][]string{
		"shadow-utils": {"userdel alice", "groupdel alice"},
		"busybox":      {"deluser alice", "delgroup alice"},
		"pw":           {"pw userdel -n alice", "pw groupdel -n alice"},
	}
	etc := localDB(t,
		"alice:x:3001:3001:Alice:/research/home/alice:/usr/sbin/nologin\n",
		"alice:x:3001:\n")

	for name, newProvider := range backends() {
		t.Run(name, func(t *testing.T) {
			fake := &run.Fake{}
			p := withEtc(newProvider(fake), etc)
			if err := p.RemoveAccount(context.Background(), "alice"); err != nil {
				t.Fatalf("RemoveAccount: %v", err)
			}
			if got := fake.Commands(); !reflect.DeepEqual(got, want[name]) {
				t.Errorf("commands =\n\t%#v\nwant\n\t%#v", got, want[name])
			}
		})
	}
}

// Presence must be read from the LOCAL database, not from NSS.
//
// The delete tools only touch the local files. Probing NSS instead asks a
// different question: once winbind answers for a name that has already been
// detached — which is the whole point of detaching it — the probe reports
// present, the delete finds nothing local to remove and fails, and an operation
// documented as idempotent errors out. This is the second run of detach on a
// handed-over user, and the retry of an interrupted one.
func TestRemoveAccountIsIdempotentWhenOnlyTheDirectoryAnswers(t *testing.T) {
	// Empty local database: the name resolves through the directory, not here.
	etc := localDB(t, "root:x:0:0:root:/root:/bin/bash\n", "root:x:0:\n")

	for name, newProvider := range backends() {
		t.Run(name, func(t *testing.T) {
			// A runner that would FAIL any delete, standing in for `userdel alice`
			// reporting that alice does not exist in /etc/passwd.
			fake := &run.Fake{Handler: func(_, cmd string, args ...string) (string, error) {
				return "", errNoSuchUser
			}}
			p := withEtc(newProvider(fake), etc)
			if err := p.RemoveAccount(context.Background(), "alice"); err != nil {
				t.Fatalf("must be a no-op when there is no local entry, got %v", err)
			}
			if got := deleteVerbs(fake.Commands()); len(got) != 0 {
				t.Errorf("no local entry must issue no deletes, got %v", got)
			}
		})
	}
}

// A group is only the user's private group when its gid equals the user's uid.
// Deleting one merely because it shares the name would destroy a shared team
// group and its member list — audit documents that such a group can exist — and
// leave the group folder owned by a gid that resolves to nothing.
func TestRemoveAccountLeavesANonUPGGroupAlone(t *testing.T) {
	etc := localDB(t,
		"alice:x:3001:3001:Alice:/research/home/alice:/usr/sbin/nologin\n",
		// Same name, but a real team gid — not alice's private group.
		"alice:x:12000:alice,bob\n")

	for name, newProvider := range backends() {
		t.Run(name, func(t *testing.T) {
			fake := &run.Fake{}
			p := withEtc(newProvider(fake), etc)
			if err := p.RemoveAccount(context.Background(), "alice"); err != nil {
				t.Fatalf("RemoveAccount: %v", err)
			}
			for _, c := range deleteVerbs(fake.Commands()) {
				if strings.Contains(c, "groupdel") || strings.Contains(c, "delgroup") {
					t.Errorf("must not delete a group that is not the UPG, got %q", c)
				}
			}
			// The user itself must still go.
			if len(deleteVerbs(fake.Commands())) == 0 {
				t.Error("the user account should still have been removed")
			}
		})
	}
}

// errNoSuchUser stands in for what a delete tool reports when the local database
// has no such entry.
var errNoSuchUser = errors.New("user 'alice' does not exist in /etc/passwd")
