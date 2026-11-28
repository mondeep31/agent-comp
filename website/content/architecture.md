# Architecture

agent-comp is a single-host control plane for hardware-isolated agent computers. It uses Firecracker directly, a Go host service, and a Rust guest agent.

## The request path

```text
Agent / acctl / MCP client
          │ HTTPS or WebSocket
    Cloudflare Tunnel
          │ loopback
      acd (Go)
          ├── authentication + quotas
          ├── sandbox lifecycle + warm pool
          ├── snapshots + images
          └── Firecracker driver + network namespaces
                         │ vsock
              microVM → agentd (Rust, PID 1)
                         ├── shell / process execution
                         └── files / guest configuration
```

The tunnel carries control requests. Sandbox outbound traffic uses its own network namespace and NAT path, with an internal-network deny policy.

## Components

| Component | Responsibility | Source |
| --- | --- | --- |
| acd | API, service assembly, lifecycle | `cmd/acd`, `internal/daemon` |
| acctl | CLI and stdio MCP bridge | `cmd/acctl` |
| agentd | Init, exec, files, post-restore configuration | `guest/agentd` |
| Sandbox manager | Ownership, state, TTL, quotas | `internal/sandbox` |
| Warm pool | Pre-restore, prewarm, pause, refill | `internal/pool` |
| VM driver | Firecracker, jailer, cgroups, overlays | `internal/vmm` |
| Networking | Namespaces, address allocation, nftables | `internal/network` |
| Guest protocol | Length-prefixed protobuf over vsock | `proto`, `internal/guest` |

## Why snapshots and a pool

A golden snapshot is built once for an image and resource shape. Restoring file-backed memory avoids a full boot. A background pool goes further: it restores a guest, waits for the first agent response to fault in pages, and pauses it until allocation.

On allocation, agentd receives fresh hostname, clock, entropy, and networking configuration. Golden snapshot fingerprints invalidate reuse when image or boot configuration changes.

## Storage and isolation

A shared base image is read-only. Each VM gets a private sparse writable drive; agentd mounts the guest overlay. This works on ext4 without requiring reflink support. File-backed snapshot memory shares clean pages through the host page cache, while writes become private.

Every sandbox has its own guest kernel, jail, UID/GID, resource limits, and network namespace. See [the security controls and their verification status](security.html).

## Boundaries of v1

The platform runs on one host. Sandboxes are headless Linux by default; images with a `display` add a virtual screen (Xvfb, openbox and Chromium) that agentd captures and drives natively over X11, exposed as the screenshot and computer endpoints and the MCP `computer_action` tool. Multi-host scheduling and production capacity planning remain future work. User snapshots are in-memory service resources and do not survive acd restarts; they are not backups.
