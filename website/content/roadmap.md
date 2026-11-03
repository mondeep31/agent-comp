# Status & next steps

Status snapshot: **3 November 2026**. This page combines the repository's implemented behavior with the latest innernet handoff. It is a development milestone, not a production-readiness certification.

## What is implemented

The v1 implementation stages have shipped: Firecracker driver, Rust guest agent, golden snapshots and warm pool, isolated networking, authenticated lifecycle APIs, WebSocket execution, MCP, images, hardening controls, and CI/release pipelines.

The development service runs with the Ubuntu 24.04 base image, networking enabled, and a pool of four. CI and KVM integration were green at the handoff. The existing implementation-plan checkboxes are historical and do not reflect that completed state.

## 1. Establish a stable API endpoint

**Needs:** an operator-selected domain on Cloudflare.

Create a named Cloudflare tunnel and route its public hostname to the loopback API. Install its token as a root-only systemd credential, then run the long API smoke test through that hostname. Keep metrics private.

**Done when:** authentication, create, file operations, egress denial, MCP, and a silent 130-second WebSocket command pass through the permanent URL. See [Deployment](deploy.html).

## 2. Issue credentials per agent

Replace shared development credentials with named API keys for each agent or tenant. Names determine resource ownership. Reload acd to apply key changes without dropping VMs, and establish revocation and rotation procedures.

**Done when:** each agent has its own secret delivery path and ownership isolation is verified using different keys.

## 3. Close the security backlog

These are remaining tasks, not existing guarantees:

| Priority | Work | Completion evidence |
| --- | --- | --- |
| First | Split the root daemon into an unprivileged API and a narrow privileged VM helper | API compromise cannot invoke arbitrary privileged host operations |
| First | Move Firecracker into a separate PID namespace; track VMMs with pidfds | Lifecycle and cleanup integration tests pass |
| Next | Add VMGenID-aware guest RNG reseeding | Fresh entropy at resume, before user execution |
| Next | Add per-key egress allowlists and bandwidth accounting | Policy and accounting tests across multiple keys |
| Next | Sign images and generate SBOMs | Verified image provenance in the build/deploy path |
| Next | Persist per-key API audit events | Restart-safe, attributable operation history |
| Next | Use a systemd-delegated cgroup subtree | Limits and cleanup verified under delegation |

The priority order above is a documentation recommendation. [Security & isolation](security.html) distinguishes tested controls from implemented controls that still lack automated coverage.

## 4. Validate production capacity

Choose production hardware and measure restore latency, concurrency, density, resource exhaustion, and long-duration behavior there. Every current published benchmark is from nested virtualization on the development host. No bare-metal SLO or production density has been established.

Decide required scale before introducing multi-host scheduling. Durable user snapshot storage and restart recovery also need a separate design if the product requires persistent computers.

## Keep this page current

After a stage ships, record its commit and evidence in innernet, update the relevant reference guide, and revise this page. Keep completed implementation work separate from deployment prerequisites and proposed future work.
