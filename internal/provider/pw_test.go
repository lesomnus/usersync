package provider

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/lesomnus/usersync/internal/run"
)

func TestPwCommands(t *testing.T) {
	ctx := context.Background()
	// pw probes presence with groupshow/usershow: a non-zero exit means absent.
	absent := func(_, name string, args ...string) (string, error) {
		if name == "pw" && len(args) >= 1 && (args[0] == "groupshow" || args[0] == "usershow") {
			return "", fmt.Errorf("pw: no such entry")
		}
		return "", nil
	}
	// nil handler => groupshow/usershow succeed => entry present.

	tests := []struct {
		name    string
		handler func(stdin, name string, args ...string) (string, error)
		do      func(Provider) error
		want    []string
	}{
		{
			name:    "EnsureGroup absent",
			handler: absent,
			do: func(p Provider) error {
				return p.EnsureGroup(ctx, GroupSpec{Name: "teams", GID: 5000})
			},
			want: []string{
				"pw groupshow teams",
				"pw groupadd -n teams -g 5000",
			},
		},
		{
			name: "EnsureGroup present is idempotent",
			// nil handler => groupshow succeeds => present.
			do: func(p Provider) error {
				return p.EnsureGroup(ctx, GroupSpec{Name: "teams", GID: 5000})
			},
			want: []string{
				"pw groupshow teams",
			},
		},
		{
			name:    "EnsureUser absent",
			handler: absent,
			do: func(p Provider) error {
				return p.EnsureUser(ctx, UserSpec{
					Name: "alice", UID: 1000, GID: 1000,
					Home: "/home/alice", Shell: "/bin/sh", FullName: "Alice A",
				})
			},
			want: []string{
				"pw usershow alice",
				"pw groupshow alice",
				"pw groupadd -n alice -g 1000",
				"pw useradd -n alice -u 1000 -g 1000 -d /home/alice -s /bin/sh -c Alice A",
			},
		},
		{
			name: "EnsureUser present is idempotent",
			do: func(p Provider) error {
				return p.EnsureUser(ctx, UserSpec{
					Name: "alice", UID: 1000, GID: 1000,
					Home: "/home/alice", Shell: "/bin/sh", FullName: "Alice A",
				})
			},
			want: []string{
				"pw usershow alice",
			},
		},
		{
			name: "SetSupplementaryGroups replaces",
			do: func(p Provider) error {
				return p.SetSupplementaryGroups(ctx, "alice", []string{"team-a", "team-b"})
			},
			want: []string{
				"pw usermod alice -G team-a,team-b",
			},
		},
		{
			name: "SetSupplementaryGroups empty clears",
			do: func(p Provider) error {
				return p.SetSupplementaryGroups(ctx, "alice", nil)
			},
			// empty argument renders as an extra space between -G and the end.
			want: []string{
				"pw usermod alice -G ",
			},
		},
		{
			name: "LockPassword",
			do: func(p Provider) error {
				return p.LockPassword(ctx, "alice")
			},
			want: []string{
				"pw lock alice",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &run.Fake{Handler: tt.handler}
			p := NewPw(fake)
			if err := tt.do(p); err != nil {
				t.Fatalf("action: %v", err)
			}
			if got := fake.Commands(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("commands =\n\t%#v\nwant\n\t%#v", got, tt.want)
			}
		})
	}
}
