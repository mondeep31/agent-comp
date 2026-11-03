#!/usr/bin/env bash
# Smoke test and baseline timings for Firecracker on this host:
#   1. cold boot to a userspace marker
#   2. full snapshot create
#   3. snapshot restore to first guest output
# Results are dev-host numbers (see HOST_TAG); do not compare against bare metal.
set -euo pipefail

CI="${CI_DIR:-/var/lib/agent-comp/ci}"
WORK="$(mktemp -d /tmp/fc-smoke.XXXXXX)"
HOST_TAG="${HOST_TAG:-dev-nested-$(hostname)}"
trap 'pkill -f "$WORK" 2>/dev/null || true; sudo umount "$WORK/mnt" 2>/dev/null || true; rm -rf "$WORK"' EXIT

now_ms() { echo $(( $(date +%s%N) / 1000000 )); }

api() {
  local sock="$1" method="$2" path="$3" body="${4:-}"
  local code
  code="$(curl -sS --unix-socket "$sock" -o "$WORK/resp" -w '%{http_code}' -X "$method" \
    -H 'Content-Type: application/json' "http://localhost${path}" ${body:+-d "$body"})"
  if [ "${code:0:1}" != "2" ]; then
    echo "API $method $path -> $code: $(cat "$WORK/resp")" >&2
    return 1
  fi
}

wait_for() {
  local file="$1" pattern="$2" timeout_ms="${3:-20000}" start
  start="$(now_ms)"
  until grep -q "$pattern" "$file" 2>/dev/null; do
    [ $(( $(now_ms) - start )) -lt "$timeout_ms" ] || { echo "timeout waiting for $pattern" >&2; tail -20 "$file" >&2; return 1; }
    sleep 0.005
  done
}

# Build a rootfs copy with two tiny init programs.
cp --sparse=always "$CI/rootfs.ext4" "$WORK/rootfs.ext4"
mkdir "$WORK/mnt"
sudo mount -o loop "$WORK/rootfs.ext4" "$WORK/mnt"
sudo tee "$WORK/mnt/smoke-reboot" >/dev/null <<'EOF'
#!/bin/sh
mount -t proc proc /proc
echo SMOKE_READY
echo b > /proc/sysrq-trigger
EOF
sudo tee "$WORK/mnt/smoke-idle" >/dev/null <<'EOF'
#!/bin/sh
mount -t proc proc /proc
echo SMOKE_READY
i=0
while :; do i=$((i+1)); echo "TICK $i"; sleep 0.05; done
EOF
sudo chmod 0755 "$WORK/mnt/smoke-reboot" "$WORK/mnt/smoke-idle"
sudo umount "$WORK/mnt"

BOOT_ARGS_BASE="console=ttyS0 reboot=k panic=1 pci=off ro quiet"

# 1. Cold boot to userspace marker.
cat > "$WORK/cold.json" <<EOF
{
  "boot-source": {"kernel_image_path": "$CI/vmlinux", "boot_args": "$BOOT_ARGS_BASE init=/smoke-reboot"},
  "drives": [{"drive_id": "rootfs", "path_on_host": "$WORK/rootfs.ext4", "is_root_device": true, "is_read_only": true}],
  "machine-config": {"vcpu_count": 1, "mem_size_mib": 256}
}
EOF
t0="$(now_ms)"
firecracker --no-api --config-file "$WORK/cold.json" --level Error 2>&1 | grep -m1 -q SMOKE_READY
t1="$(now_ms)"
cold_ms=$(( t1 - t0 ))

# 2. Boot an idle VM via the API and snapshot it.
S1="$WORK/fc1.sock"
firecracker --api-sock "$S1" --level Error > "$WORK/fc1.log" 2>&1 &
until [ -S "$S1" ]; do sleep 0.005; done
api "$S1" PUT /boot-source "{\"kernel_image_path\":\"$CI/vmlinux\",\"boot_args\":\"$BOOT_ARGS_BASE init=/smoke-idle\"}"
api "$S1" PUT /drives/rootfs "{\"drive_id\":\"rootfs\",\"path_on_host\":\"$WORK/rootfs.ext4\",\"is_root_device\":true,\"is_read_only\":true}"
api "$S1" PUT /machine-config '{"vcpu_count":1,"mem_size_mib":256}'
api "$S1" PUT /actions '{"action_type":"InstanceStart"}'
wait_for "$WORK/fc1.log" "TICK 3"

api "$S1" PATCH /vm '{"state":"Paused"}'
t0="$(now_ms)"
api "$S1" PUT /snapshot/create "{\"snapshot_type\":\"Full\",\"snapshot_path\":\"$WORK/vm.snap\",\"mem_file_path\":\"$WORK/vm.mem\"}"
t1="$(now_ms)"
snap_ms=$(( t1 - t0 ))
pkill -f "$S1" || true
sleep 0.2

# 3. Restore in a fresh Firecracker process and wait for new guest output.
S2="$WORK/fc2.sock"
t0="$(now_ms)"
firecracker --api-sock "$S2" --level Error > "$WORK/fc2.log" 2>&1 &
until [ -S "$S2" ]; do sleep 0.001; done
api "$S2" PUT /snapshot/load "{\"snapshot_path\":\"$WORK/vm.snap\",\"mem_backend\":{\"backend_type\":\"File\",\"backend_path\":\"$WORK/vm.mem\"},\"resume_vm\":true}"
t1="$(now_ms)"
wait_for "$WORK/fc2.log" "TICK" 5000
t2="$(now_ms)"
restore_api_ms=$(( t1 - t0 ))
restore_output_ms=$(( t2 - t0 ))
pkill -f "$S2" || true

cat <<EOF
{"host_tag":"$HOST_TAG","firecracker":"$(firecracker --version | head -1)","vcpu":1,"mem_mib":256,
 "cold_boot_to_userspace_ms":$cold_ms,"snapshot_create_ms":$snap_ms,
 "restore_api_ms":$restore_api_ms,"restore_to_guest_output_ms":$restore_output_ms}
EOF
