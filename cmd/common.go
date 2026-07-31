package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/lesomnus/usersync/cmd/config"
	"github.com/lesomnus/usersync/internal/executor"
	"github.com/lesomnus/usersync/internal/fsops"
	"github.com/lesomnus/usersync/internal/idrange"
	"github.com/lesomnus/usersync/internal/provider"
	"github.com/lesomnus/usersync/internal/roster"
	"github.com/lesomnus/usersync/internal/run"
	"github.com/lesomnus/usersync/internal/samba"
	"github.com/lesomnus/usersync/internal/secret"
	"github.com/lesomnus/usersync/internal/state"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
)

// commonFlags are shared by plan/apply/export. Fresh instances per command so
// their parsed state is not aliased.
func commonFlags() flg.Flags {
	return flg.Flags{
		&flg.String{Name: "roster", Brief: "path to roster.yaml", Value: z.Ptr("roster.yaml")},
		&flg.Switch{Name: "json", Brief: "machine-readable JSON report"},
		&flg.Switch{Name: "skip-out-of-scope", Brief: "skip out-of-scope roster entries with a warning instead of failing"},
		&flg.String{Name: "seed-file", Brief: "seed file for initial passwords (or env USERSYNC_SEED)"},
		&flg.String{Name: "home-base", Brief: "home directory root (overrides config paths.home)"},
		&flg.String{Name: "groups-base", Brief: "group folder root (overrides config paths.groups)"},
	}
}

// applyCommonFlags overlays flag values onto the loaded config (flags win when set).
func applyCommonFlags(cmd *xli.Command, c *config.Config) {
	flg.VisitP(cmd, "home-base", &c.Paths.Home)
	flg.VisitP(cmd, "groups-base", &c.Paths.Groups)
	flg.VisitP(cmd, "seed-file", &c.SeedFile)
	flg.Visit[bool](cmd, "skip-out-of-scope", func(v bool) {
		if v {
			c.OnOutOfScope = "skip"
		}
	})
}

func jsonRequested(cmd *xli.Command) bool {
	v, _ := flg.Get[bool](cmd, "json")
	return v
}

// loadRoster opens, strictly decodes, and validates the roster against the
// classifier and out-of-scope policy.
func loadRoster(cmd *xli.Command, c *config.Config, cls *idrange.Classifier) (*roster.Roster, []roster.Skipped, error) {
	path := "roster.yaml"
	if p, ok := flg.Get[string](cmd, "roster"); ok && p != "" {
		path = p
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, z.Err(err, "open roster %q", path)
	}
	defer f.Close()

	ro, err := roster.Load(f)
	if err != nil {
		return nil, nil, z.Err(err, "load roster %q", path)
	}
	policy, err := c.Policy()
	if err != nil {
		return nil, nil, err
	}
	skipped, err := ro.Validate(cls, policy)
	if err != nil {
		return nil, nil, z.Err(err, "validate roster %q", path)
	}
	return ro, skipped, nil
}

// backends builds the provider and samba backends over the given runner.
func backends(c *config.Config, r run.Runner) (provider.Provider, samba.Samba, error) {
	p, err := provider.Detect(c.Provider, r)
	if err != nil {
		return nil, nil, err
	}
	return p, samba.New(r), nil
}

// deriver loads the seed and builds a password deriver.
func deriver(c *config.Config) (*secret.Deriver, error) {
	seed, err := secret.LoadSeed(c.SeedFile)
	if err != nil {
		return nil, err
	}
	return secret.New(seed), nil
}

// requireRoot returns an error unless running as root.
func requireRoot() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root (euid 0)")
	}
	return nil
}

// lockRun takes an exclusive, non-blocking advisory lock so two mutating runs
// (apply/purge/shares --write) cannot race. It returns a release func to defer.
func lockRun() (func(), error) {
	path := "/run/usersync.lock"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		path = filepath.Join(os.TempDir(), "usersync.lock")
		f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open lock file: %w", err)
		}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another usersync run is in progress (lock %s)", path)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// errW returns the command's stderr, defaulting to os.Stderr.
func errW(cmd *xli.Command) io.Writer {
	if cmd.ErrWriter != nil {
		return cmd.ErrWriter
	}
	return os.Stderr
}

// warnSkipped prints out-of-scope skip warnings to stderr.
func warnSkipped(cmd *xli.Command, skipped []roster.Skipped) {
	w := errW(cmd)
	for _, s := range skipped {
		fmt.Fprintf(w, "warning: skipping %s %q: %s\n", s.Kind, s.Name, s.Reason)
	}
}

// --- dry-run runner + filesystem: print backend commands instead of executing ---

type printRunner struct{ w io.Writer }

func (p printRunner) Run(_ context.Context, stdin, name string, args ...string) (string, error) {
	switch name {
	case "getent", "pdbedit": // read-only probes: stay silent, report absent
		return "", nil
	}
	line := name
	if len(args) > 0 {
		line += " " + strings.Join(args, " ")
	}
	if stdin != "" {
		line += "   # (password on stdin)"
	}
	fmt.Fprintln(p.w, "    "+line)
	return "", nil
}

type printFS struct{ w io.Writer }

func (p printFS) EnsureGroupDir(path string, gid uint32) error {
	fmt.Fprintf(p.w, "    mkdir -p %s && chgrp %d %s && chmod 2770 %s\n", path, gid, path, path)
	return nil
}

func (p printFS) EnsureHomeDir(path string, uid, gid uint32) error {
	fmt.Fprintf(p.w, "    mkdir -p %s && chown %d:%d %s && chmod 0700 %s\n", path, uid, gid, path, path)
	return nil
}

// Stat is unused during command preview (Collect uses the real FS); present
// only to satisfy fsops.FS.
func (printFS) Stat(string) (bool, uint32, uint32, uint32) { return false, 0, 0, 0 }

// dryDeps builds an executor whose backends print commands instead of executing
// them (used by `plan --commands`). The deriver seed is irrelevant because the
// print runner never reveals passwords.
func dryDeps(c *config.Config, w io.Writer) (executor.Deps, error) {
	r := printRunner{w: w}
	p, s, err := backends(c, r)
	if err != nil {
		return executor.Deps{}, err
	}
	return executor.Deps{
		Provider:   p,
		Samba:      s,
		Deriver:    secret.New([]byte("preview")),
		FS:         printFS{w: w},
		HomeBase:   c.Paths.Home,
		GroupsBase: c.Paths.Groups,
	}, nil
}

// collectActual gathers filtered actual state (with home/group-folder existence)
// using the given runner-backed backends. smbOptional degrades an SMB scan
// failure (e.g. pdbedit needs root) to a warning for the read-only commands.
func collectActual(ctx context.Context, c *config.Config, r run.Runner, cls *idrange.Classifier, smbOptional bool, warn io.Writer) (*state.State, error) {
	p, s, err := backends(c, r)
	if err != nil {
		return nil, err
	}
	return executor.Collect(ctx, p, s, cls, executor.CollectOpts{
		HomeBase:    c.Paths.Home,
		GroupsBase:  c.Paths.Groups,
		FS:          fsops.OS{},
		SmbOptional: smbOptional,
		Warn:        warn,
	})
}
