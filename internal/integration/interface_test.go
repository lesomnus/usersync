package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// These pin what docs/interface.md promises to programs outside this
// repository. darak shells out to `usersync roster` and parses stdout, so the
// shape below is an interface in the same sense a flag name is: a field renamed
// here breaks a reader that compiles fine and fails only when it runs.
//
// The comparison is byte for byte on purpose. A change that makes this test
// fail is either additive — update the expectation and the field table in
// docs/interface.md together — or it is not, and then it also bumps `schema`.

// interfaceFixture covers every field the roster output can carry: owners,
// readers, an `all` cohort, an anonymous group, a profile with no home and a
// quota, and each user status.
func interfaceFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("usersync.yaml", `paths:
  home: /srv/data/homes
  groups: /srv/data/teams
mode: manage
provider: shadow-utils
`)
	write("roster.yaml", `profiles:
  intern: { home: false, quota: 0 }
groups:
  - name: team-a
    gid: 10001
    description: Perception team
    owners: [skim]
    members: [skim, jlee]
    readers: [team-a-ro]
  - name: team-a-ro
    gid: 10002
    members: [intern1]
  - name: everyone
    gid: 10003
    all: default
  - name: public
    gid: 19999
    anonymous: read
users:
  - name: skim
    uid: 3001
    full_name: Sunghyun Kim
  - name: jlee
    uid: 3002
    status: disabled
  - name: intern1
    uid: 3003
    profile: intern
  - name: oldhand
    uid: 3005
    status: reserved
`)
	return dir
}

// What the fixture shows about the contract, beyond field names:
//   - groups and users come out in roster order;
//   - `members` of an ordinary group is the declaration as written, so a
//     disabled user is still listed — status is the user's to say;
//   - `members` of an `all` group is the resolved cohort: active users of that
//     profile only (not jlee, disabled; not intern1, profile intern);
//   - list fields are `[]`, never `null`;
//   - `quota` is present only when a limit applies, and 0 is a limit;
//   - `paths` are the configured bases, and `views` is readers inverted: on
//     team-a-ro, the team it may read and the path apply mounts that view at.
const wantRoster = `{
  "schema": 1,
  "paths": {
    "home": "/srv/data/homes",
    "groups": "/srv/data/teams"
  },
  "groups": [
    {
      "name": "team-a",
      "gid": 10001,
      "description": "Perception team",
      "owners": [
        "skim"
      ],
      "readers": [
        "team-a-ro"
      ],
      "views": [],
      "members": [
        "skim",
        "jlee"
      ],
      "anonymous": "none"
    },
    {
      "name": "team-a-ro",
      "gid": 10002,
      "owners": [],
      "readers": [],
      "views": [
        {
          "team": "team-a",
          "path": "/srv/data/teams/team-a-ro/team-a"
        }
      ],
      "members": [
        "intern1"
      ],
      "anonymous": "none"
    },
    {
      "name": "everyone",
      "gid": 10003,
      "owners": [],
      "readers": [],
      "views": [],
      "members": [
        "skim"
      ],
      "all": true,
      "anonymous": "none"
    },
    {
      "name": "public",
      "gid": 19999,
      "owners": [],
      "readers": [],
      "views": [],
      "members": [],
      "anonymous": "read"
    }
  ],
  "users": [
    {
      "name": "skim",
      "uid": 3001,
      "full_name": "Sunghyun Kim",
      "status": "active",
      "home": true
    },
    {
      "name": "jlee",
      "uid": 3002,
      "status": "disabled",
      "home": true
    },
    {
      "name": "intern1",
      "uid": 3003,
      "status": "active",
      "home": false,
      "quota": 0
    },
    {
      "name": "oldhand",
      "uid": 3005,
      "status": "reserved",
      "home": true
    }
  ]
}
`

func TestInterfaceRosterOutput(t *testing.T) {
	bin, dir := binary(t), interfaceFixture(t)

	r := runCLI(t, bin, dir, "roster")
	if r.code != 0 {
		t.Fatalf("roster exited %d\nstderr: %s", r.code, r.stderr)
	}
	// stdout carries the JSON and nothing else; logs go to stderr. A consumer
	// parses stdout whole, so one stray line there is a parse error.
	if r.stdout != wantRoster {
		t.Errorf("roster stdout differs from the documented interface (docs/interface.md).\n--- got\n%s\n--- want\n%s", r.stdout, wantRoster)
	}
}

// With paths.views set, the view a consumer is sent to is in the namespace, and
// `shares` serves it as [<reader>.read]. The legacy views stay mounted meanwhile
// (legacy_views defaults to true), but they are no longer what anybody is told.
func TestInterfaceViewsMoveWithPathsViews(t *testing.T) {
	bin, dir := binary(t), interfaceFixture(t)
	cfg := `paths:
  home: /srv/data/homes
  groups: /srv/data/teams
  views: /srv/data/views
mode: manage
provider: shadow-utils
`
	if err := os.WriteFile(filepath.Join(dir, "usersync.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	r := runCLI(t, bin, dir, "roster")
	if r.code != 0 {
		t.Fatalf("roster exited %d\nstderr: %s", r.code, r.stderr)
	}
	var out struct {
		Paths struct {
			Views string `json:"views"`
		} `json:"paths"`
		Groups []struct {
			Name  string `json:"name"`
			Views []struct {
				Team, Path string
			} `json:"views"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("roster stdout is not JSON: %v\n%s", err, r.stdout)
	}
	if out.Paths.Views != "/srv/data/views" {
		t.Errorf("paths.views = %q", out.Paths.Views)
	}
	var got string
	for _, g := range out.Groups {
		if g.Name == "team-a-ro" && len(g.Views) == 1 {
			got = g.Views[0].Path
		}
	}
	if got != "/srv/data/views/team-a-ro/team-a" {
		t.Errorf("team-a-ro's view path = %q; want it in the view namespace", got)
	}

	s := runCLI(t, bin, dir, "shares")
	if s.code != 0 {
		t.Fatalf("shares exited %d\nstderr: %s", s.code, s.stderr)
	}
	for _, want := range []string{"[team-a-ro.read]", "path = /srv/data/views/team-a-ro", "read only = yes", "valid users = @team-a-ro"} {
		if !strings.Contains(s.stdout, want) {
			t.Errorf("shares output lacks %q:\n%s", want, s.stdout)
		}
	}
	if strings.Contains(s.stdout, "[team-a.read]") {
		t.Errorf("[team-a.read] rendered, but team-a reads nothing")
	}
}

// read_all comes out resolved, as `all` does: the folders it reaches list it in
// readers, and it lists them all in views. A consumer that only knows schema 1
// therefore routes a read_all reader with no change. Without the view namespace
// alone, the roster is refused rather than failing at apply.
func TestInterfaceReadAll(t *testing.T) {
	bin, dir := binary(t), interfaceFixture(t)
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "roster.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	write("roster.yaml", strings.Replace(string(b), "users:\n", `  - name: audit
    gid: 10090
    read_all: true
    members: [skim]
users:
`, 1))

	cfg := "paths: { home: /srv/data/homes, groups: /srv/data/teams, views: /srv/data/views }\nmode: manage\nprovider: shadow-utils\n"
	for _, legacy := range []string{"", "legacy_views: true\n"} {
		write("usersync.yaml", cfg+legacy)
		if r := runCLI(t, bin, dir, "roster"); r.code == 0 || !strings.Contains(r.stderr, "legacy_views: false") {
			t.Errorf("read_all with legacy views (%q) was not refused: exit %d\n%s", legacy, r.code, r.stderr)
		}
	}

	write("usersync.yaml", cfg+"legacy_views: false\n")
	r := runCLI(t, bin, dir, "roster")
	if r.code != 0 {
		t.Fatalf("roster exited %d\nstderr: %s", r.code, r.stderr)
	}
	var out struct {
		Groups []struct {
			Name    string   `json:"name"`
			Readers []string `json:"readers"`
			ReadAll bool     `json:"read_all"`
			Views   []struct {
				Team, Path string
			} `json:"views"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("roster stdout is not JSON: %v\n%s", err, r.stdout)
	}
	readers, views := map[string][]string{}, []string{}
	for _, g := range out.Groups {
		readers[g.Name] = g.Readers
		if g.Name == "audit" {
			if !g.ReadAll {
				t.Error("audit lacks read_all: true")
			}
			for _, v := range g.Views {
				views = append(views, v.Team+"="+v.Path)
			}
		}
	}
	want := map[string][]string{
		"team-a":    {"team-a-ro", "audit"},
		"team-a-ro": {"audit"},
		"everyone":  {"audit"},
		"public":    {}, // world-open already
		"audit":     {},
	}
	for g, w := range want {
		if !reflect.DeepEqual(readers[g], w) {
			t.Errorf("%s readers = %v; want %v", g, readers[g], w)
		}
	}
	wantViews := []string{
		"everyone=/srv/data/views/audit/everyone",
		"team-a=/srv/data/views/audit/team-a",
		"team-a-ro=/srv/data/views/audit/team-a-ro",
	}
	if !reflect.DeepEqual(views, wantViews) {
		t.Errorf("audit views = %v; want %v", views, wantViews)
	}

	s := runCLI(t, bin, dir, "shares")
	if s.code != 0 || !strings.Contains(s.stdout, "[audit.read]") {
		t.Errorf("no [audit.read] (exit %d):\n%s%s", s.code, s.stdout, s.stderr)
	}
}
