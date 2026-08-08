package cmd

import (
	"strings"
	"testing"

	"github.com/lesomnus/usersync/internal/cmd/config"
)

// The mode gate decides whether usersync still owns the accounts. Getting it
// backwards would either let an apply fight a directory service for ownership,
// or lock the tool out of a system it is supposed to be managing.
func TestRequireManageMode(t *testing.T) {
	for name, tt := range map[string]struct {
		mode      string
		wantBlock bool
	}{
		"explicit manage": {mode: "manage", wantBlock: false},
		// Evaluate() fills the default, but a Config built in code may not have
		// been evaluated; an empty mode must not be read as audit.
		"unset defaults to manage": {mode: "", wantBlock: false},
		"audit":                    {mode: "audit", wantBlock: true},
	} {
		t.Run(name, func(t *testing.T) {
			err := requireManageMode(&config.Config{Mode: tt.mode}, "apply")
			if tt.wantBlock && err == nil {
				t.Fatal("expected the command to be refused")
			}
			if !tt.wantBlock && err != nil {
				t.Fatalf("expected the command to be allowed, got %v", err)
			}
			if tt.wantBlock {
				// The message has to say how to get out of the state it just
				// blocked, or the operator is stuck with a bare refusal.
				for _, want := range []string{"apply", "audit", "mode: manage"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal should mention %q, got: %v", want, err)
					}
				}
			}
		})
	}
}
