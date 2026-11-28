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
  # Caps for all VMs together (memory.max and cpu.weight of cgroup_parent),
  # so co-hosted services keep their share. 0 leaves them unset.
  cgroup_memory_max_mib: 0
  cgroup_cpu_weight: 0

images:
  - name: base
    kernel: /var/lib/agent-comp/images/vmlinux
    rootfs: /var/lib/agent-comp/images/base.ext4
    # boot_args defaults to booting agentd as init.
  # A computer with a screen and a browser (make image-desktop). Images may
  # override the default shape and pool size; display images need at least
  # 768 MiB. Remove this entry if desktop.ext4 is not built.
  # - name: desktop
  #   kernel: /var/lib/agent-comp/images/vmlinux
  #   rootfs: /var/lib/agent-comp/images/desktop.ext4
  #   display: {width: 1280, height: 800}
  #   sandbox: {memory_mib: 1024}
  #   pool_size: 1

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
  # Summed memory of all live sandboxes; 0 means unlimited.
  max_committed_memory_mib: 0
  requests_per_second: 20
  request_burst: 40
  disk_bandwidth_mibps: 200
  net_bandwidth_mbps: 200

pool:
  # Paused, pre-restored VMs kept per image at its default shape, unless the
  # image sets pool_size.
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

Pool size is per image at that image's default shape; an image's `pool_size` overrides `pool.size`. `limits.max_committed_memory_mib` refuses new sandboxes once the memory of live ones would pass it, and `firecracker.cgroup_memory_max_mib` caps the VMs' actual memory together, which protects other services on a shared host.

## Desktop images

An image with `display` boots with a virtual screen: agentd starts Xvfb and the image's desktop session (openbox and Chromium in `make image-desktop`) before the golden snapshot, so sandboxes and pooled VMs start with the browser already open. Screenshots and input go through `GET /v1/sandboxes/{id}/screenshot`, `POST /v1/sandboxes/{id}/computer` and the MCP `computer_action` tool. Give display images at least 768 MiB (1024 is comfortable) and a small pool; each idle pooled desktop costs about 25 MiB of host memory and an active one about 80-300 MiB on the dev host. Resource caps and per-key quotas are upper bounds, not a guarantee that the host can support all combinations concurrently. Measure on the target host before increasing concurrency.
