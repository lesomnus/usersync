#!/usr/bin/env bash
#
# End-to-end integration verification for usersync.
#
# Runs the tagged Go integration test (internal/integration) as root against
# REAL shadow-utils and Samba inside a THROWAWAY container that is removed on
# exit — nothing is created on the host or the devcontainer. CI runs the very
# same `go test -tags integration` (see .github/workflows/integration.yaml).
#
# Requires the `docker` (dind) sidecar from .devcontainer/docker-compose.yaml
# (DOCKER_HOST=tcp://docker:2375).
#
#   bash scripts/verify-integration.sh
#
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$here"

# --- wait for the dind docker daemon (the sidecar may still be starting) ---
echo ">> waiting for the docker daemon (DOCKER_HOST=${DOCKER_HOST:-unset})..."
for _ in $(seq 1 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
docker info >/dev/null 2>&1 || {
	echo "FATAL: cannot reach the docker daemon."
	echo "  Expected the 'docker' dind sidecar from .devcontainer/docker-compose.yaml"
	echo "  with DOCKER_HOST=tcp://docker:2375 set in the dev service."
	echo "  Rebuild the devcontainer so both services come up."
	exit 1
}

# The repo is bind-mounted at /src (shared with the dind daemon via /workspace).
# Run the whole suite against each account backend in its own container:
#   - shadow-utils on Debian (golang:1.26)
#   - busybox on Alpine (golang:1.26-alpine), which really uses adduser/addgroup
# (pw/FreeBSD cannot run under Linux Docker, so it stays golden-command tested.)
run_suite() { # <image> <provider> <install-cmd>
	echo ">> integration in $1 (provider=$2)..."
	docker run --rm -i \
		-v "$here:/src" -w /src \
		-e GOFLAGS=-buildvcs=false \
		-e USERSYNC_TEST_PROVIDER="$2" \
		"$1" sh -c "set -e; echo '   installing deps...'; $3 >/dev/null 2>&1; go test -tags integration -v ./internal/integration/"
}

rc=0
run_suite "golang:1.26" "shadow-utils" \
	"export DEBIAN_FRONTEND=noninteractive; apt-get update -qq && apt-get install -y -qq passwd samba samba-common-bin smbclient" || rc=1
run_suite "golang:1.26-alpine" "busybox" \
	"apk add --no-cache samba samba-common-tools samba-client shadow" || rc=1
exit $rc
