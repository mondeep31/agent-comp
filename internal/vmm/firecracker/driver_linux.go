package firecracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mondeep31/agent-comp/internal/storage"
	"github.com/mondeep31/agent-comp/internal/vmm"
)

// Guest-visible paths inside every chroot. They are identical for all VMs so a
// snapshot taken in one jail restores in any other.
const (
	chrootKernel   = "/vmlinux"
	chrootRootfs   = "/rootfs.ext4"
	chrootOverlay  = "/overlay.ext4"
	chrootSnapshot = "/vm.snap"
	chrootMemory   = "/vm.mem"
	chrootNewState = "/snapshot.state"
	chrootNewMem   = "/snapshot.mem"
	chrootAPISock  = "/api.sock"
	chrootVsock    = "/v.sock"
	guestCID       = 3
	cgroupRoot     = "/sys/fs/cgroup"
	// memoryOverheadMiB covers the VMM process itself on top of guest RAM.
	memoryOverheadMiB = 96
)

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9-]{1,64}$`)

type apiStep struct {
	path string
	body any
}

// Options configures the Firecracker driver.
type Options struct {
	FirecrackerBin string
	JailerBin      string
	ChrootBase     string
	UIDBase        int
	// CgroupParent is relative to /sys/fs/cgroup.
	CgroupParent string
	// MaxVMs bounds concurrently running VMs (one uid per VM).
	MaxVMs       int
	ConsoleBytes int
	Logger       *slog.Logger
}

// Driver runs Firecracker VMs under jailer.
type Driver struct {
	opts  Options
	log   *slog.Logger
	mu    sync.Mutex
	slots []bool
}

var _ vmm.Driver = (*Driver)(nil)

// New validates the host and prepares the parent cgroup. It must run as root.
func New(opts Options) (*Driver, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.ConsoleBytes <= 0 {
		opts.ConsoleBytes = 64 << 10
	}
	if opts.MaxVMs <= 0 {
		return nil, errors.New("firecracker: MaxVMs must be > 0")
	}
	for _, bin := range []string{opts.FirecrackerBin, opts.JailerBin} {
		if _, err := os.Stat(bin); err != nil {
			return nil, fmt.Errorf("firecracker: %w", err)
		}
	}
	if os.Geteuid() != 0 {
		return nil, errors.New("firecracker: jailer requires root")
	}
	if err := ensureCgroupParent(opts.CgroupParent); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.ChrootBase, 0o750); err != nil {
		return nil, err
	}
	d := &Driver{opts: opts, log: opts.Logger, slots: make([]bool, opts.MaxVMs)}
	d.cleanupStale()
	return d, nil
}

// cleanupStale removes jails and cgroups left by a previous process. Their
// VMMs died with it (Pdeathsig), so nothing in them is live.
func (d *Driver) cleanupStale() {
	jails := filepath.Join(d.opts.ChrootBase, filepath.Base(d.opts.FirecrackerBin))
	entries, _ := os.ReadDir(jails)
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(jails, e.Name())); err == nil {
			d.log.Info("removed stale jail", "id", e.Name())
		}
	}
	cgroups, _ := os.ReadDir(filepath.Join(cgroupRoot, d.opts.CgroupParent))
	for _, e := range cgroups {
		if e.IsDir() {
			_ = os.Remove(filepath.Join(cgroupRoot, d.opts.CgroupParent, e.Name()))
		}
	}
}

func ensureCgroupParent(parent string) error {
	controllers := []byte("+cpu +memory +pids")
	if err := os.WriteFile(filepath.Join(cgroupRoot, "cgroup.subtree_control"), controllers, 0o600); err != nil {
		return fmt.Errorf("enabling root cgroup controllers: %w", err)
	}
	dir := filepath.Join(cgroupRoot, parent)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating cgroup %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), controllers, 0o600); err != nil {
		return fmt.Errorf("enabling controllers in %s: %w", dir, err)
	}
	return nil
}

func (d *Driver) allocSlot() (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, used := range d.slots {
		if !used {
			d.slots[i] = true
			return i, nil
		}
	}
	return 0, fmt.Errorf("firecracker: all %d VM slots in use", len(d.slots))
}

func (d *Driver) freeSlot(i int) {
	d.mu.Lock()
	d.slots[i] = false
	d.mu.Unlock()
}

func (d *Driver) jailDir(id string) string {
	return filepath.Join(d.opts.ChrootBase, filepath.Base(d.opts.FirecrackerBin), id)
}

// Boot cold boots a VM from a kernel.
func (d *Driver) Boot(ctx context.Context, spec vmm.BootSpec) (vmm.VM, error) {
	links := map[string]string{
		chrootKernel: spec.Kernel,
		chrootRootfs: spec.Disks.RootfsBase,
	}
	m, err := d.launch(ctx, spec.ID, spec.Machine, spec.Network, links, spec.Disks.Overlay)
	if err != nil {
		return nil, err
	}
	steps := []apiStep{
		{path: "/boot-source", body: bootSource{KernelImagePath: chrootKernel, BootArgs: spec.BootArgs}},
		{path: "/drives/rootfs", body: drive{
			DriveID:      "rootfs",
			PathOnHost:   chrootRootfs,
			IsRootDevice: true,
			IsReadOnly:   true,
		}},
		{path: "/drives/overlay", body: drive{
			DriveID:     "overlay",
			PathOnHost:  chrootOverlay,
			RateLimiter: bandwidthLimiter(int64(spec.Disks.BandwidthMiBps) << 20),
		}},
		{path: "/machine-config", body: machineConfig{VCPUCount: spec.Machine.VCPU, MemSizeMiB: spec.Machine.MemoryMiB}},
		{path: "/vsock", body: vsockDevice{GuestCID: guestCID, UDSPath: chrootVsock}},
		{path: "/entropy", body: struct{}{}},
	}
	if n := spec.Network; n != nil {
		limit := bandwidthLimiter(int64(n.BandwidthMbps) * 1_000_000 / 8)
		steps = append(steps, apiStep{path: "/network-interfaces/eth0", body: networkInterface{
			IfaceID:       "eth0",
			HostDevName:   n.TapName,
			GuestMAC:      n.GuestMAC,
			RxRateLimiter: limit,
			TxRateLimiter: limit,
		}})
	}
	steps = append(steps, apiStep{path: "/actions", body: action{ActionType: "InstanceStart"}})

	for _, s := range steps {
		if err := m.api.put(ctx, s.path, s.body); err != nil {
			m.fail(ctx, err)
			return nil, err
		}
	}
	return m, nil
}

// Restore starts a VM from a full snapshot.
func (d *Driver) Restore(ctx context.Context, spec vmm.RestoreSpec) (vmm.VM, error) {
	links := map[string]string{
		chrootSnapshot: spec.Snapshot.State,
		chrootMemory:   spec.Snapshot.Memory,
		chrootRootfs:   spec.Disks.RootfsBase,
	}
	m, err := d.launch(ctx, spec.ID, spec.Machine, spec.Network, links, spec.Disks.Overlay)
	if err != nil {
		return nil, err
	}
	load := snapshotLoad{
		SnapshotPath: chrootSnapshot,
		MemBackend:   memBackend{BackendType: "File", BackendPath: chrootMemory},
		ResumeVM:     spec.Resume,
	}
	if err := m.api.put(ctx, "/snapshot/load", load); err != nil {
		m.fail(ctx, err)
		return nil, err
	}
	return m, nil
}

// launch prepares the chroot, starts jailer and waits for the API socket.
func (d *Driver) launch(
	ctx context.Context,
	id string,
	machine vmm.Machine,
	network *vmm.Network,
	links map[string]string,
	overlay string,
) (*jailedVM, error) {
	if !idPattern.MatchString(id) {
		return nil, fmt.Errorf("firecracker: invalid vm id %q", id)
	}
	if machine.VCPU < 1 || machine.MemoryMiB < 128 {
		return nil, fmt.Errorf("firecracker: invalid machine %+v", machine)
	}
	slot, err := d.allocSlot()
	if err != nil {
		return nil, err
	}
	uid := d.opts.UIDBase + slot
	jailDir := d.jailDir(id)
	root := filepath.Join(jailDir, "root")

	m := &jailedVM{
		id:      id,
		driver:  d,
		slot:    slot,
		jailDir: jailDir,
		root:    root,
		cgroup:  filepath.Join(cgroupRoot, d.opts.CgroupParent, id),
		console: vmm.NewRing(d.opts.ConsoleBytes),
		done:    make(chan struct{}),
	}

	if err := m.prepareRoot(uid, links, overlay); err != nil {
		m.cleanupAfterSetupError(err)
		return nil, err
	}

	args := []string{
		"--id", id,
		"--exec-file", d.opts.FirecrackerBin,
		"--uid", strconv.Itoa(uid),
		"--gid", strconv.Itoa(uid),
		"--chroot-base-dir", d.opts.ChrootBase,
		"--cgroup-version", "2",
		"--parent-cgroup", d.opts.CgroupParent,
		"--cgroup", fmt.Sprintf("cpu.max=%d 100000", machine.VCPU*100000),
		"--cgroup", fmt.Sprintf("memory.max=%d", int64(machine.MemoryMiB+memoryOverheadMiB)<<20),
		"--cgroup", "pids.max=64",
		"--resource-limit", "no-file=1024",
	}
	if network != nil {
		args = append(args, "--netns", network.NetNSPath)
	}
	args = append(args, "--", "--api-sock", chrootAPISock, "--level", "Warning")

	// The VMM must outlive the request context, so it is not tied to ctx; Stop owns its lifetime.
	cmd := exec.Command(d.opts.JailerBin, args...) //nolint:noctx,gosec // binary from operator config, args built here
	cmd.Stdout = m.console
	cmd.Stderr = m.console
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
	if err := cmd.Start(); err != nil {
		m.cleanupAfterSetupError(err)
		return nil, fmt.Errorf("starting jailer: %w", err)
	}
	m.cmd = cmd
	go func() {
		_ = cmd.Wait()
		close(m.done)
	}()

	sock := filepath.Join(root, chrootAPISock)
	if err := waitForSocket(ctx, sock, m.done); err != nil {
		m.fail(ctx, err)
		return nil, fmt.Errorf("%w; console: %s", err, tail(m.console.Bytes(), 512))
	}
	m.api = newAPIClient(sock)
	return m, nil
}

func (m *jailedVM) prepareRoot(uid int, links map[string]string, overlay string) error {
	if err := os.RemoveAll(m.jailDir); err != nil {
		return err
	}
	if err := os.MkdirAll(m.root, 0o750); err != nil {
		return err
	}
	if err := os.Chown(m.root, uid, uid); err != nil {
		return err
	}
	for name, src := range links {
		if err := storage.LinkOrCopy(src, filepath.Join(m.root, name)); err != nil {
			return fmt.Errorf("linking %s into chroot: %w", src, err)
		}
	}
	dst := filepath.Join(m.root, chrootOverlay)
	if err := os.Rename(overlay, dst); err != nil {
		if err := storage.CloneFile(overlay, dst); err != nil {
			return fmt.Errorf("placing overlay: %w", err)
		}
		_ = os.Remove(overlay)
	}
	return os.Chown(dst, uid, uid)
}

func waitForSocket(ctx context.Context, path string, done <-chan struct{}) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-done:
			return errors.New("firecracker exited before its API socket appeared")
		case <-ctx.Done():
			return fmt.Errorf("waiting for API socket: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// jailedVM is a running Firecracker VM inside its jailer chroot.
type jailedVM struct {
	id      string
	driver  *Driver
	slot    int
	jailDir string
	root    string
	cgroup  string
	cmd     *exec.Cmd
	api     *apiClient
	console *vmm.Ring
	done    chan struct{}

	stopOnce sync.Once
	stopErr  error
}

var _ vmm.VM = (*jailedVM)(nil)

func (m *jailedVM) ID() string            { return m.id }
func (m *jailedVM) Console() []byte       { return m.console.Bytes() }
func (m *jailedVM) Done() <-chan struct{} { return m.done }

func (m *jailedVM) running() error {
	select {
	case <-m.done:
		return vmm.ErrNotRunning
	default:
		return nil
	}
}

func (m *jailedVM) Pause(ctx context.Context) error {
	if err := m.running(); err != nil {
		return err
	}
	return m.api.patch(ctx, "/vm", vmState{State: "Paused"})
}

func (m *jailedVM) Resume(ctx context.Context) error {
	if err := m.running(); err != nil {
		return err
	}
	return m.api.patch(ctx, "/vm", vmState{State: "Resumed"})
}

func (m *jailedVM) Snapshot(ctx context.Context, dst vmm.SnapshotFiles) error {
	if err := m.running(); err != nil {
		return err
	}
	for _, name := range []string{chrootNewState, chrootNewMem} {
		_ = os.Remove(filepath.Join(m.root, name))
	}
	req := snapshotCreate{SnapshotType: "Full", SnapshotPath: chrootNewState, MemFilePath: chrootNewMem}
	if err := m.api.put(ctx, "/snapshot/create", req); err != nil {
		return err
	}
	moves := map[string]string{chrootNewState: dst.State, chrootNewMem: dst.Memory}
	for src, to := range moves {
		from := filepath.Join(m.root, src)
		if err := os.Rename(from, to); err != nil {
			if err := storage.CloneFile(from, to); err != nil {
				return fmt.Errorf("moving snapshot file: %w", err)
			}
			_ = os.Remove(from)
		}
		if err := os.Chown(to, 0, 0); err != nil {
			return err
		}
		// Snapshots are shared read-only by every jailed uid that restores them.
		if err := os.Chmod(to, 0o444); err != nil { //nolint:gosec // see above
			return err
		}
	}
	return nil
}

// OverlayPath is the host path of the VM's writable disk while it runs.
func (m *jailedVM) OverlayPath() string {
	return filepath.Join(m.root, chrootOverlay)
}

func (m *jailedVM) DialVsock(ctx context.Context, port uint32) (net.Conn, error) {
	if err := m.running(); err != nil {
		return nil, err
	}
	return dialVsock(ctx, filepath.Join(m.root, chrootVsock), port)
}

func (m *jailedVM) Stop(ctx context.Context) error {
	m.stopOnce.Do(func() { m.stopErr = m.stop(ctx) })
	return m.stopErr
}

// fail tears the VM down after a setup error. Teardown must finish even if the
// caller's context is already cancelled.
func (m *jailedVM) fail(ctx context.Context, err error) {
	m.driver.log.Warn("vm setup failed", "vm", m.id, "err", err, "console", tail(m.console.Bytes(), 512))
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if stopErr := m.Stop(ctx); stopErr != nil {
		m.driver.log.Error("vm teardown failed", "vm", m.id, "err", stopErr)
	}
}

func (m *jailedVM) cleanupAfterSetupError(cause error) {
	if err := m.cleanup(); err != nil {
		m.driver.log.Error("vm cleanup failed", "vm", m.id, "cause", cause, "err", err)
	}
}

func (m *jailedVM) stop(ctx context.Context) error {
	if m.api != nil {
		m.api.close()
	}
	if m.cmd != nil && m.cmd.Process != nil {
		_ = syscall.Kill(-m.cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-m.done:
		case <-ctx.Done():
			return fmt.Errorf("waiting for vm %s to exit: %w", m.id, ctx.Err())
		}
	}
	return m.cleanup()
}

func (m *jailedVM) cleanup() error {
	defer m.driver.freeSlot(m.slot)
	var errs []error
	if err := os.RemoveAll(m.jailDir); err != nil {
		errs = append(errs, err)
	}
	// The cgroup can stay busy for a moment after the last task exits.
	for i := 0; i < 50; i++ {
		err := os.Remove(m.cgroup)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			break
		}
		if i == 49 {
			errs = append(errs, fmt.Errorf("removing cgroup %s: %w", m.cgroup, err))
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.Join(errs...)
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.TrimSpace(string(b))
}
