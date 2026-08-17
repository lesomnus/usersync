package quota

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/lesomnus/usersync/internal/run"
)

func TestEnforceBytes(t *testing.T) {
	// A declared 0 must never reach ZFS as 0 (which means "no limit"); it becomes
	// the smallest real cap. Everything else is itself.
	cases := map[uint64]uint64{0: 1, 1: 1, 100: 100, 1 << 30: 1 << 30}
	for in, want := range cases {
		if got := EnforceBytes(in); got != want {
			t.Errorf("EnforceBytes(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestNopReportsUnsupported(t *testing.T) {
	n := Nop{}
	ctx := context.Background()
	if err := n.Probe(ctx); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Nop.Probe = %v, want ErrUnsupported", err)
	}
	if _, err := n.List(ctx); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Nop.List = %v, want ErrUnsupported", err)
	}
	if err := n.Set(ctx, 3001, 100); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Nop.Set = %v, want ErrUnsupported", err)
	}
	if err := n.Clear(ctx, 3001); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Nop.Clear = %v, want ErrUnsupported", err)
	}
}

func TestZFSSetUsesUserquotaProperty(t *testing.T) {
	f := &run.Fake{}
	z := ZFS{Runner: f, Dataset: "tank/nas"}
	if err := z.Set(context.Background(), 3067, 100); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(append([]string{f.Calls[0].Name}, f.Calls[0].Args...), " ")
	if got != "zfs set userquota@3067=100 tank/nas" {
		t.Errorf("Set ran %q", got)
	}
}

func TestZFSSetZeroEnforcedAsOne(t *testing.T) {
	// The intern case: quota: 0 must land as a 1-byte cap, not ZFS's "no limit".
	f := &run.Fake{}
	z := ZFS{Runner: f, Dataset: "tank/nas"}
	if err := z.Set(context.Background(), 3067, 0); err != nil {
		t.Fatal(err)
	}
	if got := f.Calls[0].Args[1]; got != "userquota@3067=1" {
		t.Errorf("zero quota set as %q, want userquota@3067=1", got)
	}
}

func TestZFSClearUsesNone(t *testing.T) {
	f := &run.Fake{}
	z := ZFS{Runner: f, Dataset: "tank/nas"}
	if err := z.Clear(context.Background(), 3067); err != nil {
		t.Fatal(err)
	}
	if got := f.Calls[0].Args[1]; got != "userquota@3067=none" {
		t.Errorf("clear set as %q, want userquota@3067=none", got)
	}
}

func TestZFSListParsesUserspace(t *testing.T) {
	// `zfs userspace -Hnp -o name,quota`: tab-separated uid<TAB>quota. "none" and 0
	// mean no limit and are dropped; only real caps are returned.
	out := "3001\t107374182400\n3067\t1\n3002\tnone\n3003\t0\n"
	f := &run.Fake{Handler: func(_, name string, args ...string) (string, error) {
		return out, nil
	}}
	z := ZFS{Runner: f, Dataset: "tank/nas"}
	got, err := z.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint32]uint64{3001: 107374182400, 3067: 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}
}

func TestZFSProbePropagatesError(t *testing.T) {
	f := &run.Fake{Handler: func(_, name string, args ...string) (string, error) {
		return "", errors.New("cannot open 'tank/nas': dataset does not exist")
	}}
	z := ZFS{Runner: f, Dataset: "tank/nas"}
	if err := z.Probe(context.Background()); err == nil {
		t.Fatal("Probe should fail when the dataset is missing")
	}
}
