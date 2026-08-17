package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/usersync/internal/idrange"
	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/z"
)

var DefaultConfigPaths = []string{
	"usersync.yaml",
	"usersync.yml",
}

// Range is an inclusive [min, max] window of uid/gid values.
type Range struct {
	Min uint32 `yaml:"min"`
	Max uint32 `yaml:"max"`
}

// PathsConfig locates the home and group folder roots.
type PathsConfig struct {
	Home   string `yaml:"home"`
	Groups string `yaml:"groups"`
}

// ManageConfig is the id window usersync manages.
type ManageConfig struct {
	UID Range `yaml:"uid"`
	GID Range `yaml:"gid"`
}

// ProtectConfig is the reserved id ranges usersync never touches. SystemFloor
// is clamped up to idrange.HardFloor (1000) by the classifier.
type ProtectConfig struct {
	SystemFloor uint32  `yaml:"system_floor"`
	UID         []Range `yaml:"uid"`
	GID         []Range `yaml:"gid"`
}

// QuotaConfig selects the per-uid disk-quota backend. Absent (or "none") means
// declared `quota:` fields in the roster are recorded but not enforced. "zfs"
// enforces them via the native `userquota@<uid>` property on Dataset — the
// dataset the managed store lives on (e.g. "tank/nas"). It is opt-in because it
// needs the zfs tool and /dev/zfs in usersync's runtime, which not every host
// has; where it is off, a declared quota is a documented intent, not a control.
type QuotaConfig struct {
	Backend string `yaml:"backend"`
	Dataset string `yaml:"dataset"`
}

// Config is the operational configuration (usersync.yaml). The roster (desired
// state) lives separately in roster.yaml.
type Config struct {
	path string

	Paths   PathsConfig   `yaml:"paths"`
	Manage  ManageConfig  `yaml:"manage"`
	Protect ProtectConfig `yaml:"protect"`

	// Mode is who owns the accounts.
	//
	//   manage  usersync creates and reconciles them (the default)
	//   audit   something else does — a directory service — and usersync only
	//           verifies that what the system resolves matches the roster
	//
	// In audit mode every mutating command refuses. The roster stays the ledger
	// of which number belongs to whom after the handover, and `usersync audit` is
	// what checks the directory still agrees with it; an `apply` at that point
	// would try to recreate accounts the directory owns.
	Mode string `yaml:"mode"`

	OnOutOfScope string `yaml:"on_out_of_scope"`
	SeedFile     string `yaml:"seed_file"`
	Provider     string `yaml:"provider"`

	Quota QuotaConfig `yaml:"quota"`

	Otel OtelConfig `yaml:"otel"`
}

func ReadFromFile(p string) (*Config, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, z.Err(err, "open")
	}
	defer f.Close()

	var c Config
	// Strict, matching roster.Load. Every safety control in this file is a
	// default-on-absence: `mode` falls back to manage, `system_floor` to 1000. A
	// non-strict decode turns a typo'd KEY into that safe-looking default instead
	// of an error — `moode: audit` loads as `mode: manage`, and `apply` sails
	// straight past the gate whose whole job is to stop it from recreating
	// accounts a directory already owns. Validate() catches a bad enum VALUE;
	// only strict decoding catches the key.
	if err := yaml.NewDecoder(f, yaml.Strict()).Decode(&c); err != nil {
		return nil, z.Err(err, "decode")
	}

	c.path = p
	return &c, nil
}

func (c *Config) Path() string { return c.path }

// Evaluate fills defaults for any unset field.
func (c *Config) Evaluate() error {
	z.FallbackP(&c.Paths.Home, "/research/home")
	z.FallbackP(&c.Paths.Groups, "/research/groups")
	// The 3000-19999 band is reserved for usersync as a whole (see
	// identity-roadmap.md): a future on-prem AD must be able to carry these exact
	// uid/gid numbers in its RFC2307 attributes, and renumbering later is
	// effectively impossible once ZFS snapshots hold files by numeric owner. The
	// window is deliberately wider than today's headcount because widening it
	// after the band has been negotiated with IT is the expensive direction.
	z.FallbackP(&c.Manage.UID.Min, uint32(3000))
	z.FallbackP(&c.Manage.UID.Max, uint32(9999))
	z.FallbackP(&c.Manage.GID.Min, uint32(10000))
	z.FallbackP(&c.Manage.GID.Max, uint32(19999))
	z.FallbackP(&c.Protect.SystemFloor, uint32(1000))
	z.FallbackP(&c.Mode, "manage")
	z.FallbackP(&c.OnOutOfScope, "error")
	z.FallbackP(&c.SeedFile, "./seed.secret")
	z.FallbackP(&c.Provider, "auto")
	z.FallbackP(&c.Quota.Backend, "none")
	return nil
}

// Classifier builds the id classifier from the manage/protect configuration.
func (c *Config) Classifier() *idrange.Classifier {
	conv := func(rs []Range) []idrange.Range {
		out := make([]idrange.Range, len(rs))
		for i, r := range rs {
			out[i] = idrange.Range{Min: r.Min, Max: r.Max}
		}
		return out
	}
	return idrange.New(idrange.Config{
		SystemFloor: c.Protect.SystemFloor,
		UID: idrange.Set{
			Manage:  idrange.Range{Min: c.Manage.UID.Min, Max: c.Manage.UID.Max},
			Protect: conv(c.Protect.UID),
		},
		GID: idrange.Set{
			Manage:  idrange.Range{Min: c.Manage.GID.Min, Max: c.Manage.GID.Max},
			Protect: conv(c.Protect.GID),
		},
	})
}

// Validate rejects a config that would silently mis-scope or mis-provision, so a
// typo (inverted range, a floor above the window, a relative path, an unknown
// enum) fails loudly at load instead of quietly voiding safety. Call after Evaluate.
func (c *Config) Validate() error {
	okRange := func(what string, min, max uint32) error {
		if max < min {
			return fmt.Errorf("%s range is inverted (min %d > max %d)", what, min, max)
		}
		return nil
	}
	for _, e := range []error{
		okRange("manage.uid", c.Manage.UID.Min, c.Manage.UID.Max),
		okRange("manage.gid", c.Manage.GID.Min, c.Manage.GID.Max),
	} {
		if e != nil {
			return e
		}
	}
	for i, r := range c.Protect.UID {
		if r.Max < r.Min {
			return fmt.Errorf("protect.uid[%d] range is inverted (min %d > max %d) — it would protect nothing", i, r.Min, r.Max)
		}
	}
	for i, r := range c.Protect.GID {
		if r.Max < r.Min {
			return fmt.Errorf("protect.gid[%d] range is inverted (min %d > max %d) — it would protect nothing", i, r.Min, r.Max)
		}
	}
	// A floor above the whole window means no id can ever be managed. Compare the
	// EFFECTIVE floor (the classifier clamps the configured value up to HardFloor),
	// so a sub-1000 window with a low system_floor doesn't silently manage nothing.
	floor := c.Protect.SystemFloor
	if floor < idrange.HardFloor {
		floor = idrange.HardFloor
	}
	if floor > c.Manage.UID.Max {
		return fmt.Errorf("effective floor %d is above the manage.uid window (max %d); no user could be managed", floor, c.Manage.UID.Max)
	}
	if floor > c.Manage.GID.Max {
		return fmt.Errorf("effective floor %d is above the manage.gid window (max %d); no group could be managed", floor, c.Manage.GID.Max)
	}
	// UPG gid == uid, so the user window and the team-group window must be disjoint.
	if c.Manage.UID.Min <= c.Manage.GID.Max && c.Manage.GID.Min <= c.Manage.UID.Max {
		return fmt.Errorf("manage.uid (%d-%d) and manage.gid (%d-%d) overlap; they must be disjoint because each user's UPG gid equals its uid",
			c.Manage.UID.Min, c.Manage.UID.Max, c.Manage.GID.Min, c.Manage.GID.Max)
	}
	for _, p := range []struct{ name, val string }{{"paths.home", c.Paths.Home}, {"paths.groups", c.Paths.Groups}} {
		if !filepath.IsAbs(p.val) {
			return fmt.Errorf("%s must be an absolute path, got %q", p.name, p.val)
		}
		if i := strings.IndexFunc(p.val, func(r rune) bool { return r < 0x20 || r == 0x7f }); i >= 0 {
			return fmt.Errorf("%s must not contain control/newline characters", p.name)
		}
	}
	switch c.Mode {
	case "", "manage", "audit":
	default:
		return fmt.Errorf("invalid mode %q (want manage|audit)", c.Mode)
	}
	switch c.OnOutOfScope {
	case "", "error", "skip":
	default:
		return fmt.Errorf("invalid on_out_of_scope %q (want error|skip)", c.OnOutOfScope)
	}
	switch c.Provider {
	case "", "auto", "shadow-utils", "shadowutils", "busybox", "pw":
	default:
		return fmt.Errorf("invalid provider %q (want auto|shadow-utils|busybox|pw)", c.Provider)
	}
	switch c.Quota.Backend {
	case "", "none":
	case "zfs":
		if strings.TrimSpace(c.Quota.Dataset) == "" {
			return fmt.Errorf("quota.backend %q requires quota.dataset (the dataset the managed store lives on, e.g. tank/nas)", c.Quota.Backend)
		}
	default:
		return fmt.Errorf("invalid quota.backend %q (want none|zfs)", c.Quota.Backend)
	}
	return nil
}

// Policy maps on_out_of_scope to a roster.Policy.
func (c *Config) Policy() (roster.Policy, error) {
	switch c.OnOutOfScope {
	case "", "error":
		return roster.PolicyError, nil
	case "skip":
		return roster.PolicySkip, nil
	default:
		return roster.PolicyError, fmt.Errorf("invalid on_out_of_scope %q (want error|skip)", c.OnOutOfScope)
	}
}
