package secret

import (
	"io"
	"os"
	"strings"
	"testing"
)

// captureStderr runs fn with os.Stderr redirected to a pipe and returns what
// was written there.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

func TestInitPWDeterministic(t *testing.T) {
	d1 := New([]byte("seed-value"))
	d2 := New([]byte("seed-value"))
	if a, b := d1.InitPW("skim"), d2.InitPW("skim"); a != b {
		t.Fatalf("same seed+user must reproduce: %q != %q", a, b)
	}
}

func TestInitPWFormat(t *testing.T) {
	d := New([]byte("seed-value"))
	pw := d.InitPW("skim")
	if !strings.HasPrefix(pw, "Hd-") {
		t.Errorf("missing prefix: %q", pw)
	}
	if len(pw) != len("Hd-")+derivedLen {
		t.Errorf("len(%q) = %d, want %d", pw, len(pw), len("Hd-")+derivedLen)
	}
	// base32 (no padding) body must be A-Z2-7 only.
	body := strings.TrimPrefix(pw, "Hd-")
	for _, r := range body {
		if !((r >= 'A' && r <= 'Z') || (r >= '2' && r <= '7')) {
			t.Errorf("body %q has non-base32 char %q", body, r)
		}
	}
}

func TestInitPWDistinctPerUser(t *testing.T) {
	d := New([]byte("seed-value"))
	if d.InitPW("alice") == d.InitPW("bob") {
		t.Error("different users must derive different passwords")
	}
}

func TestInitPWSeedSensitive(t *testing.T) {
	if New([]byte("seed-a")).InitPW("skim") == New([]byte("seed-b")).InitPW("skim") {
		t.Error("different seeds must derive different passwords")
	}
}

// Golden vector: freezes the derivation so an admin (or a rewrite) reproduces
// the exact same password. If this breaks, the algorithm changed — that is a
// compatibility break for already-provisioned users.
func TestInitPWGolden(t *testing.T) {
	got := New([]byte("golden-seed")).InitPW("skim")
	const want = "Hd-KUMNZIRNEZHZ"
	if got != want {
		t.Errorf("InitPW golden = %q, want %q", got, want)
	}
}

func TestLoadSeedFromEnv(t *testing.T) {
	t.Setenv(EnvSeed, "  env-seed  ")
	b, err := LoadSeed("/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "env-seed" {
		t.Errorf("got %q, want trimmed %q", b, "env-seed")
	}
}

func TestLoadSeedFromFile(t *testing.T) {
	os.Unsetenv(EnvSeed)
	f := t.TempDir() + "/seed.secret"
	if err := os.WriteFile(f, []byte("file-seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := LoadSeed(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "file-seed" {
		t.Errorf("got %q, want %q", b, "file-seed")
	}
}

func TestLoadSeedFilePermsWarning(t *testing.T) {
	os.Unsetenv(EnvSeed)

	loadWith := func(t *testing.T, perm os.FileMode) (string, []byte) {
		t.Helper()
		f := t.TempDir() + "/seed.secret"
		if err := os.WriteFile(f, []byte("file-seed\n"), perm); err != nil {
			t.Fatal(err)
		}
		// WriteFile is subject to umask; force the exact bits.
		if err := os.Chmod(f, perm); err != nil {
			t.Fatal(err)
		}
		var b []byte
		out := captureStderr(t, func() {
			var err error
			b, err = LoadSeed(f)
			if err != nil {
				t.Fatalf("LoadSeed(perm %04o) = %v", perm, err)
			}
		})
		if string(b) != "file-seed" {
			t.Errorf("got %q, want %q", b, "file-seed")
		}
		return out, b
	}

	t.Run("0644 warns", func(t *testing.T) {
		out, _ := loadWith(t, 0o644)
		if !strings.Contains(out, "warning:") {
			t.Errorf("group/other-accessible seed file must warn, got %q", out)
		}
	})

	t.Run("0600 silent", func(t *testing.T) {
		out, _ := loadWith(t, 0o600)
		if out != "" {
			t.Errorf("0600 seed file must not warn, got %q", out)
		}
	})
}

func TestLoadSeedEmptyRejected(t *testing.T) {
	os.Unsetenv(EnvSeed)
	f := t.TempDir() + "/empty"
	if err := os.WriteFile(f, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSeed(f); err == nil {
		t.Error("empty seed file must be rejected")
	}
}
