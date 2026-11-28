# Headless developer image: Ubuntu with common build tools and runtimes.
# shellcheck shell=bash disable=SC2034
NAME="${NAME:-base}"
SUITE="${SUITE:-noble}"
MIRROR="${MIRROR:-http://archive.ubuntu.com/ubuntu}"
COMPONENTS=main,universe

PACKAGES=(
  # agentd and networking
  iproute2 iputils-ping ca-certificates netbase
  # shell basics
  bash coreutils procps psmisc less file findutils grep sed gawk diffutils patch
  tar gzip bzip2 xz-utils zstd unzip zip jq tree vim-tiny nano
  # fetching code and data
  curl wget git openssh-client rsync dnsutils
  # building and running code
  build-essential pkg-config make cmake
  python3 python3-pip python3-venv python3-dev python-is-python3
  nodejs npm
  sqlite3
)

write_sources() {
  cat >"$root/etc/apt/sources.list" <<X
deb $MIRROR $SUITE main universe
deb $MIRROR $SUITE-updates main universe
deb $MIRROR $SUITE-security main universe
X
}
