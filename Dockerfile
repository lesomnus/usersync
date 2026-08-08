ARG BUILD_HASH="0000000000000000000000000000000000000000"
ARG BUILD_ID="r0"
ARG APP_VERSION="000000-r0"

FROM golang:1.26 AS base

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ENV CGO_ENABLED=0



FROM base AS test

RUN --mount=type=cache,target=/root/.cache/go-build \
	go test -v -trimpath ./...



FROM base AS builder

ARG BUILD_HASH
ARG BUILD_ID
ARG APP_VERSION
RUN BUILD_HASH=${BUILD_HASH} \
	BUILD_ID=${BUILD_ID} \
	APP_VERSION=${APP_VERSION} \
	./scripts/gen-version-file.sh

ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
	mkdir /dist \
	&& GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o /dist/arm64 . \
	&& GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /dist/amd64 . \
	&& "/dist/${TARGETARCH}" version

FROM scratch AS build
COPY --from=builder /dist/ /



# The published image is a BINARY CARRIER, not a runtime.
#
# usersync drives the OS account tools — getent, useradd/adduser/pw, smbpasswd,
# pdbedit, testparm — and none of them exist in a scratch filesystem, so every
# command that touches system state fails here however it is invoked. What this
# image IS good for is handing the binary to an image that has those tools,
# without dragging a Go toolchain into the consumer's build:
#
#   COPY --from=ghcr.io/lesomnus/usersync:v0.1.0 /usersync /usr/local/bin/usersync
#
# The non-root USER and the --help default keep `docker run` on this image
# harmless rather than misleading: it prints usage instead of half-pretending to
# be a deployment. See admin-guide.md §1.1.
FROM scratch AS app

ARG TARGETARCH
COPY "${TARGETARCH}" /usersync

USER 65532:65532
ENTRYPOINT ["/usersync"]
CMD ["--help"]
