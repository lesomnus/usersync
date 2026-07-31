// Package idrange classifies unix uid/gid values into how usersync is allowed
// to treat them: Protected (never touch), Managed (reconcile), or OutOfScope.
//
// The rules (see plan.md §4 "관리 범위 & 보호 범위"):
//   - Protected wins over Managed: an id inside a protect range is Protected
//     even if it also falls inside the manage window.
//   - Any id below the system floor is Protected. The floor defaults to and is
//     clamped up to at least HardFloor (1000): config may raise it (more
//     restrictive) but never lower it below 1000.
package idrange

// HardFloor is the absolute minimum uid/gid usersync will ever touch. The
// configured system floor is clamped up to at least this value; it can never be
// set lower, even via config.
const HardFloor uint32 = 1000

// Range is an inclusive [Min, Max] window of uid/gid values.
type Range struct {
	Min uint32
	Max uint32
}

// Contains reports whether id is within the inclusive range.
func (r Range) Contains(id uint32) bool { return id >= r.Min && id <= r.Max }

// Valid reports whether the range is well-formed (Max >= Min).
func (r Range) Valid() bool { return r.Max >= r.Min }

// Set is the manage window plus protect ranges for a single id kind (uid or gid).
type Set struct {
	Manage  Range
	Protect []Range
}

// Class is the classification of an id.
type Class int

const (
	// Protected ids are never created, modified, disabled, or deleted. A roster
	// entry declaring one is always a hard error.
	Protected Class = iota
	// Managed ids are the only ones usersync creates and reconciles.
	Managed
	// OutOfScope ids are neither managed nor protected: a roster entry declaring
	// one is refused by default, or skipped with a warning under on_out_of_scope.
	OutOfScope
)

func (c Class) String() string {
	switch c {
	case Protected:
		return "protected"
	case Managed:
		return "managed"
	case OutOfScope:
		return "out-of-scope"
	default:
		return "unknown"
	}
}

// Config configures a Classifier. SystemFloor is clamped up to HardFloor.
type Config struct {
	SystemFloor uint32
	UID         Set
	GID         Set
}

// Classifier classifies uids and gids per the manage/protect configuration.
type Classifier struct {
	systemFloor uint32
	uid         Set
	gid         Set
}

// New builds a Classifier, clamping SystemFloor up to HardFloor so the absolute
// safety floor can never be weakened by configuration.
func New(cfg Config) *Classifier {
	floor := cfg.SystemFloor
	if floor < HardFloor {
		floor = HardFloor
	}
	return &Classifier{systemFloor: floor, uid: cfg.UID, gid: cfg.GID}
}

// SystemFloor returns the effective (clamped) system floor.
func (c *Classifier) SystemFloor() uint32 { return c.systemFloor }

func (c *Classifier) classify(id uint32, s Set) Class {
	// Floor and protect ranges win over the manage window.
	if id < c.systemFloor {
		return Protected
	}
	for _, r := range s.Protect {
		if r.Contains(id) {
			return Protected
		}
	}
	if s.Manage.Contains(id) {
		return Managed
	}
	return OutOfScope
}

// UID classifies a user id.
func (c *Classifier) UID(id uint32) Class { return c.classify(id, c.uid) }

// GID classifies a group id.
func (c *Classifier) GID(id uint32) Class { return c.classify(id, c.gid) }
