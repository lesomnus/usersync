package idrange

import "testing"

func defaultClassifier() *Classifier {
	return New(Config{
		SystemFloor: 1000,
		UID:         Set{Manage: Range{Min: 3000, Max: 6999}, Protect: []Range{{Min: 5000, Max: 5099}}},
		GID:         Set{Manage: Range{Min: 7000, Max: 7999}, Protect: []Range{{Min: 8000, Max: 8199}}},
	})
}

func TestClassifyUID(t *testing.T) {
	c := defaultClassifier()
	cases := []struct {
		id   uint32
		want Class
	}{
		{0, Protected},      // root
		{999, Protected},    // below floor
		{1000, OutOfScope},  // at floor but outside manage
		{2999, OutOfScope},  // just below manage
		{3000, Managed},     // manage lower bound
		{3001, Managed},     // normal user
		{4999, Managed},     // just below protect hole
		{5000, Protected},   // protect wins over manage (lower bound)
		{5099, Protected},   // protect upper bound
		{5100, Managed},     // just above protect hole, still in manage
		{6999, Managed},     // manage upper bound
		{7000, OutOfScope},  // above manage
		{9000, OutOfScope},  // far out
	}
	for _, tc := range cases {
		if got := c.UID(tc.id); got != tc.want {
			t.Errorf("UID(%d) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestClassifyGID(t *testing.T) {
	c := defaultClassifier()
	cases := []struct {
		id   uint32
		want Class
	}{
		{999, Protected},
		{7000, Managed},
		{7999, Managed},
		{8000, Protected},  // gid protect range
		{8199, Protected},
		{8200, OutOfScope},
	}
	for _, tc := range cases {
		if got := c.GID(tc.id); got != tc.want {
			t.Errorf("GID(%d) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestSystemFloorClampedUp(t *testing.T) {
	// A config trying to set the floor below the hard floor must be clamped up.
	c := New(Config{SystemFloor: 500, UID: Set{Manage: Range{Min: 3000, Max: 6999}}})
	if got := c.SystemFloor(); got != HardFloor {
		t.Fatalf("SystemFloor() = %d, want %d (clamped)", got, HardFloor)
	}
	if got := c.UID(700); got != Protected {
		t.Errorf("UID(700) = %v, want Protected (floor clamped to 1000)", got)
	}
}

func TestSystemFloorRaised(t *testing.T) {
	// A higher floor is honored (more restrictive).
	c := New(Config{SystemFloor: 2000, UID: Set{Manage: Range{Min: 1500, Max: 6999}}})
	if got := c.SystemFloor(); got != 2000 {
		t.Fatalf("SystemFloor() = %d, want 2000", got)
	}
	if got := c.UID(1800); got != Protected {
		t.Errorf("UID(1800) = %v, want Protected (below raised floor)", got)
	}
	if got := c.UID(2500); got != Managed {
		t.Errorf("UID(2500) = %v, want Managed", got)
	}
}

func TestRangeContains(t *testing.T) {
	r := Range{Min: 10, Max: 20}
	for _, id := range []uint32{10, 15, 20} {
		if !r.Contains(id) {
			t.Errorf("Contains(%d) = false, want true", id)
		}
	}
	for _, id := range []uint32{9, 21, 0} {
		if r.Contains(id) {
			t.Errorf("Contains(%d) = true, want false", id)
		}
	}
}
