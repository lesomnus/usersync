// Package samba abstracts the SMB (tdbsam) credential store, orthogonal to the
// OS account backend. The default implementation wraps smbpasswd and pdbedit.
package samba

import (
	"context"

	"github.com/lesomnus/usersync/internal/run"
)

// Account is an actual SMB account and whether it is enabled.
type Account struct {
	Name    string
	Enabled bool
}

// Samba is the SMB credential lifecycle backend.
type Samba interface {
	// Accounts lists the actual SMB accounts and their enabled state (pdbedit).
	Accounts(ctx context.Context) (map[string]Account, error)

	// Create registers an SMB account with the given initial password. It is
	// only called for accounts that do not yet exist.
	Create(ctx context.Context, user, initialPassword string) error

	// Enable activates an SMB account (smbpasswd -e).
	Enable(ctx context.Context, user string) error

	// Disable deactivates an SMB account (smbpasswd -d); the account is kept.
	Disable(ctx context.Context, user string) error

	// Delete removes an SMB account entirely (smbpasswd -x); purge only.
	Delete(ctx context.Context, user string) error
}

// New returns the default smbpasswd/pdbedit-backed implementation.
func New(r run.Runner) Samba { return NewSmbpasswd(r) }
