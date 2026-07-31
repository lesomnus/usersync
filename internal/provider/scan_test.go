package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/state"
)

// scanViaGetent must exclude the user's primary group from its supplementary
// groups and keep genuine supplementary memberships in the order groups appear.
func TestScanViaGetent(t *testing.T) {
	passwd := "" +
		"alice:x:1000:1000:Alice A:/home/alice:/bin/sh\n"
	group := "" +
		"alice:x:1000:alice\n" + // primary group of alice, excluded
		"team-a:x:2000:alice\n" +
		"team-b:x:2001:alice\n"

	fake := &run.Fake{Handler: getentHandler(passwd, group)}
	st, err := scanViaGetent(context.Background(), fake)
	if err != nil {
		t.Fatalf("scanViaGetent: %v", err)
	}

	want := state.User{
		Name:     "alice",
		UID:      1000,
		GID:      1000,
		Groups:   []string{"team-a", "team-b"}, // primary "alice" excluded, order preserved
		FullName: "Alice A",
		Home:     "/home/alice",
		Shell:    "/bin/sh",
	}
	if got := st.Users["alice"]; !reflect.DeepEqual(got, want) {
		t.Errorf("alice = %#v, want %#v", got, want)
	}
	if len(st.Smb) != 0 {
		t.Errorf("smb = %#v, want empty", st.Smb)
	}
}

// A GECOS field can itself contain a colon; scanViaGetent must parse the fixed
// home and shell fields from the end and treat the middle as the gecos.
func TestScanViaGetentGecosWithColon(t *testing.T) {
	fake := &run.Fake{Handler: func(_, name string, args ...string) (string, error) {
		switch strings.Join(append([]string{name}, args...), " ") {
		case "getent passwd":
			return "carol:x:3007:3007:Carol C, room 1:2:/research/home/carol:/usr/sbin/nologin\n", nil
		case "getent group":
			return "carol:x:3007:\n", nil
		}
		return "", nil
	}}
	st, err := scanViaGetent(context.Background(), fake)
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
