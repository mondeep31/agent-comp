// Package golden builds and caches golden snapshots: one booted, idle VM per
// (image, shape), frozen to disk so sandboxes start by restoring it.
//
// A golden snapshot is three files: VM state, guest memory and the overlay
// disk as it was at pause time. Clones share the first two read-only and get a
// private copy (reflink where supported) of the overlay.
package golden

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agentdv1 "github.com/mondeep31/agent-comp/gen/go/agentcomp/agentd/v1"
	"github.com/mondeep31/agent-comp/internal/guest"
	"github.com/mondeep31/agent-comp/internal/storage"
	"github.com/mondeep31/agent-comp/internal/vmm"
)

// DefaultBootArgs boots straight into agentd as init. Pooled VMs sit paused
// for minutes, and on resume the guest sees the clock jump and reports
// lockups and stalls; printing those over the emulated UART costs ~150 ms,
// so the watchdogs are off and the console only carries errors.
const DefaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off quiet loglevel=3 " +
	"nowatchdog nosoftlockup workqueue.watchdog_thresh=0 rcupdate.rcu_cpu_stall_suppress=1 " +
	"init=/usr/local/bin/agentd"

// Shape is the part of a sandbox's size that is baked into its snapshot.
type Shape struct {
	VCPU      int
	MemoryMiB int
	DiskMiB   int
}

func (s Shape) String() string {
	return fmt.Sprintf("%dc-%dm-%dd", s.VCPU, s.MemoryMiB, s.DiskMiB)
}

func (s Shape) Machine() vmm.Machine { return vmm.Machine{VCPU: s.VCPU, MemoryMiB: s.MemoryMiB} }

// Key identifies one golden snapshot.
type Key struct {
	Image string
	Shape Shape
}

func (k Key) String() string { return k.Image + "/" + k.Shape.String() }

// Image is a bootable kernel plus read-only root filesystem.
type Image struct {
	Name     string
	Kernel   string
	Rootfs   string
	BootArgs string
}

// Snapshot is a ready golden snapshot.
type Snapshot struct {
	Key      Key
	Files    vmm.SnapshotFiles
	Overlay  string
	Rootfs   string
	BuiltAt  time.Time
	BuildDur time.Duration
}

type Options struct {
	Driver vmm.Driver
	// Network is nil when sandboxes have no network.
	Network vmm.NetworkProvider
	// Dir holds one subdirectory per key.
	Dir    string
	Images []Image
	// FirecrackerBin is fingerprinted: snapshots are only valid for the VMM that wrote them.
	FirecrackerBin string
	ReadyTimeout   time.Duration
	Logger         *slog.Logger
}

// Manager builds golden snapshots on first use and reuses valid ones from disk.
type Manager struct {
	opts   Options
	log    *slog.Logger
	images map[string]Image

	mu       sync.Mutex
	ready    map[Key]*Snapshot
	building map[Key]*build
}

type build struct {
	done chan struct{}
	snap *Snapshot
	err  error
}

// ErrUnknownImage is returned for keys naming an unconfigured image.
var ErrUnknownImage = errors.New("golden: unknown image")

func New(opts Options) (*Manager, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = time.Minute
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, err
	}
	images := make(map[string]Image, len(opts.Images))
	for _, img := range opts.Images {
		if img.BootArgs == "" {
			img.BootArgs = DefaultBootArgs
		}
		images[img.Name] = img
	}
	return &Manager{
		opts:     opts,
		log:      opts.Logger.With("component", "golden"),
		images:   images,
		ready:    map[Key]*Snapshot{},
		building: map[Key]*build{},
	}, nil
}

// Get returns the golden snapshot for key, building it if needed. Concurrent
// callers for the same key share one build.
func (m *Manager) Get(ctx context.Context, key Key) (*Snapshot, error) {
	img, ok := m.images[key.Image]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownImage, key.Image)
	}
	m.mu.Lock()
	if s := m.ready[key]; s != nil {
		m.mu.Unlock()
		return s, nil
	}
	b := m.building[key]
	if b == nil {
		b = &build{done: make(chan struct{})}
		m.building[key] = b
		// The build outlives any single caller; it is bounded by ReadyTimeout.
		go m.run(context.WithoutCancel(ctx), key, img, b)
	}
	m.mu.Unlock()

	select {
	case <-b.done:
		return b.snap, b.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Manager) run(ctx context.Context, key Key, img Image, b *build) {
	b.snap, b.err = m.loadOrBuild(ctx, key, img)
	m.mu.Lock()
	delete(m.building, key)
	if b.err == nil {
		m.ready[key] = b.snap
	}
	m.mu.Unlock()
	close(b.done)
}

// Invalidate forgets a cached snapshot so the next Get rebuilds it.
func (m *Manager) Invalidate(key Key) {
	m.mu.Lock()
	delete(m.ready, key)
	m.mu.Unlock()
	_ = os.RemoveAll(m.keyDir(key))
}

func (m *Manager) keyDir(key Key) string {
	return filepath.Join(m.opts.Dir, key.Image, key.Shape.String())
}

type meta struct {
	Fingerprint string    `json:"fingerprint"`
	BuiltAt     time.Time `json:"built_at"`
	BuildMillis int64     `json:"build_ms"`
}

func paths(dir string) (vmm.SnapshotFiles, string, string) {
	return vmm.SnapshotFiles{
		State:  filepath.Join(dir, "vm.snap"),
		Memory: filepath.Join(dir, "vm.mem"),
	}, filepath.Join(dir, "overlay.ext4"), filepath.Join(dir, "meta.json")
}

// fingerprint changes whenever anything baked into the snapshot changes.
func (m *Manager) fingerprint(key Key, img Image) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "v1\n%s\n%s\n", key, img.BootArgs)
	if m.opts.Network != nil {
		fmt.Fprintf(h, "net %s\n", m.opts.Network.KernelArgs())
	}
	for _, p := range []string{img.Kernel, img.Rootfs, m.opts.FirecrackerBin} {
		if p == "" {
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s %d %d\n", p, st.Size(), st.ModTime().UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (m *Manager) loadOrBuild(ctx context.Context, key Key, img Image) (*Snapshot, error) {
	fp, err := m.fingerprint(key, img)
	if err != nil {
		return nil, fmt.Errorf("golden %s: %w", key, err)
	}
	dir := m.keyDir(key)
	files, overlay, metaPath := paths(dir)
	if data, err := os.ReadFile(metaPath); err == nil {
		var md meta
		if json.Unmarshal(data, &md) == nil && md.Fingerprint == fp {
			m.log.Info("reusing golden snapshot", "key", key.String(), "built_at", md.BuiltAt)
			return &Snapshot{
				Key: key, Files: files, Overlay: overlay, Rootfs: img.Rootfs,
				BuiltAt: md.BuiltAt, BuildDur: time.Duration(md.BuildMillis) * time.Millisecond,
			}, nil
		}
		m.log.Info("golden snapshot is stale; rebuilding", "key", key.String())
	}

	start := time.Now()
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".build-"+key.Shape.String()+"-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	if err := m.build(ctx, key, img, tmp); err != nil {
		return nil, fmt.Errorf("golden %s: %w", key, err)
	}
	md := meta{Fingerprint: fp, BuiltAt: time.Now().UTC(), BuildMillis: time.Since(start).Milliseconds()}
	data, err := json.Marshal(md)
	if err != nil {
		return nil, err
	}
	_, _, tmpMeta := paths(tmp)
	if err := os.WriteFile(tmpMeta, data, 0o600); err != nil {
		return nil, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return nil, err
	}
	m.log.Info("built golden snapshot", "key", key.String(), "duration", time.Since(start))
	return &Snapshot{
		Key: key, Files: files, Overlay: overlay, Rootfs: img.Rootfs,
		BuiltAt: md.BuiltAt, BuildDur: time.Since(start),
	}, nil
}

// build cold boots a VM, waits for agentd, quiesces it and snapshots it into dir.
func (m *Manager) build(ctx context.Context, key Key, img Image, dir string) (err error) {
	ctx, cancel := context.WithTimeout(ctx, m.opts.ReadyTimeout)
	defer cancel()

	files, overlay, _ := paths(dir)
	bootOverlay := filepath.Join(dir, "boot-overlay.ext4")
	if err := storage.CreateExt4(ctx, bootOverlay, key.Shape.DiskMiB); err != nil {
		return err
	}
	id := "golden-" + randomHex(6)
	args := img.BootArgs
	var netw *vmm.Network
	if m.opts.Network != nil {
		args = strings.TrimSpace(args + " " + m.opts.Network.KernelArgs())
		if netw, err = m.opts.Network.Attach(ctx, id); err != nil {
			return fmt.Errorf("attaching network: %w", err)
		}
		defer func() { err = errors.Join(err, m.opts.Network.Detach(context.WithoutCancel(ctx), id)) }()
	}

	vm, err := m.opts.Driver.Boot(ctx, vmm.BootSpec{
		ID: id, Kernel: img.Kernel, BootArgs: args, Machine: key.Shape.Machine(),
		Disks: vmm.Disks{RootfsBase: img.Rootfs, Overlay: bootOverlay}, Network: netw,
	})
	if err != nil {
		return fmt.Errorf("booting: %w", err)
	}
	defer func() {
		if stopErr := vm.Stop(context.WithoutCancel(ctx)); stopErr != nil && !errors.Is(stopErr, vmm.ErrNotRunning) {
			err = errors.Join(err, stopErr)
		}
	}()

	c := guest.ForVM(vm)
	if err := c.WaitReady(ctx); err != nil {
		return fmt.Errorf("%w; console:\n%s", err, tail(vm.Console(), 4096))
	}
	// Flush the page cache so the overlay copy taken at pause is consistent.
	exit, err := c.Run(ctx, &agentdv1.ExecStart{Argv: []string{"sync"}}, nil, nil, nil)
	if err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	if exit.GetExitCode() != 0 {
		return fmt.Errorf("sync exited %d", exit.GetExitCode())
	}
	if err := vm.Pause(ctx); err != nil {
		return err
	}
	if err := vm.Snapshot(ctx, files); err != nil {
		return err
	}
	return storage.CloneFile(vm.OverlayPath(), overlay)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
