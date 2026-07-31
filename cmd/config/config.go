package config

import (
	"fmt"
	"os"

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

// Config is the operational configuration (usersync.yaml). The roster (desired
// state) lives separately in roster.yaml.
type Config struct {
	path string

	Paths        PathsConfig   `yaml:"paths"`
	Manage       ManageConfig  `yaml:"manage"`
	Protect      ProtectConfig `yaml:"protect"`
	OnOutOfScope string        `yaml:"on_out_of_scope"`
	SeedFile     string        `yaml:"seed_file"`
	Provider     string        `yaml:"provider"`

	Otel OtelConfig `yaml:"otel"`
}

func ReadFromFile(p string) (*Config, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, z.Err(err, "open")
	}
	defer f.Close()

	var c Config
	if err := yaml.NewDecoder(f).Decode(&c); err != nil {
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
	z.FallbackP(&c.Manage.UID.Min, uint32(3000))
	z.FallbackP(&c.Manage.UID.Max, uint32(6999))
	z.FallbackP(&c.Manage.GID.Min, uint32(7000))
	z.FallbackP(&c.Manage.GID.Max, uint32(7999))
	z.FallbackP(&c.Protect.SystemFloor, uint32(1000))
	z.FallbackP(&c.OnOutOfScope, "error")
	z.FallbackP(&c.SeedFile, "./seed.secret")
	z.FallbackP(&c.Provider, "auto")
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
