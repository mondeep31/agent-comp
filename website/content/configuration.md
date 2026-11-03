# Configuration

acd reads its host configuration from `/etc/agent-comp/acd.yaml`. The example below is the repository reference. Keep the API and metrics bound to loopback; expose only the API through the tunnel.

## Reference configuration

```yaml
# acd configuration. Every key is optional except images; values shown are the
# defaults. Install as /etc/agent-comp/acd.yaml.

# Keep the API on loopback: public ingress goes through the Cloudflare tunnel.
listen: 127.0.0.1:8080
# Prometheus metrics; empty disables. Never expose this through the tunnel.
metrics_listen: 127.0.0.1:9464
data_dir: /var/lib/agent-comp

firecracker:
  binary: /usr/local/bin/firecracker
  jailer_binary: /usr/local/bin/jailer
  # Keep under data_dir: images and jails must be on one mount for hard links.
  chroot_base: /var/lib/agent-comp/jailer
  uid_base: 200000
  cgroup_parent: agent-comp

images:
  - name: base
    kernel: /var/lib/agent-comp/images/vmlinux
    rootfs: /var/lib/agent-comp/images/base.ext4
    # boot_args defaults to booting agentd as init.

sandbox:
  vcpu: 1
  memory_mib: 256
  disk_mib: 2048
  ttl: 1h
  idle_timeout: 15m

limits:
  max_sandboxes: 64
  max_sandboxes_per_key: 16
  max_snapshots_per_key: 8
  max_vcpu: 2
  max_memory_mib: 2048
  max_disk_mib: 16384
  max_ttl: 24h
  max_exec_timeout: 30m
  max_file_bytes: 67108864
  requests_per_second: 20
  request_burst: 40
  disk_bandwidth_mibps: 200
  net_bandwidth_mbps: 200

pool:
  # Paused, pre-restored VMs kept per image at the default shape.
  size: 4
  refill_concurrency: 2

network:
  enabled: true
  cidr: 10.200.0.0/16
  # uplink: eth0   # default: the default route's interface
  nameservers: [8.8.8.8, 1.0.0.1]
  # deny_cidrs defaults to private, link-local, CGNAT/Tailscale and metadata ranges.

auth:
  # One "name:sha256hex" line per key. Create keys with:
  #   acctl keygen -append /etc/agent-comp/api-keys NAME
  # then reload with: systemctl reload acd (SIGHUP).
  api_keys_file: /etc/agent-comp/api-keys
```

## Operational behavior

Key reload uses SIGHUP (`systemctl reload acd`). Configuration changes requiring a service restart also terminate live sandboxes and user snapshots. The installer preserves existing configuration files, so review new defaults when upgrading.

## Sizing and quotas

Pool size is per image at the default shape. Resource caps and per-key quotas are upper bounds, not a guarantee that the host can support all combinations concurrently. Measure on the target host before increasing concurrency.
