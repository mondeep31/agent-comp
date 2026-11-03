#!/usr/bin/env bash
# Fetch the Firecracker CI guest kernel and Ubuntu rootfs used for smoke tests
# and as the starting point for our own images.
set -euo pipefail

DEST="${1:-/var/lib/agent-comp/ci}"
ARCH="$(uname -m)"
FC_MINOR="$(firecracker --version | head -1 | sed -E 's/Firecracker v([0-9]+\.[0-9]+).*/v\1/')"
BUCKET="http://spec.ccfc.min.s3.amazonaws.com"

mkdir -p "$DEST"
cd "$DEST"

list() {
  curl -fsSL "${BUCKET}/?prefix=$1&list-type=2" | grep -oP '(?<=<Key>)[^<]+'
}

ci_dir=""
for v in "$FC_MINOR" v1.13 v1.12 v1.11 v1.10; do
  if list "firecracker-ci/${v}/${ARCH}/vmlinux-" | grep -q .; then
    ci_dir="firecracker-ci/${v}/${ARCH}"
    break
  fi
done
[ -n "$ci_dir" ] || { echo "no CI artifacts found" >&2; exit 1; }
echo "using ${ci_dir}"

kernel_key="$(list "${ci_dir}/vmlinux-" | grep -E 'vmlinux-[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1)"
rootfs_key="$(list "${ci_dir}/ubuntu-" | grep -E '\.squashfs$' | sort -V | tail -1)"

[ -f vmlinux ] || curl -fsSL -o vmlinux "${BUCKET}/${kernel_key}"
if [ ! -f rootfs.ext4 ]; then
  curl -fsSL -o rootfs.squashfs "${BUCKET}/${rootfs_key}"
  rm -rf squashfs-root
  unsquashfs -q rootfs.squashfs
  truncate -s 1G rootfs.ext4
  mkfs.ext4 -q -d squashfs-root -F rootfs.ext4
  rm -rf squashfs-root rootfs.squashfs
fi

echo "kernel: ${kernel_key}"
echo "rootfs: ${rootfs_key}"
ls -lh "$DEST"
