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
