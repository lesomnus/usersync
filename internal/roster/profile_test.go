package roster

import (
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{
		"0": 0, "100": 100, "100B": 100,
		"1K": 1e3, "5M": 5e6, "100G": 100e9, "2T": 2e12,
		"1Ki": 1 << 10, "1Gi": 1 << 30,
	} {
		got, err := ParseSize(in)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", in, err)
		} else if got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "10X", "1Gigabyte"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) should fail", bad)
		}
	}
}

// A profile fills a user's unset policy; the user's own value wins; the named
// "default" applies to a user with no explicit profile.
func TestResolveProfiles(t *testing.T) {
	y := `profiles:
  default: { quota: 100G }
  intern:  { home: false, quota: 0 }
users:
  - { name: alice, uid: 3001 }                 # -> default: quota 100G, home default
  - { name: taeyun.an, uid: 3067, profile: intern }
  - { name: bob, uid: 3002, quota: 500G }      # own quota wins over default
`
	ro, err := Load(strings.NewReader(y))
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]User{}
	for _, u := range ro.Users {
		by[u.Name] = u
	}
	if by["alice"].Quota == nil || *by["alice"].Quota != 100e9 {
		t.Errorf("alice quota = %v, want default 100G", by["alice"].Quota)
	}
	if by["alice"].WantsHome() != true {
		t.Error("alice should keep a home (default profile does not set home)")
	}
	if by["taeyun.an"].WantsHome() != false {
		t.Error("intern profile must set home: false")
	}
	if by["taeyun.an"].Quota == nil || *by["taeyun.an"].Quota != 0 {
		t.Errorf("intern quota = %v, want 0", by["taeyun.an"].Quota)
	}
	if by["bob"].Quota == nil || *by["bob"].Quota != 500e9 {
		t.Errorf("bob quota = %v, want own 500G (overrides default)", by["bob"].Quota)
	}
}

func TestUndeclaredProfileRejected(t *testing.T) {
	y := "users:\n  - { name: a, uid: 3001, profile: ghost }\n"
	if _, err := Load(strings.NewReader(y)); err == nil {
		t.Fatal("a user referencing an undeclared profile must fail to load")
	}
}
