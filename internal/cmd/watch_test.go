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
	otelAsRead := c.Otel
	// What Build does at start: fill the running section with defaults. A reload
	// must not read that as a change to the file.
	if _, _, err := c.Otel.Build(t.Context()); err != nil {
		t.Fatal(err)
	}

	// A change is swapped in.
	moved := "paths: { home: /srv/h, groups: /srv/g, views: /srv/v2 }\n"
	write(moved)
	if otelChanged, err := reloadConfig(cmd, c, &otelAsRead); err != nil {
		t.Fatalf("reload: %v", err)
	} else if otelChanged {
		t.Error("an unchanged otel section was reported as changed")
	}
	if l := c.ViewLayout(); l.Views != "/srv/v2" {
		t.Fatalf("the new paths.views did not take: %+v", l)
	}
	if c.Path() != p {
		t.Errorf("path = %q; want %q", c.Path(), p)
	}

	// One that does not load leaves the last good config.
	write(moved + "moode: audit\n")
	if _, err := reloadConfig(cmd, c, &otelAsRead); err == nil {
		t.Fatal("a config with an unknown key was reloaded")
	}
	write("paths: { home: /srv/h, groups: /srv/g, views: /srv/g/v }\n")
	if _, err := reloadConfig(cmd, c, &otelAsRead); err == nil {
		t.Fatal("an invalid config was reloaded")
	}
	if c.Paths.Views != "/srv/v2" {
		t.Fatalf("a rejected reload changed the config: %+v", c.Paths)
	}

	// Leaving manage mode stops watch.
	write(base + "mode: audit\n")
	if _, err := reloadConfig(cmd, c, &otelAsRead); !errors.Is(err, errModeChanged) {
		t.Fatalf("reload into audit = %v; want errModeChanged", err)
	}
	if c.Mode == "audit" {
		t.Fatal("the audit config was swapped in")
	}

	// A real change to the otel section is reported once.
	write(base + "otel: { enabled: false }\n")
	if otelChanged, err := reloadConfig(cmd, c, &otelAsRead); err != nil || !otelChanged {
		t.Fatalf("otel change: changed=%v err=%v", otelChanged, err)
	}
	write(base + "otel: { enabled: false }\n# touched\n")
	if otelChanged, err := reloadConfig(cmd, c, &otelAsRead); err != nil || otelChanged {
		t.Fatalf("same otel again: changed=%v err=%v", otelChanged, err)
	}
}
