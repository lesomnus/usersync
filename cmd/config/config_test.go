package config

import "testing"

func goodConfig() *Config {
	c := &Config{}
	// mirror 파일 서버 defaults
	_ = c.Evaluate()
	return c
}

func TestValidateAcceptsDefaults(t *testing.T) {
	if err := goodConfig().Validate(); err != nil {
		t.Fatalf("default config must be valid: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*Config){
		"inverted manage.uid":    func(c *Config) { c.Manage.UID.Min, c.Manage.UID.Max = 6999, 3000 },
		"inverted protect range": func(c *Config) { c.Protect.UID = []Range{{Min: 5100, Max: 5000}} },
		"floor above window":     func(c *Config) { c.Protect.SystemFloor = 100000 },
		"overlapping uid/gid":    func(c *Config) { c.Manage.GID.Min, c.Manage.GID.Max = 3000, 6999 },
		"relative home path":     func(c *Config) { c.Paths.Home = "research/home" },
		"bad on_out_of_scope":    func(c *Config) { c.OnOutOfScope = "nope" },
		"unknown provider":       func(c *Config) { c.Provider = "adduser" },
	}
	for name, mutate := range cases {
		c := goodConfig()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected Validate to reject, got nil", name)
		}
	}
}
