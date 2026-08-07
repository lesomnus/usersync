package provider

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// defaultEtc is where the local account databases live.
const defaultEtc = "/etc"

// localEntry looks a name up in the LOCAL account database only — /etc/passwd or
// /etc/group — and returns its numeric id.
//
// This is deliberately not a getent probe. userdel/deluser/pw userdel only touch
// the local files, so asking NSS instead answers a different question: on a
// domain-joined host a directory-served name reports present, the delete then
// fails because there is nothing local to delete, and an operation documented as
// idempotent errors out. That is not a corner case — it is what happens the
// second time detach is run on a user winbind has already taken over, and what
// happens to an interrupted detach that is retried.
//
// db is "passwd" or "group". A missing file means no local entry.
func localEntry(etc, db, name string) (id uint32, found bool) {
	if etc == "" {
		etc = defaultEtc
	}
	f, err := os.Open(filepath.Join(etc, db))
	if err != nil {
		return 0, false
	}
	defer f.Close()

	prefix := name + ":"
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		// passwd: name:x:uid:gid:...   group: name:x:gid:members
		fields := strings.Split(line, ":")
		if len(fields) < 3 {
			continue
		}
		v, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			continue
		}
		return uint32(v), true
	}
	return 0, false
}

// isUPG reports whether the local group `name` is this user's private group,
// i.e. it exists locally and its gid equals the user's uid.
//
// RemoveAccount must not delete a group merely because it shares the user's
// name. A hand-made group can do that while sitting on a real team gid — audit
// reports exactly that as an "undeclared" group — and deleting it would destroy
// a shared team group and its member list, leaving the group folder owned by a
// gid that resolves to nothing, with nothing in the run saying so.
func isUPG(etc, user string, uid uint32) bool {
	gid, found := localEntry(etc, "group", user)
	return found && gid == uid
}
