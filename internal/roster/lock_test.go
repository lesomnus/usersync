package roster

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Two owners adding two different people to two different teams at the same
// time. Without a lock this is a lost update, not a conflict: both read the
// same bytes, both compute a new document from them, and the second write
// replaces the first -- so one owner is told their change succeeded and it is
// not there. Nothing reports it, which is what makes it worth a test.
func TestConcurrentEditsDoNotLoseOneAnother(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roster.yaml")
	if err := os.WriteFile(path, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}

	// The same read-modify-write an editing caller performs.
	edit := func(user, group string) error {
		return WithLock(path, func() error {
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			d, err := ParseDocument(src)
			if err != nil {
				return err
			}
			if _, err := d.AddGroup(user, group); err != nil {
				return err
			}
			// A pause inside the critical section: without the lock this is what
			// makes the interleaving certain rather than merely likely.
			time.Sleep(20 * time.Millisecond)
			return WriteFile(path, []byte(d.String()))
		})
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, tc := range []struct{ user, group string }{
		{"ychoi", "team-b"},
		{"park", "team-a"},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = edit(tc.user, tc.group)
		}()
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseDocument(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ user, want string }{
		{"ychoi", "team-b"},
		{"park", "team-a"},
	} {
		got, err := d.Groups(tc.user)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, g := range got {
			if g == tc.want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s groups = %v, want it to contain %q -- this edit was lost", tc.user, got, tc.want)
		}
	}
}

// The lock is on a SIDECAR file, because WriteFile replaces the roster by
// rename: a lock held on the roster's own inode says nothing about the file
// that takes its place.
func TestLockSurvivesTheAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roster.yaml")
	if err := os.WriteFile(path, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}

	held := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithLock(path, func() error {
			// Replace the file while holding the lock, then keep holding it.
			if err := WriteFile(path, []byte(sample)); err != nil {
				return err
			}
			close(held)
			time.Sleep(150 * time.Millisecond)
			return nil
		})
	}()

	<-held
	start := time.Now()
	if err := WithLock(path, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	// The second caller must have waited: if the rename had released the lock it
	// would have returned immediately.
	if waited := time.Since(start); waited < 50*time.Millisecond {
		t.Errorf("second lock acquired after %s; the rename released it", waited)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
