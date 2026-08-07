package provider

import (
	"context"
	"strconv"
	"strings"

	"github.com/lesomnus/usersync/internal/run"
)

// lookupViaGetent resolves ONE name through NSS and returns its numeric id.
//
// This is deliberately a keyed lookup rather than a scan of the enumeration,
// because the two answer different questions once a directory service is in the
// picture. `getent passwd` with no key asks every NSS module to list everything
// it has, and winbind does not do that unless `winbind enum users = yes` (sssd
// likewise defaults `enumerate = false`) — the cost of enumerating a domain is
// why. `getent passwd <name>` asks the modules to resolve one name, which they
// always answer.
//
// So enumeration cannot be used to check whether a declared account exists after
// a handover: every domain-served user would look absent. That is the whole
// reason this exists next to scanViaGetent.
//
// db is "passwd" or "group". A name that does not resolve is not an error.
func lookupViaGetent(ctx context.Context, r run.Runner, db, name string) (uint32, bool, error) {
	out, err := r.Run(ctx, "", "getent", db, name)
	if err != nil {
		// getent exits non-zero when the key is unknown, which is an answer, not a
		// failure. There is no way to tell that apart from a broken NSS module
		// through the exit status alone, so treat both as "not found" and let the
		// caller's other signals speak.
		return 0, false, nil
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return 0, false, nil
	}
	// Only the first line matters; a key resolves to at most one entry per module
	// but several modules may answer, and NSS order decides the winner.
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	// passwd: name:x:uid:gid:...   group: name:x:gid:members
	f := strings.Split(line, ":")
	if len(f) < 3 {
		return 0, false, nil
	}
	id, err := strconv.ParseUint(f[2], 10, 32)
	if err != nil {
		return 0, false, nil
	}
	return uint32(id), true, nil
}
