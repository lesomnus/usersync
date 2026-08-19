package cmd

import (
	"testing"

	"github.com/lesomnus/usersync/internal/reconcile"
)

// NSS-only is the web pod's contract: it converges /etc/passwd and group
// memberships and reports problems, but never runs an action that mutates state
// the SMB server owns. This pins exactly which kinds survive the filter — a new
// action kind is dropped by default, which is the safe side for "do not touch
// shared state", and adding one that SHOULD run here means updating this test.
func TestNSSOnlyActionsKeepsPosixDropsShared(t *testing.T) {
	kept := []reconcile.Kind{
		reconcile.CreateGroup, reconcile.SetGroupAdmins,
		reconcile.CreateUser, reconcile.CreateUserDisabled, reconcile.UpdateUserGroups,
		reconcile.RefuseGroup, reconcile.OrphanGroup,
		reconcile.RefuseUser, reconcile.OrphanUser, reconcile.ReservedPresent,
	}
	dropped := []reconcile.Kind{
		reconcile.SetGroupReaders, reconcile.AddSmb, reconcile.EnableUser,
		reconcile.DisableUser, reconcile.EnsureHome, reconcile.SetUserQuota,
		reconcile.ClearUserQuota,
	}

	var in []reconcile.Action
	for _, k := range append(append([]reconcile.Kind{}, kept...), dropped...) {
		in = append(in, reconcile.Action{Kind: k, Name: "x"})
	}

	got := map[reconcile.Kind]bool{}
	for _, a := range nssOnlyActions(in) {
		got[a.Kind] = true
	}
	for _, k := range kept {
		if !got[k] {
			t.Errorf("%s was dropped; NSS-only must keep POSIX/notice actions", k)
		}
	}
	for _, k := range dropped {
		if got[k] {
			t.Errorf("%s survived; NSS-only must leave SMB/folder/quota to the SMB server", k)
		}
	}
}
