#!/usr/bin/env bash
# Register this KVM host as a GitHub Actions self-hosted runner for the
# integration workflow. The runner gets its own user with passwordless sudo
# (the tests run Firecracker under jailer as root), so treat any workflow that
# targets the `kvm` label as root on this host: only trusted refs may use it.
#
# The registration token is short-lived and read from stdin, never argv:
#   gh api -X POST repos/OWNER/REPO/actions/runners/registration-token -q .token \
#     | ssh HOST 'sudo bash agent-comp/scripts/host/install-runner.sh OWNER/REPO'
set -euo pipefail

REPO="${1:?usage: install-runner.sh OWNER/REPO < token}"
RUNNER_USER=ac-runner
RUNNER_DIR=/opt/actions-runner
RUNNER_VERSION=2.338.2
RUNNER_SHA256=baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40
RUST_TOOLCHAIN=1.99.1

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
IFS= read -r TOKEN
[ -n "$TOKEN" ] || { echo "no registration token on stdin" >&2; exit 1; }

if ! id "$RUNNER_USER" >/dev/null 2>&1; then
  useradd --create-home --shell /bin/bash "$RUNNER_USER"
fi
usermod -aG kvm "$RUNNER_USER"
install -m 0440 /dev/stdin "/etc/sudoers.d/$RUNNER_USER" <<EOF
$RUNNER_USER ALL=(root) NOPASSWD: ALL
Defaults:$RUNNER_USER !requiretty
EOF
visudo -cf "/etc/sudoers.d/$RUNNER_USER" >/dev/null

home="$(getent passwd "$RUNNER_USER" | cut -d: -f6)"
if [ ! -x "$home/.cargo/bin/rustup" ]; then
  sudo -u "$RUNNER_USER" -H bash -c \
    "curl -fsSL --proto '=https' --tlsv1.2 https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain $RUST_TOOLCHAIN --target x86_64-unknown-linux-musl -c clippy,rustfmt"
fi

install -d -o "$RUNNER_USER" -g "$RUNNER_USER" -m 0750 "$RUNNER_DIR"
if [ ! -x "$RUNNER_DIR/config.sh" ]; then
  tarball="actions-runner-linux-x64-$RUNNER_VERSION.tar.gz"
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL -o "$tmp/$tarball" \
    "https://github.com/actions/runner/releases/download/v$RUNNER_VERSION/$tarball"
  echo "$RUNNER_SHA256  $tmp/$tarball" | sha256sum -c --quiet
  tar -C "$RUNNER_DIR" -xzf "$tmp/$tarball"
  chown -R "$RUNNER_USER:$RUNNER_USER" "$RUNNER_DIR"
  "$RUNNER_DIR/bin/installdependencies.sh" >/dev/null
fi

if [ ! -f "$RUNNER_DIR/.runner" ]; then
  sudo -u "$RUNNER_USER" -H "$RUNNER_DIR/config.sh" --unattended \
    --url "https://github.com/$REPO" --token "$TOKEN" \
    --name "$(hostname)-kvm" --labels kvm --work _work --replace
fi

# config.sh writes .path from its own environment; replace it afterwards.
printf '%s\n' "$home/.cargo/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
  | install -o "$RUNNER_USER" -g "$RUNNER_USER" -m 0644 /dev/stdin "$RUNNER_DIR/.path"

cd "$RUNNER_DIR"
./svc.sh install "$RUNNER_USER" >/dev/null 2>&1 || true
./svc.sh stop >/dev/null 2>&1 || true
./svc.sh start
./svc.sh status | head -5
