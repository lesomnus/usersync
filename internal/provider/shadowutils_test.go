package provider

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/state"
)

// getentHandler returns a run.Fake handler that serves canned `getent passwd`
// and `getent group` output and empty for everything else.
func getentHandler(passwd, group string) func(stdin, name string, args ...string) (string, error) {
	return func(stdin, name string, args ...string) (string, error) {
		if name == "getent" && len(args) == 1 {
			switch args[0] {
			case "passwd":
				return passwd, nil
			case "group":
				return group, nil
			}
		}
		return "", nil
	}
}

func TestShadowUtilsScan(t *testing.T) {
	// alice's own group (gid 1000) lists her as a member; it is her primary
	// group and must be excluded from supplementary groups. wheel and dev are
	// genuine supplementary groups. Malformed lines are skipped.
	passwd := "" +
		"root:x:0:0:root:/root:/bin/bash\n" +
		"alice:x:1000:1000:Alice A:/home/alice:/bin/bash\n" +
		"bob:x:1001:1001::/home/bob:/bin/sh\n" +
		"short:x:5\n" + // too few fields
		"nope:x:abc:1:x:/x:/x\n" // unparseable uid

	group := "" +
		"root:x:0:\n" +
		"alice:x:1000:alice\n" + // primary group of alice, excluded
		"bob:x:1001:\n" +
		"wheel:x:10:alice,bob\n" +
		"dev:x:2000:alice\n" +
		"weird:x:notanum:alice\n" + // unparseable gid
		"badgroup:x\n" // too few fields

	fake := &run.Fake{Handler: getentHandler(passwd, group)}
	p := NewShadowUtils(fake)

	st, err := p.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// Users: root, alice, bob (short/nope skipped).
	if len(st.Users) != 3 {
		t.Fatalf("users = %d, want 3: %#v", len(st.Users), st.Users)
	}
	wantAlice := state.User{
		Name:     "alice",
		UID:      1000,
		GID:      1000,
		Groups:   []string{"wheel", "dev"}, // primary group "alice" excluded, order preserved
		FullName: "Alice A",
		Home:     "/home/alice",
		Shell:    "/bin/bash",
	}
	if got := st.Users["alice"]; !reflect.DeepEqual(got, wantAlice) {
		t.Errorf("alice = %#v, want %#v", got, wantAlice)
	}
	wantBob := state.User{
		Name:   "bob",
		UID:    1001,
		GID:    1001,
		Groups: []string{"wheel"},
		Home:   "/home/bob",
		Shell:  "/bin/sh",
	}
	if got := st.Users["bob"]; !reflect.DeepEqual(got, wantBob) {
		t.Errorf("bob = %#v, want %#v", got, wantBob)
	}

	// Groups: root, alice, bob, wheel, dev (weird/badgroup skipped).
	wantGroups := map[string]state.Group{
		"root":  {Name: "root", GID: 0},
		"alice": {Name: "alice", GID: 1000},
		"bob":   {Name: "bob", GID: 1001},
		"wheel": {Name: "wheel", GID: 10},
		"dev":   {Name: "dev", GID: 2000},
	}
	if !reflect.DeepEqual(st.Groups, wantGroups) {
		t.Errorf("groups = %#v, want %#v", st.Groups, wantGroups)
	}

	if len(st.Smb) != 0 {
		t.Errorf("smb = %#v, want empty", st.Smb)
	}
}

func TestShadowUtilsCommands(t *testing.T) {
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
				"groupadd -g 5000 teams",
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
					Home: "/home/alice", Shell: "/bin/bash", FullName: "Alice A",
				})
			},
			want: []string{
				"getent passwd alice",
				"getent group alice",
				"groupadd -g 1000 alice",
				"useradd -u 1000 -g 1000 -M -d /home/alice -s /bin/bash -c Alice A alice",
			},
		},
		{
			name:    "EnsureUser present is idempotent",
			handler: present,
			do: func(p Provider) error {
				return p.EnsureUser(ctx, UserSpec{
					Name: "alice", UID: 1000, GID: 1000,
					Home: "/home/alice", Shell: "/bin/bash", FullName: "Alice A",
				})
			},
			want: []string{
				"getent passwd alice",
			},
		},
		{
			name: "SetSupplementaryGroups replaces",
			do: func(p Provider) error {
				return p.SetSupplementaryGroups(ctx, "alice", []string{"wheel", "dev"})
			},
			want: []string{
				"usermod -G wheel,dev alice",
			},
		},
		{
			name: "SetSupplementaryGroups empty clears",
			do: func(p Provider) error {
				return p.SetSupplementaryGroups(ctx, "alice", nil)
			},
			// empty argument renders as an extra space between -G and the user.
			want: []string{
				"usermod -G  alice",
			},
		},
		{
			name: "LockPassword",
			do: func(p Provider) error {
				return p.LockPassword(ctx, "alice")
			},
			want: []string{
				"usermod -L alice",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &run.Fake{Handler: tt.handler}
			p := NewShadowUtils(fake)
			if err := tt.do(p); err != nil {
				t.Fatalf("action: %v", err)
			}
			if got := fake.Commands(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("commands =\n\t%#v\nwant\n\t%#v", got, tt.want)
			}
		})
	}
}

// A GECOS field can itself contain a colon; Scan must parse the fixed home and
// shell fields from the end and treat the middle as the gecos.
func TestShadowUtilsScanGecosWithColon(t *testing.T) {
	fake := &run.Fake{Handler: func(_, name string, args ...string) (string, error) {
		switch strings.Join(append([]string{name}, args...), " ") {
		case "getent passwd":
			return "carol:x:3007:3007:Carol C, room 1:2:/research/home/carol:/usr/sbin/nologin\n", nil
		case "getent group":
			return "carol:x:3007:\n", nil
		}
		return "", nil
	}}
	st, err := NewShadowUtils(fake).(*shadowUtils).Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u := st.Users["carol"]
	if u.FullName != "Carol C, room 1:2" {
		t.Errorf("FullName = %q, want %q", u.FullName, "Carol C, room 1:2")
	}
	if u.Home != "/research/home/carol" || u.Shell != "/usr/sbin/nologin" {
		t.Errorf("home/shell mis-parsed: home=%q shell=%q", u.Home, u.Shell)
	}
}

func TestShadowUtilsEnsureUserSkipsExistingUPG(t *testing.T) {
	// user absent but its UPG group already exists (interrupted prior apply):
	// groupadd must be skipped, useradd still run.
	fake := &run.Fake{Handler: func(_, name string, args ...string) (string, error) {
		cmd := strings.Join(append([]string{name}, args...), " ")
		switch cmd {
		case "getent passwd alice":
			return "", nil // user absent
		case "getent group alice":
			return "alice:x:3001:", nil // UPG already present
		}
		return "", nil
	}}
	err := NewShadowUtils(fake).EnsureUser(context.Background(), UserSpec{Name: "alice", UID: 3001, GID: 3001, Home: "/h", Shell: "/s"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fake.Commands() {
		if strings.HasPrefix(c, "groupadd ") {
			t.Errorf("groupadd must be skipped when UPG exists, got %q", c)
		}
	}
	if !slices.ContainsFunc(fake.Commands(), func(c string) bool { return strings.HasPrefix(c, "useradd ") }) {
		t.Error("useradd must still run")
	}
}
