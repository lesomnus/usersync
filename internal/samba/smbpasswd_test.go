package samba

import (
	"context"
	"reflect"
	"testing"

	"github.com/lesomnus/usersync/internal/run"
)

// pdbeditVerbose is a realistic `pdbedit -L -v` listing with two stanzas: an
// enabled user (flags [U]) and a disabled one (flags [DU]).
const pdbeditVerbose = `Unix username:        alice
NT username:
Account Flags:        [U          ]
User SID:             S-1-5-21-1000000000-2000000000-3000000000-3002
Primary Group SID:    S-1-5-21-1000000000-2000000000-3000000000-513
Full Name:            Alice Example
Home Directory:       \\server\alice
HomeDir Drive:
Logon Script:
Profile Path:         \\server\alice\profile
Domain:               WORKGROUP
Account desc:
Workstations:
Munged dial:
Logon time:           0
Logoff time:          never
Kickoff time:         never
Password last set:    Wed, 01 Jan 2025 00:00:00 UTC
Password can change:  Wed, 01 Jan 2025 00:00:00 UTC
Password must change: never
Last bad password   : 0
Bad password count  : 0
Logon hours         : FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF

Unix username:        bob
NT username:
Account Flags:        [DU         ]
User SID:             S-1-5-21-1000000000-2000000000-3000000000-3004
Primary Group SID:    S-1-5-21-1000000000-2000000000-3000000000-513
Full Name:            Bob Example
Home Directory:       \\server\bob
HomeDir Drive:
Logon Script:
Profile Path:         \\server\bob\profile
Domain:               WORKGROUP
Account desc:
Workstations:
Munged dial:
Logon time:           0
Logoff time:          never
Kickoff time:         never
Password last set:    Wed, 01 Jan 2025 00:00:00 UTC
Password can change:  Wed, 01 Jan 2025 00:00:00 UTC
Password must change: never
Last bad password   : 0
Bad password count  : 0
Logon hours         : FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF
`

func TestSmbpasswdAccounts(t *testing.T) {
	var gotCall run.Call
	f := &run.Fake{Handler: func(stdin, name string, args ...string) (string, error) {
		gotCall = run.Call{Stdin: stdin, Name: name, Args: args}
		return pdbeditVerbose, nil
	}}
	s := NewSmbpasswd(f)

	got, err := s.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}

	// It must query pdbedit for the verbose listing.
	if gotCall.String() != "pdbedit -L -v" {
		t.Errorf("command = %q, want %q", gotCall.String(), "pdbedit -L -v")
	}

	want := map[string]Account{
		"alice": {Name: "alice", Enabled: true},
		"bob":   {Name: "bob", Enabled: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("accounts = %#v, want %#v", got, want)
	}
}

func TestSmbpasswdAccountsEmpty(t *testing.T) {
	f := &run.Fake{} // default handler returns "", nil
	s := NewSmbpasswd(f)

	got, err := s.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("accounts = %#v, want empty", got)
	}
}

func TestSmbpasswdCreate(t *testing.T) {
	f := &run.Fake{}
	s := NewSmbpasswd(f)

	if err := s.Create(context.Background(), "carol", "s3cr3t"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if len(f.Calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.Calls))
	}
	c := f.Calls[0]
	if c.Name != "smbpasswd" || !reflect.DeepEqual(c.Args, []string{"-a", "-s", "carol"}) {
		t.Errorf("command = %q, want %q", c.String(), "smbpasswd -a -s carol")
	}
	// -s reads the password twice from stdin: set then confirm.
	if want := "s3cr3t\ns3cr3t\n"; c.Stdin != want {
		t.Errorf("stdin = %q, want %q", c.Stdin, want)
	}
}

func TestSmbpasswdLifecycle(t *testing.T) {
	cases := []struct {
		name string
		call func(Samba) error
		want string
	}{
		{"enable", func(s Samba) error { return s.Enable(context.Background(), "dave") }, "smbpasswd -e dave"},
		{"disable", func(s Samba) error { return s.Disable(context.Background(), "dave") }, "smbpasswd -d dave"},
		{"delete", func(s Samba) error { return s.Delete(context.Background(), "dave") }, "smbpasswd -x dave"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &run.Fake{}
			s := NewSmbpasswd(f)
			if err := tc.call(s); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(f.Calls) != 1 {
				t.Fatalf("calls = %d, want 1", len(f.Calls))
			}
			c := f.Calls[0]
			if c.String() != tc.want {
				t.Errorf("command = %q, want %q", c.String(), tc.want)
			}
			if c.Stdin != "" {
				t.Errorf("stdin = %q, want empty", c.Stdin)
			}
		})
	}
}
