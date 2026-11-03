# Security model and hardening checklist

## Threat model

Sandboxes run arbitrary code chosen by agents, which are themselves driven by
untrusted input. Assume the code inside a sandbox is hostile and tries to:

1. escape the VM to the host or into another sandbox,
2. reach the host's networks: the LAN `10.253.0.0/24`, the tailnet
   (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`), cloud metadata, other sandboxes,
3. exhaust host resources (CPU, memory, disk, PIDs, bandwidth),
4. attack acd through the guest protocol,
5. read another tenant's sandboxes or snapshots through the API.

API clients are authenticated but not trusted beyond their own resources.
The host operator is trusted.

## Controls

Status: ✅ in place and covered by a test, 🔶 in place but not covered by an
automated test, ⏳ open.

### Isolation: one VM per sandbox

| Control | Status | Where |
| --- | --- | --- |
| Hardware virtualization (KVM): every sandbox has its own guest kernel | ✅ | `internal/vmm/firecracker` |
| Firecracker's minimal device model (virtio block, net and vsock only) | ✅ | driver |
| Firecracker's default seccomp filters (never disabled) | 🔶 | jailer args |
| jailer: per-VM chroot, unique uid/gid (`uid_base + slot`), no shared writable files | ✅ | driver, `TestBootSnapshotRestoreClones` |
| cgroup v2 per VM: `cpu.max` = vCPUs, `memory.max` = RAM + overhead, `pids.max=64` | 🔶 | driver |
| `RLIMIT_NOFILE` 1024 for the VMM | 🔶 | driver |
| Read-only shared base image; each VM writes only its private overlay | ✅ | driver, golden |
| Disk bandwidth limit (`limits.disk_bandwidth_mibps`) | 🔶 | driver rate limiter |
| Stale jails and cgroups are removed at startup | 🔶 | `cleanupStale` |
| Snapshot clones get a fresh hostname, clock and entropy mixed into the guest RNG | ✅ | `sandbox.configure`, `TestSnapshotFork` |

### Network

| Control | Status | Where |
| --- | --- | --- |
| One network namespace per VM; tap only reachable inside it | ✅ | `internal/network` |
| Egress only via NAT through the uplink; denied: RFC 1918, CGNAT/Tailscale, link-local and metadata, loopback, multicast, ULA | ✅ | `TestSandboxEgressPolicy` |
| No traffic from sandboxes to the host itself (`input` dropped from `acv*`) | ✅ | same test (host LAN and tailnet IPs with sshd) |
| Sandboxes cannot reach each other | ✅ | same test |
| Bandwidth limit per direction (`limits.net_bandwidth_mbps`) | 🔶 | Firecracker rate limiter |
| No inbound connections to sandboxes | ✅ | no port forwarding exists |

### Guest protocol (agentd → acd)

| Control | Status | Where |
| --- | --- | --- |
| Frames capped at 4 MiB; malformed frames end the connection | ✅ | `internal/guest` tests |
| Unknown error codes and file types from the guest never panic acd | 🔶 | `sandbox/ops.go` |
| Output captured for the API is capped (1 MiB per stream); file reads capped (`max_file_bytes`) | ✅ | `TestExec`, `TestWriteFileLimit` |
| Guest error messages are passed through only for known error classes | ✅ | `api.classify` |

### API and ingress

| Control | Status | Where |
| --- | --- | --- |
| acd listens on loopback only; a non-loopback `listen` logs a warning | 🔶 | `cmd/acd` |
| Public ingress only through Cloudflare Tunnel (outbound-only connection) | ✅ | `deploy/agent-comp-tunnel.service`, `scripts/host/smoke-api.sh` through the tunnel |
| Bearer API keys: 256-bit random, stored as SHA-256 digests, compared in constant time, reload on SIGHUP | ✅ | `internal/auth` |
| Every resource is owned by a key; other keys get 404 (REST, WebSocket and MCP) | ✅ | `TestOwnershipIsolation`, `TestToolsOverHTTP` |
| MCP tools take the owner from a header the auth middleware always overwrites | ✅ | `TestToolsOverHTTP` (spoofed header) |
| Per-key rate limit and quotas (sandboxes, snapshots, vCPU, memory, disk, TTL, exec timeout) | ✅ | `TestRateLimit`, `TestQuotas` |
| JSON bodies capped at 8 MiB with unknown fields rejected | ✅ | `TestCreateErrors` |
| 500 responses never include internal error text | 🔶 | `api.fail` |
| Metrics on a separate loopback listener, never routed through the tunnel | 🔶 | `metrics_listen` |
| Request IDs (Cloudflare ray id when present) in every log line and response | ✅ | `TestAuth` |

### Host and operations

| Control | Status | Where |
| --- | --- | --- |
| systemd unit with a capability bounding set, `NoNewPrivileges`, `ProtectSystem=strict`, `UMask=0077` | 🔶 | `deploy/acd.service`, verified on comp-agents |
| cloudflared runs as a dynamic user with heavy sandboxing; the token is a systemd credential, never in argv or the environment | 🔶 | `deploy/agent-comp-tunnel.service` |
| No secrets in the repo or in shared agent memory; the tunnel token lives in root-only `/etc/agent-comp/tunnel-token` | ✅ | process rule, `.gitignore` |
| The smoke test reads the API key from a file and passes it to curl through a 0600 header file | 🔶 | `scripts/host/smoke-api.sh` |
| Base image has no SSH host keys, apt lists or machine-id baked in | 🔶 | `images/build-base.sh` |
| Package manifest recorded per image build | 🔶 | `base.manifest` |
| Dependency vulnerability and license checks (`make audit`) on every push and weekly | ✅ | `.github/workflows/ci.yml` |
| CI actions pinned by commit SHA, read-only `GITHUB_TOKEN` by default, no persisted checkout credentials, Dependabot for actions, Go and Cargo | 🔶 | `.github/` |
| The self-hosted KVM runner (root via sudo) only runs commits on main and manual dispatches, never pull requests | 🔶 | `.github/workflows/integration.yml`, `scripts/host/install-runner.sh` |

## Open items

- acd runs as root. Splitting it into an unprivileged API process and a small
  privileged VM helper would shrink the blast radius of an API bug.
- Firecracker runs in the host PID namespace. jailer's `--new-pid-ns` forks
  and exits, so the driver would need to track the VMM by pidfd instead.
- Snapshot clones share the golden VM's guest kernel RNG state until
  `Configure` mixes in fresh entropy, a few milliseconds after resume.
  VMGenID support in the guest kernel would reseed it at resume.
- Egress allowlists per key (only `pypi.org`, `github.com`, …) and per-key
  bandwidth accounting.
- Image signing and an SBOM for `base.ext4`.
- A persistent audit log of API actions per key.
- The per-VM cgroups live under a top-level `agent-comp` cgroup instead of a
  systemd-delegated subtree.
