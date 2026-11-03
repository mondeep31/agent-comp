#!/usr/bin/env bash
# Build the "base" sandbox image: an Ubuntu rootfs with common developer tools
# and agentd as init, packed into a read-only ext4 file.
#
# Usage: sudo images/build-base.sh [OUT_DIR]
#
# Writes OUT_DIR/base.ext4 (mode 0444) and OUT_DIR/base.manifest (installed
# packages and versions). Needs debootstrap, e2fsprogs and bin/agentd
# (make build-agentd). Default OUT_DIR is /var/lib/agent-comp/images.
set -euo pipefail

OUT_DIR="${1:-/var/lib/agent-comp/images}"
SUITE="${SUITE:-noble}"
MIRROR="${MIRROR:-http://archive.ubuntu.com/ubuntu}"
NAME="${NAME:-base}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
AGENTD="${AGENTD:-$REPO_ROOT/bin/agentd}"

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

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
[ -x "$AGENTD" ] || { echo "missing $AGENTD (make build-agentd)" >&2; exit 1; }

work="$(mktemp -d /var/tmp/agent-comp-image.XXXXXX)"
root="$work/root"
cleanup() {
  for m in dev/pts dev proc sys; do
    mountpoint -q "$root/$m" && umount -l "$root/$m"
  done
  rm -rf "$work"
}
trap cleanup EXIT

echo "==> debootstrap $SUITE"
debootstrap --variant=minbase --components=main,universe "$SUITE" "$root" "$MIRROR" >/dev/null

mount -t proc proc "$root/proc"
mount -t sysfs sys "$root/sys"
mount --bind /dev "$root/dev"
mount --bind /dev/pts "$root/dev/pts"

# Keep package scripts from starting services inside the chroot.
printf '#!/bin/sh\nexit 101\n' >"$root/usr/sbin/policy-rc.d"
chmod 0755 "$root/usr/sbin/policy-rc.d"

cat >"$root/etc/apt/sources.list" <<EOF
deb $MIRROR $SUITE main universe
deb $MIRROR $SUITE-updates main universe
deb $MIRROR $SUITE-security main universe
EOF
cat >"$root/etc/dpkg/dpkg.cfg.d/01-nodoc" <<'EOF'
path-exclude=/usr/share/doc/*
path-include=/usr/share/doc/*/copyright
path-exclude=/usr/share/man/*
path-exclude=/usr/share/info/*
path-exclude=/usr/share/locale/*
path-include=/usr/share/locale/locale.alias
EOF

echo "==> installing packages"
cp /etc/resolv.conf "$root/etc/resolv.conf"
chroot "$root" /usr/bin/env DEBIAN_FRONTEND=noninteractive bash -euo pipefail -c "
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends ${PACKAGES[*]} >/dev/null
  apt-get dist-upgrade -y -qq >/dev/null
  apt-get clean
"

echo "==> configuring"
install -m 0755 "$AGENTD" "$root/usr/local/bin/agentd"
rm -f "$root/usr/sbin/policy-rc.d"
# agentd writes resolv.conf and the hostname at boot; neither may be a
# symlink into a service that does not run here.
rm -f "$root/etc/resolv.conf" "$root/etc/hostname"
: >"$root/etc/resolv.conf"
echo sandbox >"$root/etc/hostname"
cat >"$root/etc/hosts" <<'EOF'
127.0.0.1 localhost
::1 localhost ip6-localhost ip6-loopback
EOF
: >"$root/etc/machine-id"
mkdir -p "$root/work"
cat >"$root/etc/profile.d/agent-comp.sh" <<'EOF'
export PIP_DISABLE_PIP_VERSION_CHECK=1
export PIP_BREAK_SYSTEM_PACKAGES=1
EOF
# Remove credentials and caches that must not be shared by every sandbox.
rm -rf "$root"/var/lib/apt/lists/* "$root"/var/cache/apt/*.bin "$root"/var/log/*.log \
  "$root"/etc/ssh/ssh_host_* "$root"/root/.cache "$root"/tmp/* "$root"/var/tmp/*

chroot "$root" dpkg-query -W -f '${Package}\t${Version}\n' >"$work/manifest"

for m in dev/pts dev proc sys; do umount "$root/$m"; done

echo "==> packing ext4"
used_mib=$(du -sm "$root" | cut -f1)
size_mib=$((used_mib + used_mib / 5 + 64))
mkdir -p "$OUT_DIR"
tmp="$OUT_DIR/.$NAME.ext4.tmp"
rm -f "$tmp"
truncate -s "${size_mib}M" "$tmp"
mkfs.ext4 -q -F -L "$NAME" -m 0 -E root_owner=0:0 -d "$root" "$tmp"
e2fsck -fy "$tmp" >/dev/null
chmod 0444 "$tmp"
mv -f "$tmp" "$OUT_DIR/$NAME.ext4"
install -m 0644 "$work/manifest" "$OUT_DIR/$NAME.manifest"

echo "==> $OUT_DIR/$NAME.ext4 (${size_mib} MiB, $(wc -l <"$work/manifest") packages)"
