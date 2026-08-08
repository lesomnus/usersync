// Package integration holds usersync's end-to-end tests, which drive the real
// reconciler against real shadow-utils/busybox and a real Samba — actual
// useradd, actual smbpasswd, actual testparm — rather than fakes. They are
// guarded by the `integration` build tag and self-skip unless run as root with
// those tools present; see integration_test.go and scripts/verify-integration.sh.
//
// What they do NOT cover is the CLI itself: they call the packages directly, so
// flag names, exit codes, and the stdout/stderr split are not exercised here.
// That surface is covered by cli_test.go, which builds and runs the binary.
package integration
