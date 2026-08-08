package roster

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// WithLock runs fn while holding an exclusive lock covering the roster at path.
//
// Editing the roster is read-modify-write, and without this two concurrent
// edits lose one of them silently: both read the same bytes, both compute a new
// document from them, and the second write replaces the first — the operator
// who added someone to a team sees it succeed and the membership is not there.
// That is a lost update, not a conflict, so nothing reports it.
//
// The existing lockRun in the CLI does not cover this. It is taken INSIDE
// `apply`, after any editing has already happened, and it is non-blocking, so
// it exists to stop two convergence runs from racing rather than to serialize
// writers of the declaration.
//
// The lock is a SIDECAR file, not the roster itself, because the roster is
// replaced by rename: a lock held on its inode says nothing about the file that
// takes its place. It is also advisory — it serializes writers that cooperate,
// and a hand edit with $EDITOR is not one of them. That is the same bargain
// every lockfile makes, and the roster stays hand-editable by design.
func WithLock(path string, fn func() error) error {
	lockPath := path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("roster: open lock %s: %w", lockPath, err)
	}
	defer f.Close()

	// Blocking, unlike the CLI's run lock. A second owner clicking "add member"
	// should wait a few milliseconds, not be told to try again — the operation
	// is short and the contention is real but rare.
	if err := flockWait(f, 5*time.Second); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	return fn()
}

// flockWait takes an exclusive lock, giving up after d.
//
// A bound rather than an indefinite wait: this runs inside an HTTP request in
// darak, and a request that hangs forever on a lock somebody else leaked is a
// worse failure than one that returns an error naming the lock.
func flockWait(f *os.File, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if err != syscall.EWOULDBLOCK {
			return fmt.Errorf("roster: lock %s: %w", f.Name(), err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("roster: another edit is in progress (waited %s for %s)", d, f.Name())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
