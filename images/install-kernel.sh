#!/usr/bin/env bash
# Install the guest kernel next to the images. v1 uses the Firecracker CI
# kernel (fetched by scripts/host/fetch-ci-artifacts.sh): it has virtio,
# vsock, overlayfs and the network options agentd needs.
#
# Usage: sudo images/install-kernel.sh [CI_DIR] [OUT_DIR]
set -euo pipefail

CI_DIR="${1:-/var/lib/agent-comp/ci}"
OUT_DIR="${2:-/var/lib/agent-comp/images}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

[ -f "$CI_DIR/vmlinux" ] || "$REPO_ROOT/scripts/host/fetch-ci-artifacts.sh" "$CI_DIR"
mkdir -p "$OUT_DIR"
install -m 0444 "$CI_DIR/vmlinux" "$OUT_DIR/vmlinux.tmp"
mv -f "$OUT_DIR/vmlinux.tmp" "$OUT_DIR/vmlinux"
echo "==> $OUT_DIR/vmlinux"
