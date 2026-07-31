package provider

import (
	"context"
	"reflect"
	"testing"

	"github.com/lesomnus/usersync/internal/run"
)

func TestBusyboxCommands(t *testing.T) {
	ctx := context.Background()
	present := func(_, _ string, _ ...string) (string, error) {
		// Non-empty getent output => entry present.
		return "some:entry\n", nil
	}

	tests := []struct {
		name    string
		handler func(stdin, name string, args ...string) (string, error)
		do      func(Provider) error
		want    []string
	}{
		{
			name: "EnsureGroup absent",
			// nil handler => getent returns "" => absent.
			do: func(p Provider) error {
				return p.EnsureGroup(ctx, GroupSpec{Name: "teams", GID: 5000})
			},
			want: []string{
				"getent group teams",
				"addgroup -g 5000 teams",
			},
		},
		{
			name:    "EnsureGroup present is idempotent",
			handler: present,
			do: func(p Provider) error {
				return p.EnsureGroup(ctx, GroupSpec{Name: "teams", GID: 5000})
			},
			want: []string{
				"getent group teams",
			},
		},
		{
			name: "EnsureUser absent",
			do: func(p Provider) error {
				return p.EnsureUser(ctx, UserSpec{
					Name: "alice", UID: 1000, GID: 1000,
					Home: "/home/alice", Shell: "/bin/sh", FullName: "Alice A",
				})
			},
			want: []string{
				"getent passwd alice",
				"addgroup -g 1000 alice",
				"adduser -u 1000 -h /home/alice -s /bin/sh -G alice -g Alice A -D -H alice",
			},
		},
		{
			name:    "EnsureUser present is idempotent",
			handler: present,
			do: func(p Provider) error {
				return p.EnsureUser(ctx, UserSpec{
					Name: "alice", UID: 1000, GID: 1000,
					Home: "/home/alice", Shell: "/bin/sh", FullName: "Alice A",
				})
			},
			want: []string{
				"getent passwd alice",
			},
		},
		{
			name: "LockPassword",
			do: func(p Provider) error {
				return p.LockPassword(ctx, "alice")
			},
			want: []string{
				"passwd -l alice",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &run.Fake{Handler: tt.handler}
			p := NewBusybox(fake)
			if err := tt.do(p); err != nil {
				t.Fatalf("action: %v", err)
			}
			if got := fake.Commands(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("commands =\n\t%#v\nwant\n\t%#v", got, tt.want)
			}
		})
	}
}

// busybox has no usermod: SetSupplementaryGroups must diff the desired set
// against the current membership (read via getent) and issue an addgroup for
// each new group and a delgroup for each removed group, in sorted order, while
// leaving unchanged memberships untouched.
func TestBusyboxSetSupplementaryGroups(t *testing.T) {
	// alice is currently in [team-a, team-x]; desired is [team-a, team-b].
	// Expect: add team-b, remove team-x, do not touch team-a.
	passwd := "alice:x:1000:1000:Alice A:/home/alice:/bin/sh\n"
	group := "" +
		"alice:x:1000:alice\n" + // primary group, excluded
		"team-a:x:2000:alice\n" +
		"team-x:x:2002:alice\n"

	fake := &run.Fake{Handler: getentHandler(passwd, group)}
	p := NewBusybox(fake)

	if err := p.SetSupplementaryGroups(context.Background(), "alice", []string{"team-a", "team-b"}); err != nil {
		t.Fatalf("SetSupplementaryGroups: %v", err)
	}

	want := []string{
		// scan reads passwd then group,
		"getent passwd",
		"getent group",
		// then the diff is applied: adds (sorted) before removes (sorted).
		"addgroup alice team-b",
		"delgroup alice team-x",
	}
	if got := fake.Commands(); !reflect.DeepEqual(got, want) {
		t.Errorf("commands =\n\t%#v\nwant\n\t%#v", got, want)
	}
}

// When the user is absent from the scan its current groups are treated as empty,
// so every desired group is added and nothing is removed.
func TestBusyboxSetSupplementaryGroupsUnknownUser(t *testing.T) {
	fake := &run.Fake{Handler: getentHandler("", "")}
	p := NewBusybox(fake)

	if err := p.SetSupplementaryGroups(context.Background(), "ghost", []string{"team-b", "team-a"}); err != nil {
		t.Fatalf("SetSupplementaryGroups: %v", err)
	}

	want := []string{
		"getent passwd",
		"getent group",
		"addgroup ghost team-a",
		"addgroup ghost team-b",
	}
	if got := fake.Commands(); !reflect.DeepEqual(got, want) {
		t.Errorf("commands =\n\t%#v\nwant\n\t%#v", got, want)
	}
}
