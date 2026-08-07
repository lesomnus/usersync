package provider

import (
	"context"
	"fmt"
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

// allPresent makes every presence probe report "found", so each backend takes
// its full delete path. `pw` probes with its own show subcommands and reads a
// nil error as present; getent-based backends read non-empty output as present.
func allPresent(_, _ string, _ ...string) (string, error) { return "some:entry\n", nil }

// allAbsent makes every presence probe report "not found" for each backend's own
// convention: empty output for getent, a non-zero exit for `pw ... show`.
func allAbsent(_, name string, args ...string) (string, error) {
	if name == "pw" && len(args) > 0 && strings.HasSuffix(args[0], "show") {
		return "", fmt.Errorf("pw: no such entry")
	}
	return "", nil
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

	for name, newProvider := range backends() {
		t.Run(name, func(t *testing.T) {
			fake := &run.Fake{Handler: allPresent}
			if err := newProvider(fake).RemoveAccount(context.Background(), "alice"); err != nil {
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
		"shadow-utils": {
			"getent passwd alice",
			"userdel alice", // no -r
			"getent group alice",
			"groupdel alice",
		},
		"busybox": {
			"getent passwd alice",
			"deluser alice", // no --remove-home
			"getent group alice",
			"delgroup alice",
		},
		"pw": {
			"pw usershow alice",
			"pw userdel -n alice", // no -r
			"pw groupshow alice",
			"pw groupdel -n alice",
		},
	}

	for name, newProvider := range backends() {
		t.Run(name, func(t *testing.T) {
			fake := &run.Fake{Handler: allPresent}
			if err := newProvider(fake).RemoveAccount(context.Background(), "alice"); err != nil {
				t.Fatalf("RemoveAccount: %v", err)
			}
			if got := fake.Commands(); !reflect.DeepEqual(got, want[name]) {
				t.Errorf("commands =\n\t%#v\nwant\n\t%#v", got, want[name])
			}
		})
	}
}

// Detach can be retried: a run interrupted between the user delete and the group
// delete, or simply repeated, must not fail on the parts already done.
func TestRemoveAccountAbsentIsIdempotent(t *testing.T) {
	for name, newProvider := range backends() {
		t.Run(name, func(t *testing.T) {
			fake := &run.Fake{Handler: allAbsent}
			if err := newProvider(fake).RemoveAccount(context.Background(), "alice"); err != nil {
				t.Fatalf("RemoveAccount on an absent user must be a no-op, got %v", err)
			}
			for _, c := range fake.Commands() {
				for _, verb := range []string{"userdel", "groupdel", "deluser", "delgroup"} {
					if strings.Contains(c, verb) {
						t.Errorf("absent user must issue no deletes, got %q", c)
					}
				}
			}
		})
	}
}
