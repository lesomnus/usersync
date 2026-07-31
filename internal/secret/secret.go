// Package secret derives deterministic initial SMB passwords from a seed, so an
// administrator can recompute a user's initial password without storing it.
//
// The seed itself is never stored in the roster; it comes from a 0600 file or
// the USERSYNC_SEED environment variable (see LoadSeed). Initial passwords are
// only set at account creation and never reset, so a user's own later change is
// preserved (see plan.md §7).
package secret

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"os"
	"strings"
)

// EnvSeed is the environment variable that can supply the seed in place of a file.
const EnvSeed = "USERSYNC_SEED"

// prefix guarantees a minimum complexity class (upper + digit + symbol) on top
// of the derived body.
const prefix = "Hd-"

// derivedLen is how many base32 characters of the HMAC are used.
const derivedLen = 12

// base32 (RFC 4648) uses A-Z and 2-7 only: no lowercase and no 0/1/8/9, which
// already excludes the visually ambiguous characters. No padding.
var enc = base32.StdEncoding.WithPadding(base32.NoPadding)

// Deriver produces deterministic initial passwords for a fixed seed.
type Deriver struct {
	seed []byte
}

// New returns a Deriver over the given seed.
func New(seed []byte) *Deriver { return &Deriver{seed: seed} }

// InitPW returns the deterministic initial password for username:
//
//	"Hd-" + base32(HMAC_SHA256(seed, "usersync:v1:"+username))[:12]
func (d *Deriver) InitPW(username string) string {
	mac := hmac.New(sha256.New, d.seed)
	mac.Write([]byte("usersync:v1:" + username))
	body := enc.EncodeToString(mac.Sum(nil))
	return prefix + body[:derivedLen]
}

// LoadSeed resolves the seed: the USERSYNC_SEED environment variable takes
// precedence; otherwise the file at path is read. Surrounding whitespace is
// trimmed. An empty seed is rejected.
func LoadSeed(path string) ([]byte, error) {
	if v, ok := os.LookupEnv(EnvSeed); ok {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil, fmt.Errorf("%s is set but empty", EnvSeed)
		}
		return []byte(v), nil
	}
	if path == "" {
		return nil, fmt.Errorf("no seed: set %s or provide a seed file", EnvSeed)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read seed file %q: %w", path, err)
	}
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) == 0 {
		return nil, fmt.Errorf("seed file %q is empty", path)
	}
	return b, nil
}
