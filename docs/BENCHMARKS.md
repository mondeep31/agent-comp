# Benchmarks

Every result is tagged with the host it ran on. `dev-nested-*` numbers come from a KVM guest running Firecracker under nested virtualization and are not representative of bare metal.

## Host baseline (Step 0)

Script: `scripts/host/smoke-boot.sh`. Guest: Firecracker CI kernel 6.1.141 with the Ubuntu 24.04 CI rootfs, 1 vCPU, 256 MiB, read-only root, a trivial `/bin/sh` init.

| Host tag | Firecracker | Cold boot to userspace | Snapshot create (full, 256 MiB) | Snapshot load API | Restore to guest output |
| --- | --- | --- | --- | --- | --- |
| dev-nested-comp-agents | v1.17.0 | 1015-1073 ms | 439-465 ms | 19-24 ms | 69-71 ms |

Host: Ubuntu 24.04.5, kernel 6.8.0-106, 4 vCPU (VT-x, nested), 15 GiB RAM, ext4. Three runs on 2026-11-03.

Takeaways:

- Snapshot restore is about 15x faster than a cold boot, even nested. Golden snapshots are the default create path.
- Most of the restore time after the load API returns is lazy page faulting of the file-backed memory; a warm pool of pre-restored, paused VMs removes it from the request path.

## Golden snapshots and warm pool

Test: `make integration PKG=./internal/pool/ RUN=TestGoldenAndPool` (needs `make build-agentd`). Guest: CI kernel and rootfs with agentd as init, 1 vCPU, 256 MiB, 1 GiB overlay disk. "Configured" means the host has received agentd's reply to `Configure` (hostname, clock, entropy), which is the moment a sandbox is usable.

| Host tag | Path | Result |
| --- | --- | --- |
| dev-nested-comp-agents | Golden build (cold boot, agentd ready, sync, snapshot) | 3.3-3.4 s, once per (image, shape) |
| dev-nested-comp-agents | Golden reuse from disk (fingerprint check) | 0.5 ms |
| dev-nested-comp-agents | Warm acquire (resume prewarmed VM) to configured | p50 9.7 ms, max 11.8 ms (n=8) |
| dev-nested-comp-agents | On-demand restore to configured | p50 80 ms, max 84 ms (n=5) |
| dev-nested-comp-agents | 6 parallel on-demand restores to configured | p50 133 ms, wall 140 ms |

Run on 2026-11-03.

Takeaways:

- After restore, agentd's first reply took about 45 ms and each later request about 3-4 ms, while a bare vsock connect took 0.8 ms. `idle=poll` in the guest changed nothing, so the cost is first-touch page faults on file-backed guest memory, which are expensive under nested EPT.
- The pool therefore prewarms: refills restore a VM running, wait for one agentd reply, then pause it. That moved warm acquires from p50 54 ms to p50 9.7 ms. The rest is mostly the per-request fault cost and should shrink on bare metal.
- On-demand restores stay under 100 ms one at a time and scale to 6 in parallel at 140 ms wall on 4 vCPUs.
