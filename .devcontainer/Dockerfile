FROM ghcr.io/lesomnus/dev-golang:1.26

# Docker CLI so the dev container can drive the `docker` (dind) sidecar service
# via DOCKER_HOST (see docker-compose.yaml). Copied from the official CLI image,
# so nothing beyond the image pull is needed at build time.
COPY --from=docker:27-cli /usr/local/bin/docker /usr/local/bin/docker
