#!/usr/bin/env bash
# Install acd, acctl and the systemd units on this host. Safe to re-run:
# existing config, keys and tunnel token are kept.
#
# Usage (from the repo, after `make build image`): sudo deploy/install.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ETC=/etc/agent-comp

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
for f in bin/acd bin/acctl; do
  [ -x "$REPO_ROOT/$f" ] || { echo "missing $f (make build)" >&2; exit 1; }
done
for f in /var/lib/agent-comp/images/vmlinux /var/lib/agent-comp/images/base.ext4; do
  [ -f "$f" ] || { echo "missing $f (make image)" >&2; exit 1; }
done

install -m 0755 "$REPO_ROOT/bin/acd" "$REPO_ROOT/bin/acctl" /usr/local/bin/
install -d -m 0755 "$ETC"
install -d -m 0711 /var/lib/agent-comp

if [ ! -f "$ETC/acd.yaml" ]; then
  install -m 0644 "$REPO_ROOT/deploy/acd.example.yaml" "$ETC/acd.yaml"
  echo "installed $ETC/acd.yaml"
fi
if [ ! -f "$ETC/api-keys" ]; then
  install -m 0600 /dev/null "$ETC/api-keys"
  echo "created empty $ETC/api-keys; add keys with: sudo acctl keygen -append $ETC/api-keys NAME"
fi

install -m 0644 "$REPO_ROOT/deploy/acd.service" /etc/systemd/system/acd.service
install -m 0644 "$REPO_ROOT/deploy/agent-comp-tunnel.service" /etc/systemd/system/agent-comp-tunnel.service
systemctl daemon-reload
systemctl enable acd.service >/dev/null
systemctl restart acd.service

if [ -f "$ETC/tunnel-token" ]; then
  chmod 0600 "$ETC/tunnel-token"
  systemctl enable agent-comp-tunnel.service >/dev/null
  systemctl restart agent-comp-tunnel.service
else
  echo "no $ETC/tunnel-token yet; see docs/DEPLOY.md to create the tunnel"
fi

for _ in $(seq 1 50); do
  curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break
  sleep 0.2
done
systemctl --no-pager --lines=0 status acd.service
curl -fsS http://127.0.0.1:8080/healthz && echo
