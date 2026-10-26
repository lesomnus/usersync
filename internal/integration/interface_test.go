package integration

import (
	"os"
	"path/filepath"
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
//   - `quota` is present only when a limit applies, and 0 is a limit.
const wantRoster = `{
  "schema": 1,
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
