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
img="${VERIFY_IMAGE:-golang:1.26}"

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

# The repo is bind-mounted from /workspace (shared with the dind daemon at the
# same path); the driver is streamed over stdin so it needs no shared path.
echo ">> running 'go test -tags integration' in a throwaway $img container..."
docker run --rm -i \
	-v "$here:/src" -w /src \
	-e GOFLAGS=-buildvcs=false \
	"$img" bash -s <<'SCRIPT'
set -eu
export DEBIAN_FRONTEND=noninteractive
echo ">> installing shadow-utils + samba..."
apt-get update -qq
apt-get install -y -qq passwd samba samba-common-bin smbclient >/dev/null
echo ">> go test -tags integration ..."
go test -tags integration -v ./internal/integration/
SCRIPT
