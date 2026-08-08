#!/usr/bin/env bash
#
# End-to-end integration verification for usersync.
#
# Runs the tagged Go integration test (internal/integration) as root against
# REAL shadow-utils and Samba inside THROWAWAY containers — nothing is created on
# the host or in the devcontainer. CI runs the very same
# `go test -tags integration` (see the integration jobs in .github/workflows/ci.yaml).
#
#   bash scripts/verify-integration.sh
#
# The source reaches the container through a tar on stdin rather than a bind
# mount. A bind mount is resolved by the DAEMON, not by the process running
# docker, so it only works when the daemon happens to see the repository at the
# same path — which is true inside this project's own devcontainer and false
# anywhere else, including from a sibling project or a remote daemon. Piping the
# context removes the assumption entirely.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$here"

echo ">> waiting for the docker daemon (DOCKER_HOST=${DOCKER_HOST:-unset})..."
for _ in $(seq 1 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
docker info >/dev/null 2>&1 || {
	echo "FATAL: cannot reach the docker daemon." >&2
	exit 1
}

# Run the whole suite against each account backend in its own container:
#   - shadow-utils on Debian, the backend a Debian/Ubuntu server actually uses
#   - busybox on Alpine, which really does use adduser/addgroup and has no
#     usermod, so the diff-based supplementary-group path gets exercised
# (pw/FreeBSD cannot run under Linux Docker, so it stays golden-command tested.)
run_suite() { # <image> <provider> <install-cmd>
	local image=$1 provider=$2 install=$3
	local tag="usersync-integration-$provider"

	echo ">> integration in $image (provider=$provider)..."
	tar -c --exclude=.git --exclude=dist . |
		docker build -q -t "$tag" -f - . >/dev/null <<-EOF
			FROM $image
			RUN $install
			WORKDIR /src
			COPY . .
			ENV GOFLAGS=-buildvcs=false
			ENV USERSYNC_TEST_PROVIDER=$provider
		EOF
	docker run --rm "$tag" go test -tags integration -v ./internal/integration/
}

rc=0
run_suite "golang:1.26" "shadow-utils" \
	"export DEBIAN_FRONTEND=noninteractive; apt-get update -qq && apt-get install -y -qq passwd samba samba-common-bin smbclient" || rc=1
run_suite "golang:1.26-alpine" "busybox" \
	"apk add --no-cache samba samba-common-tools samba-client shadow bash" || rc=1
exit $rc
