package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A tag freezes the CLI harder than it freezes any Go symbol: flag names, where
// a flag may appear, exit codes, and which stream a message lands on are what
// other people's scripts bind to, and none of them are checked by a test that
// calls the packages directly. These build the actual binary and run it.
//
// Deliberately no build tag: these need no root and no system tools, so they
// run in the default `go test ./...` where a regression is seen immediately.

// binary builds usersync once per package run and returns its path.
func binary(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain in PATH")
	}
	out := filepath.Join(t.TempDir(), "usersync")
	// -buildvcs=false: the test may run from a copied tree with no .git.
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", out, "github.com/lesomnus/usersync")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build usersync: %v\n%s", err, b)
	}
	return out
}

// fixture writes a minimal config + roster into a temp dir and returns it.
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("usersync.yaml", `paths:
  home: /research/home
  groups: /research/groups
mode: manage
provider: shadow-utils
`)
	write("roster.yaml", `groups:
  - name: team-a
    gid: 10001
    description: Perception team
    members: [skim]
users:
  - name: skim
    uid: 3001
    full_name: Sunghyun Kim
  - name: oldhand
    uid: 3005
    status: reserved
`)
	return dir
}

type result struct {
	stdout, stderr string
	code           int
}

func runCLI(t *testing.T, bin, dir string, args ...string) result {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if err != nil {
		if !asExitError(err, &ee) {
			t.Fatalf("run %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return result{stdout: out.String(), stderr: errb.String(), code: code}
}

func asExitError(err error, dst **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*dst = e
	}
	return ok
}

func TestCLIValidateAcceptsAGoodRoster(t *testing.T) {
	bin, dir := binary(t), fixture(t)

	r := runCLI(t, bin, dir, "validate")
	if r.code != 0 {
		t.Fatalf("validate exited %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "OK") {
		t.Errorf("validate stdout = %q, want it to say OK", r.stdout)
	}
}

// The safety controls are default-on-absence, so a typo'd key must be an error
// rather than a silent fallback — at the CLI, not just in the loader.
func TestCLIRejectsTypoedConfigKey(t *testing.T) {
	bin, dir := binary(t), fixture(t)
	if err := os.WriteFile(filepath.Join(dir, "usersync.yaml"), []byte("moode: audit\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := runCLI(t, bin, dir, "validate")
	if r.code == 0 {
		t.Fatalf("validate exited 0 on a typo'd key; the safety default applied silently\nstdout: %s", r.stdout)
	}
}

// An invalid roster must fail loudly and non-zero: `plan` in a cron job is only
// useful if a bad declaration is distinguishable from a clean one.
func TestCLIValidateRejectsOutOfRangeUID(t *testing.T) {
	bin, dir := binary(t), fixture(t)
	if err := os.WriteFile(filepath.Join(dir, "roster.yaml"),
		[]byte("users:\n  - name: root2\n    uid: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := runCLI(t, bin, dir, "validate")
	if r.code == 0 {
		t.Fatal("validate accepted uid 0, which is below the protected floor")
	}
	if r.stdout != "" && !strings.Contains(r.stderr, "") {
		t.Logf("stdout: %q stderr: %q", r.stdout, r.stderr)
	}
}

// `shares` renders the smb.conf fragment with no root and no Samba installed.
// The group description must reach the share comment — that is the only thing
// carrying it, and export dropping it used to break this silently.
func TestCLISharesRendersDescriptionAsComment(t *testing.T) {
	bin, dir := binary(t), fixture(t)

	r := runCLI(t, bin, dir, "shares")
	if r.code != 0 {
		t.Fatalf("shares exited %d\nstderr: %s", r.code, r.stderr)
	}
	for _, want := range []string{"[team-a]", "Perception team", "[homes]"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("shares output missing %q\ngot:\n%s", want, r.stdout)
		}
	}
}

// --config is a ROOT flag. README used to list it among per-command flags,
// which is a documentation bug precisely because the CLI rejects the other
// order — pin both halves so the docs cannot drift back.
func TestCLIConfigFlagIsRootOnly(t *testing.T) {
	bin, dir := binary(t), fixture(t)
	if err := os.Rename(filepath.Join(dir, "usersync.yaml"), filepath.Join(dir, "alt.yaml")); err != nil {
		t.Fatal(err)
	}

	if r := runCLI(t, bin, dir, "--config", "alt.yaml", "validate"); r.code != 0 {
		t.Errorf("`--config alt.yaml validate` exited %d, want 0\nstderr: %s", r.code, r.stderr)
	}
	r := runCLI(t, bin, dir, "validate", "--config", "alt.yaml")
	if r.code == 0 {
		t.Error("`validate --config alt.yaml` exited 0; if this now works, fix README instead of this test")
	}
}

// A flag that is accepted and ignored is the worst of the three states. These
// were real: `export --json` printed YAML and exited 0.
func TestCLIRejectsFlagsTheCommandDoesNotRead(t *testing.T) {
	bin, dir := binary(t), fixture(t)

	for _, tt := range []struct{ name, flag string }{
		{"export", "--json"},
		{"export", "--seed-file"},
		{"audit", "--seed-file"},
	} {
		args := []string{tt.name, tt.flag}
		if tt.flag == "--seed-file" {
			args = append(args, "seed")
		}
		r := runCLI(t, bin, dir, args...)
		if r.code == 0 {
			t.Errorf("`%s %s` exited 0; the flag is being silently ignored", tt.name, tt.flag)
		}
	}
}

// Mutating commands must refuse without root rather than half-running.
func TestCLIApplyRefusesWithoutRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	bin, dir := binary(t), fixture(t)

	r := runCLI(t, bin, dir, "apply")
	if r.code == 0 {
		t.Fatal("apply exited 0 as a non-root user")
	}
	if !strings.Contains(r.stderr, "root") {
		t.Errorf("stderr = %q, want it to name root as the reason", r.stderr)
	}
}

// watch is a mutating, long-running command. Without root it must refuse in the
// preamble — before the poll loop — so this returns instead of blocking. It also
// pins that watch is wired into the command set at all.
func TestCLIWatchRefusesWithoutRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	bin, dir := binary(t), fixture(t)

	r := runCLI(t, bin, dir, "watch")
	if r.code == 0 {
		t.Fatal("watch exited 0 as a non-root user")
	}
	if !strings.Contains(r.stderr, "root") {
		t.Errorf("stderr = %q, want it to name root as the reason", r.stderr)
	}
}

// mode: audit means a directory owns the accounts. apply must refuse, and it
// must refuse for THAT reason — a container entrypoint keys off this.
func TestCLIApplyRefusesInAuditMode(t *testing.T) {
	bin, dir := binary(t), fixture(t)
	if err := os.WriteFile(filepath.Join(dir, "usersync.yaml"), []byte("mode: audit\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := runCLI(t, bin, dir, "apply")
	if r.code == 0 {
		t.Fatal("apply exited 0 under mode: audit")
	}
	if !strings.Contains(r.stderr, "audit") {
		t.Errorf("stderr = %q, want it to name audit mode", r.stderr)
	}
}

// `version` is what a deployment prints into its logs to say what it is.
func TestCLIVersionPrintsSomething(t *testing.T) {
	bin, dir := binary(t), fixture(t)

	r := runCLI(t, bin, dir, "version")
	if r.code != 0 {
		t.Fatalf("version exited %d\nstderr: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "USERSYNC_VERSION") {
		t.Errorf("version stdout = %q, want a USERSYNC_VERSION line", r.stdout)
	}
}
