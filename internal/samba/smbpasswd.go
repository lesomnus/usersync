package samba

import (
	"bufio"
	"context"
	"strings"

	"github.com/lesomnus/usersync/internal/run"
)

// smbpasswd is the default Samba backend built on the smbpasswd and pdbedit
// command-line tools shipped with Samba. It holds no state beyond the Runner,
// so it is safe to share.
type smbpasswd struct {
	r run.Runner
}

// NewSmbpasswd returns a Samba backed by the smbpasswd(8) and pdbedit(8) tools.
func NewSmbpasswd(r run.Runner) Samba { return &smbpasswd{r: r} }

// Accounts lists the actual SMB accounts by parsing `pdbedit -L -v`. The verbose
// listing emits one stanza per user; the account name comes from the
// "Unix username:" field and the enabled state from the "Account Flags:" field,
// whose bracketed flags contain 'D' when the account is disabled and 'U' when it
// is a normal (enabled) user. Parsing is whitespace-tolerant and ignores any
// fields it does not recognize.
func (s *smbpasswd) Accounts(ctx context.Context) (map[string]Account, error) {
	out, err := s.r.Run(ctx, "", "pdbedit", "-L", "-v")
	if err != nil {
		return nil, err
	}

	accounts := map[string]Account{}
	var cur string // name of the stanza currently being parsed

	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		key, val, ok := splitField(sc.Text())
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "unix username":
			// A new stanza begins. Record the account eagerly so a user with an
			// unexpectedly missing flags line still surfaces (disabled by default).
			cur = val
			if cur != "" {
				accounts[cur] = Account{Name: cur}
			}
		case "account flags":
			if cur == "" {
				continue
			}
			flags := flagsField(val)
			a := accounts[cur]
			a.Enabled = strings.ContainsRune(flags, 'U') && !strings.ContainsRune(flags, 'D')
			accounts[cur] = a
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return accounts, nil
}

// Create registers an SMB account with the given initial password. The -s flag
// makes smbpasswd read the new password from stdin twice (set and confirm), so
// the password is fed as two newline-terminated lines.
func (s *smbpasswd) Create(ctx context.Context, user, initialPassword string) error {
	stdin := initialPassword + "\n" + initialPassword + "\n"
	_, err := s.r.Run(ctx, stdin, "smbpasswd", "-a", "-s", user)
	return err
}

// Enable activates an SMB account (smbpasswd -e).
func (s *smbpasswd) Enable(ctx context.Context, user string) error {
	_, err := s.r.Run(ctx, "", "smbpasswd", "-e", user)
	return err
}

// Disable deactivates an SMB account while keeping it (smbpasswd -d).
func (s *smbpasswd) Disable(ctx context.Context, user string) error {
	_, err := s.r.Run(ctx, "", "smbpasswd", "-d", user)
	return err
}

// Delete removes an SMB account entirely (smbpasswd -x).
func (s *smbpasswd) Delete(ctx context.Context, user string) error {
	_, err := s.r.Run(ctx, "", "smbpasswd", "-x", user)
	return err
}

// splitField splits a "Key: value" line on the first colon and trims surrounding
// whitespace from both parts. ok is false when the line has no colon.
func splitField(line string) (key, val string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
}

// flagsField extracts the flag characters of an "Account Flags:" value, e.g.
// "[DU         ]" -> "DU         ". If the brackets are absent the raw value is
// returned so the caller can still scan it for flag letters.
func flagsField(val string) string {
	i := strings.IndexByte(val, '[')
	j := strings.IndexByte(val, ']')
	if i >= 0 && j > i {
		return val[i+1 : j]
	}
	return val
}
