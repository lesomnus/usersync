// Package integration holds usersync's end-to-end tests, which run the real
// binary against real shadow-utils and Samba. They are guarded by the
// `integration` build tag and self-skip unless run as root with those tools
// present — see integration_test.go and scripts/verify-integration.sh.
package integration
