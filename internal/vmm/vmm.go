// Package vmm defines the hypervisor-agnostic interface the sandbox manager uses
// to run microVMs. Firecracker is the only implementation today; Kata or Cloud
// Hypervisor can be added behind the same interface.
package vmm

import (
	"context"
	"errors"
	"net"
)

// ErrNotRunning is returned for operations on a VM whose process has exited.
var ErrNotRunning = errors.New("vm is not running")

// Machine is the CPU and memory shape of a VM. It is fixed for the VM's lifetime
// and baked into any snapshot taken from it.
type Machine struct {
	VCPU      int
	MemoryMiB int
}

// Network attaches the VM to a tap device inside a network namespace.
type Network struct {
	// NetNSPath is the namespace the VMM is placed in, e.g. /var/run/netns/ac-1234.
	NetNSPath string
	// TapName is the tap device inside NetNSPath.
	TapName  string
	GuestMAC string
	// BandwidthMbps limits rx and tx each; 0 disables.
	BandwidthMbps int
}

// NetworkProvider hands out per-VM network attachments.
//
// Snapshot clones keep the guest's network configuration from the golden VM,
// so every attachment must look identical from inside the guest (same tap
// name, guest address and gateway) and differ only on the host side.
type NetworkProvider interface {
	Attach(ctx context.Context, id string) (*Network, error)
	// Detach releases everything Attach created. It is idempotent.
	Detach(ctx context.Context, id string) error
	// KernelArgs configures the guest side at first boot, e.g. "ac.ip=... ac.gw=...".
	KernelArgs() string
}

// Disks are host paths. The driver links them into the VM's chroot under fixed
// names, so every clone sees the same guest-side paths.
type Disks struct {
	// RootfsBase is shared read-only between all VMs of an image.
	RootfsBase string
	// Overlay is the VM's private writable disk.
	Overlay string
	// BandwidthMiBps limits the overlay disk; 0 disables.
	BandwidthMiBps int
}

// BootSpec describes a VM that is cold booted from a kernel.
type BootSpec struct {
	ID       string
	Kernel   string
	BootArgs string
	Machine  Machine
	Disks    Disks
	// Network is optional.
	Network *Network
}

// SnapshotFiles locates a full snapshot on the host.
type SnapshotFiles struct {
	State  string
	Memory string
}

// RestoreSpec describes a VM restored from a snapshot. Machine shape, kernel and
// boot args come from the snapshot.
type RestoreSpec struct {
	ID       string
	Snapshot SnapshotFiles
	Machine  Machine
	Disks    Disks
	Network  *Network
	// Resume starts the vCPUs right after loading; otherwise the VM stays paused.
	Resume bool
}

// Driver creates VMs.
type Driver interface {
	// Boot starts a VM from a kernel and returns once the guest is running.
	Boot(ctx context.Context, spec BootSpec) (VM, error)
	// Restore starts a VM from a snapshot.
	Restore(ctx context.Context, spec RestoreSpec) (VM, error)
}

// VM is one running (or paused) microVM.
type VM interface {
	ID() string
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	// Snapshot writes a full snapshot of a paused VM to dst.
	Snapshot(ctx context.Context, dst SnapshotFiles) error
	// DialVsock opens a stream to a guest vsock port.
	DialVsock(ctx context.Context, port uint32) (net.Conn, error)
	// OverlayPath is the host path of the VM's private writable disk.
	OverlayPath() string
	// Console returns the most recent serial console and VMM output.
	Console() []byte
	// Done is closed when the VMM process exits.
	Done() <-chan struct{}
	// Stop kills the VMM and releases every host resource the driver created.
	Stop(ctx context.Context) error
}
