// Package run is a thin, injectable wrapper around command execution so that
// provider/samba backends can be unit-tested with golden-command assertions and
// canned output, without executing anything real.
package run

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes an external command with optional stdin and returns its
// stdout. Implementations: Exec (real) and Fake (test).
type Runner interface {
	Run(ctx context.Context, stdin, name string, args ...string) (stdout string, err error)
}

// Exec is the real Runner backed by os/exec.
type Exec struct{}

// Run executes name with args, feeding stdin if non-empty, and returns stdout.
// On failure the error includes the command and its stderr.
func (Exec) Run(ctx context.Context, stdin, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %w: %s",
			name, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Call is a single recorded invocation.
type Call struct {
	Stdin string
	Name  string
	Args  []string
}

// String renders the call as a shell-ish command line (stdin noted, not shown).
func (c Call) String() string {
	s := c.Name
	if len(c.Args) > 0 {
		s += " " + strings.Join(c.Args, " ")
	}
	return s
}

// Fake is a Runner that records calls and returns canned output. Handler, if
// set, decides the output/error for a call; otherwise Run returns "", nil.
type Fake struct {
	Calls   []Call
	Handler func(stdin, name string, args ...string) (string, error)
}

func (f *Fake) Run(_ context.Context, stdin, name string, args ...string) (string, error) {
	f.Calls = append(f.Calls, Call{Stdin: stdin, Name: name, Args: append([]string(nil), args...)})
	if f.Handler != nil {
		return f.Handler(stdin, name, args...)
	}
	return "", nil
}

// Commands returns each recorded call rendered as a command line.
func (f *Fake) Commands() []string {
	out := make([]string, len(f.Calls))
	for i, c := range f.Calls {
		out[i] = c.String()
	}
	return out
}
