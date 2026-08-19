# usersync-smb — the SMB file server, owned by usersync.
#
# The serving half of the darak split (nas-design ADR-9 → two images): smbd +
# winbindd + usersync, and NO darak binary, because darak does nothing special to
# SMB. usersync registers the tdbsam accounts, creates the group/home folders,
# and generates smb.conf's share block; smbd serves them. The container's
# long-running process is `usersync watch --reload-smb` — the reconcile loop that
# used to live in darak's entrypoint as bash + inotifywait, now one command with
# usersync's own guarantees (validate-before-apply, the run lock, degrade on a
# missing quota backend).
#
# Build context is the usersync repo root.
ARG BUILD_HASH="0000000000000000000000000000000000000000"
ARG BUILD_ID="r0"
ARG APP_VERSION="000000-r0"

FROM golang:1.26-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG BUILD_HASH BUILD_ID APP_VERSION
RUN BUILD_HASH=${BUILD_HASH} BUILD_ID=${BUILD_ID} APP_VERSION=${APP_VERSION} \
	./scripts/gen-version-file.sh
# No GOARCH: buildx runs this stage once per --platform (native for amd64, qemu
# for arm64), so the version check runs the binary it just built.
RUN --mount=type=cache,target=/root/.cache/go-build \
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /usersync . \
	&& /usersync version

FROM debian:trixie-slim AS app

# Same package recipe as the darak prod image, MINUS what only the web tier
# needs: curl (its HTTP health check) and inotify-tools (its bash roster watcher,
# which `usersync watch` replaces here).
#
# samba          — smbd, the file-serving path
# samba-common-bin — pdbedit/smbpasswd, which usersync drives for tdbsam
# winbind        — ntlm_auth, the web pod's password check (a winbind client even
#                  standalone), and the domain client when an on-prem AD arrives
# acl, attr      — POSIX ACLs and xattrs on the data volume
# zfsutils-linux — the `zfs` CLI for per-uid quota enforcement; userspace only,
#                  driving the HOST's loaded module over a mounted /dev/zfs
#                  (best-effort — a missing /dev/zfs warns and skips, never fails)
#
# ZFS is CDDL, so zfsutils-linux is in Debian `contrib`, not `main`; trixie-slim
# ships only `main`, so contrib is enabled first (deb822, with the classic
# sources.list as a fallback). The distro's own smb.conf ([homes]/[printers]/
# [print$]) is removed so the entrypoint's "seed the global section only when the
# file is absent" means what it says.
RUN ( [ -f /etc/apt/sources.list.d/debian.sources ] \
		&& sed -i '/^Components:/ s/$/ contrib/' /etc/apt/sources.list.d/debian.sources \
		|| sed -i 's/ main$/ main contrib/' /etc/apt/sources.list ) \
	&& apt-get update -qq \
	&& DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
		samba samba-common-bin winbind acl attr ca-certificates tzdata zfsutils-linux \
	&& rm -rf /var/lib/apt/lists/* \
	&& rm -f /etc/samba/smb.conf

COPY --from=build /usersync /usr/local/bin/usersync
COPY deploy/smb-server/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/usersync /usr/local/bin/entrypoint.sh

# 445 SMB. With host networking it is not published; it is the host's own port.
EXPOSE 445

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
