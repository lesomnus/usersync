package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lesomnus/usersync/internal/cmd/config"
	"github.com/lesomnus/xli"
)

// watch reloads usersync.yaml in place. Before, it read it only at start, and a
// change (legacy_views: false) waited for a restart while looking applied.
func TestReloadConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "usersync.yaml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := "paths: { home: /srv/h, groups: /srv/g, views: /srv/v }\n"
	write(base)
	c, err := config.ReadFromFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Evaluate(); err != nil {
		t.Fatal(err)
	}
	cmd := &xli.Command{}

	// A change is swapped in.
	write(base + "legacy_views: false\n")
	if err := reloadConfig(cmd, c); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if l := c.ViewLayout(); l.Legacy {
		t.Fatalf("legacy_views: false did not take: %+v", l)
	}
	if c.Path() != p {
		t.Errorf("path = %q; want %q", c.Path(), p)
	}

	// One that does not load leaves the last good config.
	write(base + "legacy_views: false\nmoode: audit\n")
	if err := reloadConfig(cmd, c); err == nil {
		t.Fatal("a config with an unknown key was reloaded")
	}
	write("paths: { home: /srv/h, groups: /srv/g, views: /srv/g/v }\n")
	if err := reloadConfig(cmd, c); err == nil {
		t.Fatal("an invalid config was reloaded")
	}
	if c.Paths.Views != "/srv/v" || c.ViewLayout().Legacy {
		t.Fatalf("a rejected reload changed the config: %+v", c.Paths)
	}

	// Leaving manage mode stops watch.
	write(base + "mode: audit\n")
	if err := reloadConfig(cmd, c); !errors.Is(err, errModeChanged) {
		t.Fatalf("reload into audit = %v; want errModeChanged", err)
	}
	if c.Mode == "audit" {
		t.Fatal("the audit config was swapped in")
	}
}
