// Package quota is the filesystem seam for per-uid disk quotas. usersync's other
// enforcement (home directories, reader ACLs) goes through fsops, which assumes
// POSIX operations every backend shares. Quota is separate because it is NOT a
// POSIX operation: every filesystem exposes it differently — ZFS as a native
// `userquota@<uid>` property, xfs as project quotas, btrfs as per-subvolume
// qgroups — and some not at all. The Controller interface hides that. The
// reconciler proposes "uid U should be capped at N bytes" and a backend either
// enforces it or reports it cannot, the same shape as the reader ACL.
package quota

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/lesomnus/usersync/internal/run"
)

// ErrUnsupported is returned by a backend that cannot enforce per-uid quotas. It
// is not a failure: on it, the reconciler proposes no quota actions rather than
// declaring a limit nothing enforces — the same rule the reader ACL keeps ("a
// folder that looks restricted and is not is worse than an error").
var ErrUnsupported = errors.New("per-uid quota not supported by this backend")

// Controller sets, clears, and reads per-uid byte quotas on the managed store.
type Controller interface {
	// Probe verifies the backend can enforce quotas right now: the tools are
	// present, they can reach the kernel, and the target exists. It is called once
	// before any quota work so a broken backend fails loudly and up front — in a
	// container this is where a ZFS userspace/kernel version skew too wide to talk
	// over /dev/zfs would surface, instead of every quota silently going unset.
	Probe(ctx context.Context) error
	// List returns the byte limit in force for every uid that has one; a uid absent
	// from the map has no limit. It is read once per reconcile so the diff is one
	// backend call, not one per user.
	List(ctx context.Context) (map[uint32]uint64, error)
	// Set caps uid at the declared byte limit. A declared 0 is enforced as the
	// smallest real limit the backend can express (see EnforceBytes) — never as
	// "unlimited".
	Set(ctx context.Context, uid uint32, bytes uint64) error
	// Clear removes any limit on uid (back to unlimited).
	Clear(ctx context.Context, uid uint32) error
}

// EnforceBytes maps a declared limit to the value a backend actually enforces. A
// declared 0 means "no writable space", but it cannot be sent to ZFS verbatim:
// there `userquota@uid=0` means "no limit" — the exact opposite. So a declared 0
// is enforced as 1 byte, the smallest real cap; the account still cannot create
// a file (any file's data pushes usage past one byte). Every other value is
// itself. The reconciler compares in this same enforced space, so a user
// declared `quota: 0` reads back as a 1-byte limit and does not re-drift.
func EnforceBytes(declared uint64) uint64 {
	if declared == 0 {
		return 1
	}
	return declared
}

// Nop is the backend when no quota is configured: it enforces nothing and says
// so, so the reconciler proposes no quota actions and the field is inert.
type Nop struct{}

func (Nop) Probe(context.Context) error                     { return ErrUnsupported }
func (Nop) List(context.Context) (map[uint32]uint64, error) { return nil, ErrUnsupported }
func (Nop) Set(context.Context, uint32, uint64) error       { return ErrUnsupported }
func (Nop) Clear(context.Context, uint32) error             { return ErrUnsupported }

// ZFS enforces per-uid quotas via the native `userquota@<uid>` property on a
// dataset. It shells out through a run.Runner, the same execution path (and the
// same test seam) as usersync's other backends.
type ZFS struct {
	Runner  run.Runner
	Dataset string
}

// Probe lists the dataset. Success proves the zfs tool is present, can reach the
// pool over /dev/zfs, and the dataset exists — the three ways the container path
// can be misconfigured, caught at once.
func (z ZFS) Probe(ctx context.Context) error {
	if _, err := z.Runner.Run(ctx, "", "zfs", "list", "-H", "-o", "name", z.Dataset); err != nil {
		return fmt.Errorf("zfs quota backend unavailable for dataset %q: %w", z.Dataset, err)
	}
	return nil
}

// List reads every user's quota in one call. `-n` keeps the owner numeric (a
// uid, not a resolved name), `-p` prints exact bytes; a user with no quota shows
// as "none"/"0" and is left out of the map.
func (z ZFS) List(ctx context.Context) (map[uint32]uint64, error) {
	out, err := z.Runner.Run(ctx, "", "zfs", "userspace", "-Hnp", "-o", "name,quota", z.Dataset)
	if err != nil {
		return nil, fmt.Errorf("zfs userspace %q: %w", z.Dataset, err)
	}
	res := map[uint32]uint64{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		uid, err := strconv.ParseUint(f[0], 10, 32)
		if err != nil {
			continue // non-numeric owner (a group row, or a name -n could not resolve)
		}
		q, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil || q == 0 {
			continue // "none"/"0"/"-": no limit in force
		}
		res[uint32(uid)] = q
	}
	return res, nil
}

// Set writes `userquota@<uid>=<bytes>` on the dataset. Bytes is run through
// EnforceBytes so a declared 0 becomes an enforced 1, never ZFS's "no limit".
func (z ZFS) Set(ctx context.Context, uid uint32, bytes uint64) error {
	prop := fmt.Sprintf("userquota@%d=%d", uid, EnforceBytes(bytes))
	_, err := z.Runner.Run(ctx, "", "zfs", "set", prop, z.Dataset)
	return err
}

// Clear sets the uid's quota back to `none` (unlimited).
func (z ZFS) Clear(ctx context.Context, uid uint32) error {
	prop := fmt.Sprintf("userquota@%d=none", uid)
	_, err := z.Runner.Run(ctx, "", "zfs", "set", prop, z.Dataset)
	return err
}
