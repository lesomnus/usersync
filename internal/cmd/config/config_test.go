package config

import (
	"os"
	"path/filepath"
	"testing"
)

func goodConfig() *Config {
	c := &Config{}
	// mirror the shipped usersync.yaml defaults
	_ = c.Evaluate()
	return c
}

func TestValidateAcceptsDefaults(t *testing.T) {
	if err := goodConfig().Validate(); err != nil {
		t.Fatalf("default config must be valid: %v", err)
	}
}

// The default windows are the id band reserved for usersync with IT. They must
// not drift silently: a future on-prem AD carries these exact numbers as RFC2307
// attributes, and renumbering is impractical once snapshots own files by number.
func TestDefaultIDBand(t *testing.T) {
	c := goodConfig()
	for _, tt := range []struct {
		what     string
		min, max uint32
		wantMin  uint32
		wantMax  uint32
	}{
		{"manage.uid", c.Manage.UID.Min, c.Manage.UID.Max, 3000, 9999},
		{"manage.gid", c.Manage.GID.Min, c.Manage.GID.Max, 10000, 19999},
	} {
		if tt.min != tt.wantMin || tt.max != tt.wantMax {
			t.Errorf("%s = %d-%d, want %d-%d", tt.what, tt.min, tt.max, tt.wantMin, tt.wantMax)
		}
	}
	// UPG gid == uid, so a user's uid also burns that number in gid space. The
	// two windows must stay disjoint or Validate rejects the config outright.
	if c.Manage.UID.Max >= c.Manage.GID.Min {
		t.Errorf("uid window (max %d) must end below the gid window (min %d)", c.Manage.UID.Max, c.Manage.GID.Min)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*Config){
		"inverted manage.uid":    func(c *Config) { c.Manage.UID.Min, c.Manage.UID.Max = 6999, 3000 },
		"inverted protect range": func(c *Config) { c.Protect.UID = []Range{{Min: 5100, Max: 5000}} },
		"floor above window":     func(c *Config) { c.Protect.SystemFloor = 100000 },
		"overlapping uid/gid":    func(c *Config) { c.Manage.GID.Min, c.Manage.GID.Max = 3000, 6999 },
		"relative home path":     func(c *Config) { c.Paths.Home = "research/home" },
		"newline in groups path": func(c *Config) { c.Paths.Groups = "/srv/g\n[evil]" },
		"sub-1000 window voided by clamp": func(c *Config) {
			c.Manage.UID.Min, c.Manage.UID.Max = 400, 900
			c.Manage.GID.Min, c.Manage.GID.Max = 950, 999
			c.Protect.SystemFloor = 300
		},
		"bad on_out_of_scope": func(c *Config) { c.OnOutOfScope = "nope" },
		"unknown provider":    func(c *Config) { c.Provider = "adduser" },
		"unknown mode":        func(c *Config) { c.Mode = "readonly" },
	}
	for name, mutate := range cases {
		c := goodConfig()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected Validate to reject, got nil", name)
		}
	}
}

// A typo'd KEY must fail the load, not fall back to the default.
//
// Every safety control here is default-on-absence, so a silently-dropped key is
// indistinguishable from "the operator wanted the safe default" — except that
// the operator asked for the opposite. `moode: audit` loading as `mode: manage`
// lets apply run during a handover, which is the one thing audit mode exists to
// prevent.
func TestReadFromFileRejectsUnknownKey(t *testing.T) {
	for _, tt := range []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"typo'd top-level key", "moode: audit\n", true},
		{"typo'd nested block", "protcet:\n  system_floor: 5000\n", true},
		{"typo'd key inside a good block", "protect:\n  system_flor: 5000\n", true},
		{"the correct spelling still loads", "mode: audit\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "usersync.yaml")
			if err := os.WriteFile(p, []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := ReadFromFile(p)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ReadFromFile(%q) = nil error; a dropped key silently voids the safety default", tt.yaml)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadFromFile(%q): %v", tt.yaml, err)
			}
			if c.Mode != "audit" {
				t.Errorf("Mode = %q, want audit", c.Mode)
			}
		})
	}
}
