package roster

import (
	"fmt"
	"strconv"
	"strings"
)

// Size is a byte count written in the roster as a human string ("100G", "50Gi",
// "0"). Decimal suffixes K/M/G/T are powers of 1000; binary suffixes Ki/Mi/Gi/Ti
// are powers of 1024; a bare number, or a B suffix, is bytes. Zero is a real
// zero-byte limit, not "unlimited" — the absence of the field is unlimited.
type Size uint64

// UnmarshalYAML accepts a plain integer (`quota: 0`) or a size string
// (`quota: 100G`).
func (s *Size) UnmarshalYAML(unmarshal func(any) error) error {
	var raw any
	if err := unmarshal(&raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case int:
		if v < 0 {
			return fmt.Errorf("quota cannot be negative: %d", v)
		}
		*s = Size(v)
	case int64:
		if v < 0 {
			return fmt.Errorf("quota cannot be negative: %d", v)
		}
		*s = Size(v)
	case uint64:
		*s = Size(v)
	case string:
		n, err := ParseSize(v)
		if err != nil {
			return err
		}
		*s = Size(n)
	default:
		return fmt.Errorf("quota must be a number or a size string, got %T", raw)
	}
	return nil
}

// ParseSize turns "100G" / "50Gi" / "0" into a byte count.
func ParseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	n, err := strconv.ParseUint(s[:i], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q: not a number", s)
	}
	var mult uint64
	switch strings.ToUpper(strings.TrimSpace(s[i:])) {
	case "", "B":
		mult = 1
	case "K":
		mult = 1e3
	case "M":
		mult = 1e6
	case "G":
		mult = 1e9
	case "T":
		mult = 1e12
	case "KI":
		mult = 1 << 10
	case "MI":
		mult = 1 << 20
	case "GI":
		mult = 1 << 30
	case "TI":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("bad size unit in %q (use K/M/G/T or Ki/Mi/Gi/Ti)", s)
	}
	return n * mult, nil
}

// Profile is a reusable bundle of per-user policy — the fields a user can inherit
// instead of repeating. A user's own value always wins over the profile's; a
// field the profile leaves unset is left to the user (or the built-in default).
type Profile struct {
	Home  *bool `yaml:"home,omitempty"`
	Quota *Size `yaml:"quota,omitempty"`
}

// ResolveProfiles fills each user's unset policy fields from its profile: the one
// named in `profile:`, or the profile named "default" for a user that names none.
// A user's own field always wins, so a profile is a default, not an override. An
// explicit `profile:` naming a profile that is not declared is an error — the
// same "a name that is not there is a refusal" rule the rest of the roster keeps.
func (ro *Roster) ResolveProfiles() error {
	for i := range ro.Users {
		u := &ro.Users[i]
		name := u.Profile
		if name == "" {
			if _, ok := ro.Profiles["default"]; !ok {
				continue // no explicit profile and no default: built-in defaults apply
			}
			name = "default"
		}
		p, ok := ro.Profiles[name]
		if !ok {
			return fmt.Errorf("user %q references profile %q that is not declared", u.Name, u.Profile)
		}
		if u.Home == nil {
			u.Home = p.Home
		}
		if u.Quota == nil {
			u.Quota = p.Quota
		}
	}
	return nil
}
