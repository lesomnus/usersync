package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/state"
)

// scanViaGetent reads every passwd and group entry via getent and builds the
// actual State. It is the shared Scan implementation for every backend whose
// name service exposes getent (shadow-utils, busybox, FreeBSD). It does not
// filter by id range — the caller classifies. A user's supplementary groups are
// the groups whose member list names the user, excluding the user's primary
// group. Smb is left empty. Malformed lines (too few fields or unparseable
// uid/gid) are skipped.
func scanViaGetent(ctx context.Context, r run.Runner) (*state.State, error) {
	st := state.New()

	passwd, err := r.Run(ctx, "", "getent", "passwd")
	if err != nil {
		return nil, fmt.Errorf("getent passwd: %w", err)
	}
	group, err := r.Run(ctx, "", "getent", "group")
	if err != nil {
		return nil, fmt.Errorf("getent group: %w", err)
	}

	// passwd line: name:x:uid:gid:gecos:home:shell
	for _, line := range strings.Split(passwd, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// name:x:uid:gid:gecos:home:shell — the GECOS field may itself contain a
		// colon, so parse the fixed fields from both ends and treat everything in
		// between as the gecos.
		f := strings.Split(line, ":")
		if len(f) < 7 {
			continue
		}
		uid, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			continue
		}
		gid, err := strconv.ParseUint(f[3], 10, 32)
		if err != nil {
			continue
		}
		name := f[0]
		shell := f[len(f)-1]
		home := f[len(f)-2]
		gecos := strings.Join(f[4:len(f)-2], ":")
		st.Users[name] = state.User{
			Name:     name,
			UID:      uint32(uid),
			GID:      uint32(gid),
			FullName: gecos,
			Home:     home,
			Shell:    shell,
		}
	}

	// group line: name:x:gid:member1,member2 (members may be empty)
	for _, line := range strings.Split(group, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) < 4 {
			continue
		}
		gid, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			continue
		}
		name := f[0]
		st.Groups[name] = state.Group{Name: name, GID: uint32(gid)}

		if f[3] == "" {
			continue
		}
		for _, member := range strings.Split(f[3], ",") {
			member = strings.TrimSpace(member)
			if member == "" {
				continue
			}
			u, ok := st.Users[member]
			if !ok {
				continue
			}
			// A membership that names the user's own primary group is not a
			// supplementary group.
			if uint32(gid) == u.GID {
				continue
			}
			u.Groups = append(u.Groups, name)
			st.Users[member] = u
		}
	}

	return st, nil
}
