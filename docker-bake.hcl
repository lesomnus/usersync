variable "TAG" {
  default = "local"
}
variable "REPO" {
  default = "ghcr.io/lesomnus/usersync"
}
variable "BUILD_HASH" {
  default = "0000000000000000000000000000000000000000"
}
variable "BUILD_TIMESTAMP" {
  default = "${timestamp()}"
}
variable "BUILD_DATE" {
  default = "${formatdate("YYMMDD", BUILD_TIMESTAMP)}"
}
variable "BUILD_ID" {
  default = "r0"
}
variable "APP_VERSION" {
  default = "${BUILD_DATE}-${BUILD_ID}"
}

target "test" {
  target = "test"
}
target "build" {
  target = "build"
  args = {
    BUILD_HASH  = BUILD_HASH
    BUILD_ID    = BUILD_ID
    APP_VERSION = APP_VERSION
  }
  output = [{
    type = "local"
    dest = "dist"
  }]
}
target "app" {
  target = "app"
  context = "./dist"
  dockerfile = "../Dockerfile"
  labels = {
    "org.opencontainers.image.title"         = "usersync",
    "org.opencontainers.image.licenses"      = "Apache-2.0",
    # "org.opencontainers.image.description"   = "",
    # "org.opencontainers.image.documentation" = "",
    "org.opencontainers.image.url"           = "https://github.com/lesomnus/usersync",
    # "org.opencontainers.image.vendor"        = "",
    "org.opencontainers.image.revision"      = "${BUILD_HASH}",
    "org.opencontainers.image.version"       = "${APP_VERSION}",
  }
  tags = [
    "${REPO}:${TAG}",
    "${REPO}:${BUILD_ID}",
    "${REPO}:${BUILD_DATE}",
    "${REPO}:${BUILD_DATE}-${BUILD_ID}",
  ]
}

# The SMB file-server image (deploy/smb-server): a real runtime, not a binary
# carrier — smbd + winbindd + usersync on Debian. Built from source with the repo
# root as context (it needs the Go tree and the entrypoint), so it does NOT use
# the ./dist pipeline the `app` target does. Published under a SEPARATE image name
# so its digest pins independently of the CLI carrier and of the darak web image.
target "smb" {
  target     = "app"
  context    = "."
  dockerfile = "deploy/smb-server/Dockerfile"
  args = {
    BUILD_HASH  = BUILD_HASH
    BUILD_ID    = BUILD_ID
    APP_VERSION = APP_VERSION
  }
  labels = {
    "org.opencontainers.image.title"    = "usersync-smb",
    "org.opencontainers.image.licenses" = "Apache-2.0",
    "org.opencontainers.image.url"      = "https://github.com/lesomnus/usersync",
    "org.opencontainers.image.revision" = "${BUILD_HASH}",
    "org.opencontainers.image.version"  = "${APP_VERSION}",
  }
  tags = [
    "${REPO}-smb:${TAG}",
    "${REPO}-smb:${BUILD_ID}",
    "${REPO}-smb:${BUILD_DATE}",
    "${REPO}-smb:${BUILD_DATE}-${BUILD_ID}",
  ]
}
