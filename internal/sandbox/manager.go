package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	agentdv1 "github.com/mondeep31/agent-comp/gen/go/agentcomp/agentd/v1"
	"github.com/mondeep31/agent-comp/internal/config"
	"github.com/mondeep31/agent-comp/internal/golden"
	"github.com/mondeep31/agent-comp/internal/guest"
	"github.com/mondeep31/agent-comp/internal/pool"
	"github.com/mondeep31/agent-comp/internal/storage"
	"github.com/mondeep31/agent-comp/internal/vmm"
)

const (
	readyTimeout     = 15 * time.Second
	clockSyncTimeout = 2 * time.Second
	stoppedRetention = 10 * time.Minute
	maxEnv           = 128
	maxMetadata      = 16
)

type Options struct {
	Source VMSource
	// Images maps each image name to its rootfs, shape defaults and display.
	Images       map[string]ImageSpec
	DefaultImage string
	// Defaults supplies TTL and idle timeout; shapes come from each ImageSpec.
	Defaults config.SandboxDefaults
	Limits   config.Limits
	// SnapshotDir holds user snapshots. It is wiped at startup: sandboxes and
	// snapshots do not survive an acd restart in v1.
	SnapshotDir string
	Logger      *slog.Logger
	// Events receives lifecycle events; nil disables them.
	Events func(Event)
	// Now is for tests.
	Now func() time.Time
}

// Event is a lifecycle transition, for logs and metrics.
type Event struct {
	Sandbox string
	Owner   string
	From    State
	To      State
	Reason  string
	Warm    bool
	Fork    bool
	Elapsed time.Duration
}

type sandbox struct {
	mu        sync.Mutex
	info      Info
	inst      *pool.Instance
	active    int
	stoppedAt time.Time
}

type userSnapshot struct {
	info SnapshotInfo
	snap *golden.Snapshot
	dir  string
}

// Manager owns every sandbox on this host.
//
// Lock order: Manager.mu before sandbox.mu before Manager.smu. Never take
// Manager.mu while holding a sandbox's mu.
type Manager struct {
	opts Options
	log  *slog.Logger

	mu        sync.Mutex
	sandboxes map[string]*sandbox
	pending   map[string]int
	// pendingMiB is the memory of sandboxes reserved but not yet recorded.
	pendingMiB int
	closed     bool

	smu       sync.Mutex
	snapshots map[string]*userSnapshot
}

func New(opts Options) (*Manager, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if _, ok := opts.Images[opts.DefaultImage]; !ok {
		return nil, fmt.Errorf("sandbox: default image %q is not configured", opts.DefaultImage)
	}
	if err := os.RemoveAll(opts.SnapshotDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.SnapshotDir, 0o700); err != nil {
		return nil, err
	}
	return &Manager{
		opts:      opts,
		log:       opts.Logger.With("component", "sandbox"),
		sandboxes: map[string]*sandbox{},
		snapshots: map[string]*userSnapshot{},
		pending:   map[string]int{},
	}, nil
}

// Limits exposes the configured limits to adapters.
func (m *Manager) Limits() config.Limits { return m.opts.Limits }

// Images lists the configured images, sorted by name.
func (m *Manager) Images() []ImageInfo {
	out := make([]ImageInfo, 0, len(m.opts.Images))
	for _, name := range slices.Sorted(maps.Keys(m.opts.Images)) {
		img := m.opts.Images[name]
		out = append(out, ImageInfo{
			Name: name, Default: name == m.opts.DefaultImage,
			VCPU: img.Shape.VCPU, MemoryMiB: img.Shape.MemoryMiB, DiskMiB: img.Shape.DiskMiB,
			Display: img.Display,
		})
	}
	return out
}

func (m *Manager) emit(e Event) {
	if m.opts.Events != nil {
		m.opts.Events(e)
	}
}

type plan struct {
	key   golden.Key
	snap  *golden.Snapshot
	ttl   time.Duration
	idle  time.Duration
	fork  string
	image string
}

func (m *Manager) resolve(req CreateRequest) (plan, error) {
	d, l := m.opts.Defaults, m.opts.Limits
	p := plan{
		ttl:  orDefault(req.TTL, d.TTL),
		idle: orDefault(req.IdleTimeout, d.IdleTimeout),
	}
	if p.ttl <= 0 || p.ttl > l.MaxTTL {
		return plan{}, invalid("ttl must be in (0, %v]", l.MaxTTL)
	}
	if p.idle <= 0 {
		return plan{}, invalid("idle_timeout must be > 0")
	}
	if len(req.Env) > maxEnv {
		return plan{}, invalid("at most %d env vars", maxEnv)
	}
	for k := range req.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return plan{}, invalid("invalid env var name %q", k)
		}
	}
	if len(req.Metadata) > maxMetadata {
		return plan{}, invalid("at most %d metadata entries", maxMetadata)
	}
	for k, v := range req.Metadata {
		if k == "" || len(k) > 64 || len(v) > 256 {
			return plan{}, invalid("metadata keys must be 1-64 bytes and values at most 256 bytes")
		}
	}

	if req.FromSnapshot != "" {
		m.smu.Lock()
		us := m.snapshots[req.FromSnapshot]
		m.smu.Unlock()
		if us == nil || us.info.Owner != req.Owner {
			return plan{}, fmt.Errorf("snapshot %s: %w", req.FromSnapshot, ErrNotFound)
		}
		p.snap, p.key, p.fork, p.image = us.snap, us.snap.Key, us.info.ID, us.snap.Key.Image
		return p, nil
	}

	p.image = req.Image
	if p.image == "" {
		p.image = m.opts.DefaultImage
	}
	img, ok := m.opts.Images[p.image]
	if !ok {
		return plan{}, invalid("unknown image %q", p.image)
	}
	shape := golden.Shape{
		VCPU:      orDefault(req.VCPU, img.Shape.VCPU),
		MemoryMiB: orDefault(req.MemoryMiB, img.Shape.MemoryMiB),
		DiskMiB:   orDefault(req.DiskMiB, img.Shape.DiskMiB),
	}
	if shape.VCPU < 1 || shape.VCPU > l.MaxVCPU {
		return plan{}, invalid("vcpu must be in [1, %d]", l.MaxVCPU)
	}
	if shape.MemoryMiB < 128 || shape.MemoryMiB > l.MaxMemoryMiB {
		return plan{}, invalid("memory_mib must be in [128, %d]", l.MaxMemoryMiB)
	}
	if shape.DiskMiB < 64 || shape.DiskMiB > l.MaxDiskMiB {
		return plan{}, invalid("disk_mib must be in [64, %d]", l.MaxDiskMiB)
	}
	if img.Display != nil && shape.MemoryMiB < config.MinDisplayMemoryMiB {
		return plan{}, invalid("image %q has a display and needs memory_mib >= %d", p.image, config.MinDisplayMemoryMiB)
	}
	p.key = golden.Key{Image: p.image, Shape: shape}
	return p, nil
}

func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

// reserve counts a sandbox against quotas before its VM exists.
func (m *Manager) reserve(owner string, memoryMiB int) (func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	total, mine, committed := 0, m.pending[owner], m.pendingMiB
	for _, n := range m.pending {
		total += n
	}
	for _, sb := range m.sandboxes {
		sb.mu.Lock()
		live := sb.info.State != StateStopped
		owned := sb.info.Owner == owner
		mem := sb.info.MemoryMiB
		sb.mu.Unlock()
		if !live {
			continue
		}
		total++
		committed += mem
		if owned {
			mine++
		}
	}
	l := m.opts.Limits
	if total >= l.MaxSandboxes {
		return nil, fmt.Errorf("%w: host is at its limit of %d sandboxes", ErrQuota, l.MaxSandboxes)
	}
	if mine >= l.MaxSandboxesPerKey {
		return nil, fmt.Errorf("%w: at most %d sandboxes per key", ErrQuota, l.MaxSandboxesPerKey)
	}
	if l.MaxCommittedMemoryMiB > 0 && committed+memoryMiB > l.MaxCommittedMemoryMiB {
		return nil, fmt.Errorf("%w: host memory budget of %d MiB is in use", ErrQuota, l.MaxCommittedMemoryMiB)
	}
	m.pending[owner]++
	m.pendingMiB += memoryMiB
	return func() {
		m.mu.Lock()
		m.pending[owner]--
		if m.pending[owner] == 0 {
			delete(m.pending, owner)
		}
		m.pendingMiB -= memoryMiB
		m.mu.Unlock()
	}, nil
}

// Create starts a sandbox and returns once agentd inside it is configured.
func (m *Manager) Create(ctx context.Context, req CreateRequest) (Info, error) {
	p, err := m.resolve(req)
	if err != nil {
		return Info{}, err
	}
	release, err := m.reserve(req.Owner, p.key.Shape.MemoryMiB)
	if err != nil {
		return Info{}, err
	}
	defer release()

	start := m.opts.Now()
	var inst *pool.Instance
	if p.snap != nil {
		inst, err = m.opts.Source.Restore(ctx, p.snap)
	} else {
		inst, err = m.opts.Source.Acquire(ctx, p.key)
	}
	if err != nil {
		return Info{}, fmt.Errorf("starting vm: %w", err)
	}
	if err := m.configure(ctx, inst, req.Env); err != nil {
		_ = inst.Destroy(context.WithoutCancel(ctx))
		return Info{}, err
	}

	now := m.opts.Now()
	sb := &sandbox{inst: inst, info: Info{
		ID:           inst.ID,
		Owner:        req.Owner,
		State:        StateRunning,
		Image:        p.image,
		Display:      m.opts.Images[p.image].Display,
		VCPU:         p.key.Shape.VCPU,
		MemoryMiB:    p.key.Shape.MemoryMiB,
		DiskMiB:      p.key.Shape.DiskMiB,
		CreatedAt:    now,
		ExpiresAt:    now.Add(p.ttl),
		LastActiveAt: now,
		IdleTimeout:  Duration(p.idle),
		FromSnapshot: p.fork,
		Warm:         inst.Warm,
		StartupMs:    now.Sub(start).Milliseconds(),
		Metadata:     maps.Clone(req.Metadata),
	}}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = inst.Destroy(context.WithoutCancel(ctx))
		return Info{}, ErrClosed
	}
	m.sandboxes[sb.info.ID] = sb
	m.mu.Unlock()

	go m.watch(context.WithoutCancel(ctx), sb)
	m.log.Info("sandbox created", "id", sb.info.ID, "owner", req.Owner, "key", p.key.String(),
		"warm", inst.Warm, "fork", p.fork, "startup_ms", sb.info.StartupMs)
	m.emit(Event{Sandbox: sb.info.ID, Owner: req.Owner, From: StateCreating, To: StateRunning, Warm: inst.Warm, Fork: p.fork != "", Elapsed: now.Sub(start)})
	return sb.info, nil
}

// configure gives a freshly restored VM its own identity.
func (m *Manager) configure(ctx context.Context, inst *pool.Instance, env map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := inst.Guest.WaitReady(ctx); err != nil {
		return err
	}
	entropy := make([]byte, 64)
	if _, err := rand.Read(entropy); err != nil {
		return err
	}
	err := inst.Guest.Configure(ctx, &agentdv1.Configure{
		Hostname:        inst.ID,
		WallClockUnixMs: m.opts.Now().UnixMilli(),
		Entropy:         entropy,
		Env:             env,
	})
	if err != nil {
		return fmt.Errorf("configuring guest: %w", err)
	}
	return nil
}

// watch marks a sandbox stopped if its VM dies on its own.
func (m *Manager) watch(ctx context.Context, sb *sandbox) {
	<-sb.inst.VM.Done()
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.info.State == StateRunning || sb.info.State == StatePaused {
		m.stopLocked(ctx, sb, "vm exited unexpectedly")
	}
}

// stopLocked destroys the VM and keeps the record for a while. sb.mu is held.
func (m *Manager) stopLocked(ctx context.Context, sb *sandbox, reason string) {
	from := sb.info.State
	sb.info.State = StateStopped
	sb.info.StopReason = reason
	sb.stoppedAt = m.opts.Now()
	if err := sb.inst.Destroy(ctx); err != nil {
		m.log.Error("destroying sandbox vm", "id", sb.info.ID, "err", err)
	}
	m.log.Info("sandbox stopped", "id", sb.info.ID, "reason", reason)
	m.emit(Event{Sandbox: sb.info.ID, Owner: sb.info.Owner, From: from, To: StateStopped, Reason: reason})
}

func (m *Manager) lookup(owner, id string) (*sandbox, error) {
	m.mu.Lock()
	sb := m.sandboxes[id]
	m.mu.Unlock()
	if sb == nil || sb.info.Owner != owner {
		return nil, fmt.Errorf("sandbox %s: %w", id, ErrNotFound)
	}
	return sb, nil
}

func (m *Manager) Get(owner, id string) (Info, error) {
	sb, err := m.lookup(owner, id)
	if err != nil {
		return Info{}, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.info, nil
}

// List returns the owner's sandboxes, newest first.
func (m *Manager) List(owner string) []Info {
	m.mu.Lock()
	var mine []*sandbox
	for _, sb := range m.sandboxes {
		if sb.info.Owner == owner {
			mine = append(mine, sb)
		}
	}
	m.mu.Unlock()
	out := make([]Info, 0, len(mine))
	for _, sb := range mine {
		sb.mu.Lock()
		out = append(out, sb.info)
		sb.mu.Unlock()
	}
	slices.SortFunc(out, func(a, b Info) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out
}

// Use returns the sandbox's guest client for one operation, resuming it if it
// was paused for idleness. Call done when the operation ends; while any
// operation is in flight the sandbox is never considered idle.
func (m *Manager) Use(ctx context.Context, owner, id string) (c *guest.Client, done func(), err error) {
	sb, err := m.lookup(owner, id)
	if err != nil {
		return nil, nil, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	switch sb.info.State {
	case StateRunning:
	case StatePaused:
		if err := m.resumeLocked(ctx, sb, "resumed on use"); err != nil {
			return nil, nil, err
		}
	case StateStopped:
		return nil, nil, fmt.Errorf("%w: sandbox is stopped (%s)", ErrConflict, sb.info.StopReason)
	case StateCreating, StateDeleted:
		return nil, nil, fmt.Errorf("%w: sandbox is %s", ErrConflict, sb.info.State)
	default:
		panic(fmt.Sprintf("unhandled state %q", sb.info.State))
	}
	sb.active++
	sb.info.LastActiveAt = m.opts.Now()
	return sb.inst.Guest, sync.OnceFunc(func() {
		sb.mu.Lock()
		sb.active--
		sb.info.LastActiveAt = m.opts.Now()
		sb.mu.Unlock()
	}), nil
}

func (m *Manager) resumeLocked(ctx context.Context, sb *sandbox, reason string) error {
	if err := sb.inst.VM.Resume(ctx); err != nil {
		m.stopLocked(context.WithoutCancel(ctx), sb, "resume failed: "+err.Error())
		return fmt.Errorf("%w: resume failed", ErrConflict)
	}
	sb.info.State = StateRunning
	sb.info.LastActiveAt = m.opts.Now()
	m.syncClock(ctx, sb)
	m.emit(Event{Sandbox: sb.info.ID, Owner: sb.info.Owner, From: StatePaused, To: StateRunning, Reason: reason})
	return nil
}

// syncClock resets the guest's wall clock after its vCPUs were paused, which
// otherwise leaves it behind by the pause. It also mixes in fresh entropy.
// It is best effort: a slow guest must not fail the operation that resumed it.
func (m *Manager) syncClock(ctx context.Context, sb *sandbox) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), clockSyncTimeout)
	defer cancel()
	entropy := make([]byte, 32)
	_, _ = rand.Read(entropy)
	err := sb.inst.Guest.Configure(ctx, &agentdv1.Configure{
		WallClockUnixMs: m.opts.Now().UnixMilli(),
		Entropy:         entropy,
	})
	if err != nil {
		m.log.Warn("clock sync after resume failed", "id", sb.info.ID, "err", err)
	}
}

func (m *Manager) pauseLocked(ctx context.Context, sb *sandbox, reason string) error {
	if err := sb.inst.VM.Pause(ctx); err != nil {
		m.stopLocked(context.WithoutCancel(ctx), sb, "pause failed: "+err.Error())
		return fmt.Errorf("%w: pause failed", ErrConflict)
	}
	sb.info.State = StatePaused
	m.emit(Event{Sandbox: sb.info.ID, Owner: sb.info.Owner, From: StateRunning, To: StatePaused, Reason: reason})
	return nil
}

// Pause freezes a sandbox's vCPUs. Any later operation resumes it.
func (m *Manager) Pause(ctx context.Context, owner, id string) (Info, error) {
	sb, err := m.lookup(owner, id)
	if err != nil {
		return Info{}, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	switch {
	case sb.info.State == StatePaused:
	case sb.info.State != StateRunning:
		return Info{}, fmt.Errorf("%w: sandbox is %s", ErrConflict, sb.info.State)
	case sb.active > 0:
		return Info{}, fmt.Errorf("%w: %d operations in flight", ErrConflict, sb.active)
	default:
		if err := m.pauseLocked(ctx, sb, "requested"); err != nil {
			return Info{}, err
		}
	}
	return sb.info, nil
}

func (m *Manager) Resume(ctx context.Context, owner, id string) (Info, error) {
	sb, err := m.lookup(owner, id)
	if err != nil {
		return Info{}, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	switch sb.info.State {
	case StateRunning:
	case StatePaused:
		if err := m.resumeLocked(ctx, sb, "requested"); err != nil {
			return Info{}, err
		}
	default:
		return Info{}, fmt.Errorf("%w: sandbox is %s", ErrConflict, sb.info.State)
	}
	return sb.info, nil
}

// Delete destroys the sandbox and forgets it.
func (m *Manager) Delete(ctx context.Context, owner, id string) error {
	sb, err := m.lookup(owner, id)
	if err != nil {
		return err
	}
	sb.mu.Lock()
	switch sb.info.State {
	case StateDeleted:
		sb.mu.Unlock()
		return fmt.Errorf("sandbox %s: %w", id, ErrNotFound)
	case StateStopped:
	case StateCreating, StateRunning, StatePaused:
		m.stopLocked(ctx, sb, "deleted")
	default:
		panic(fmt.Sprintf("unhandled state %q", sb.info.State))
	}
	sb.info.State = StateDeleted
	sb.mu.Unlock()
	m.mu.Lock()
	delete(m.sandboxes, id)
	m.mu.Unlock()
	return nil
}

// Snapshot captures a running or paused sandbox so new sandboxes can fork it.
func (m *Manager) Snapshot(ctx context.Context, owner, id string) (SnapshotInfo, error) {
	sb, err := m.lookup(owner, id)
	if err != nil {
		return SnapshotInfo{}, err
	}
	m.smu.Lock()
	mine := 0
	for _, s := range m.snapshots {
		if s.info.Owner == owner {
			mine++
		}
	}
	m.smu.Unlock()
	if mine >= m.opts.Limits.MaxSnapshotsPerKey {
		return SnapshotInfo{}, fmt.Errorf("%w: at most %d snapshots per key", ErrQuota, m.opts.Limits.MaxSnapshotsPerKey)
	}

	sb.mu.Lock()
	defer sb.mu.Unlock()
	wasRunning := sb.info.State == StateRunning
	if !wasRunning && sb.info.State != StatePaused {
		return SnapshotInfo{}, fmt.Errorf("%w: sandbox is %s", ErrConflict, sb.info.State)
	}
	vm := sb.inst.VM
	if wasRunning {
		if err := vm.Pause(ctx); err != nil {
			m.stopLocked(context.WithoutCancel(ctx), sb, "pause failed: "+err.Error())
			return SnapshotInfo{}, fmt.Errorf("%w: pause failed", ErrConflict)
		}
	}
	snapID := "snap-" + randomHex(8)
	dir := filepath.Join(m.opts.SnapshotDir, snapID)
	snap, err := capture(ctx, vm, dir)
	if wasRunning {
		if rerr := vm.Resume(ctx); rerr != nil {
			m.stopLocked(context.WithoutCancel(ctx), sb, "resume after snapshot failed: "+rerr.Error())
			err = errors.Join(err, rerr)
		} else {
			m.syncClock(ctx, sb)
		}
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return SnapshotInfo{}, fmt.Errorf("snapshot: %w", err)
	}
	snap.Key = sb.inst.Key
	snap.Rootfs = m.opts.Images[snap.Key.Image].Rootfs

	us := &userSnapshot{dir: dir, snap: snap, info: SnapshotInfo{
		ID: snapID, Owner: owner, Sandbox: id, Image: snap.Key.Image,
		VCPU: snap.Key.Shape.VCPU, MemoryMiB: snap.Key.Shape.MemoryMiB, DiskMiB: snap.Key.Shape.DiskMiB,
		CreatedAt: m.opts.Now(),
	}}
	m.smu.Lock()
	m.snapshots[snapID] = us
	m.smu.Unlock()
	m.log.Info("snapshot created", "snapshot", snapID, "sandbox", id, "owner", owner)
	return us.info, nil
}

func capture(ctx context.Context, vm vmm.VM, dir string) (*golden.Snapshot, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	files := vmm.SnapshotFiles{State: filepath.Join(dir, "vm.snap"), Memory: filepath.Join(dir, "vm.mem")}
	if err := vm.Snapshot(ctx, files); err != nil {
		return nil, err
	}
	overlay := filepath.Join(dir, "overlay.ext4")
	if err := storage.CloneFile(vm.OverlayPath(), overlay); err != nil {
		return nil, err
	}
	return &golden.Snapshot{Files: files, Overlay: overlay, BuiltAt: time.Now()}, nil
}

func (m *Manager) ListSnapshots(owner string) []SnapshotInfo {
	m.smu.Lock()
	defer m.smu.Unlock()
	var out []SnapshotInfo
	for _, s := range m.snapshots {
		if s.info.Owner == owner {
			out = append(out, s.info)
		}
	}
	slices.SortFunc(out, func(a, b SnapshotInfo) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out
}

// DeleteSnapshot removes a snapshot. Sandboxes already forked from it keep
// running: their VMs hold hard links to the snapshot files.
func (m *Manager) DeleteSnapshot(owner, id string) error {
	m.smu.Lock()
	s := m.snapshots[id]
	if s == nil || s.info.Owner != owner {
		m.smu.Unlock()
		return fmt.Errorf("snapshot %s: %w", id, ErrNotFound)
	}
	delete(m.snapshots, id)
	m.smu.Unlock()
	return os.RemoveAll(s.dir)
}

// Reap enforces TTLs and idle timeouts and forgets long-stopped sandboxes.
func (m *Manager) Reap(ctx context.Context) {
	m.mu.Lock()
	all := slices.Collect(maps.Values(m.sandboxes))
	m.mu.Unlock()
	now := m.opts.Now()
	var forget []string
	for _, sb := range all {
		sb.mu.Lock()
		switch sb.info.State {
		case StateRunning, StatePaused:
			if !now.Before(sb.info.ExpiresAt) {
				m.stopLocked(ctx, sb, "ttl expired")
			} else if sb.info.State == StateRunning && sb.active == 0 &&
				now.Sub(sb.info.LastActiveAt) >= time.Duration(sb.info.IdleTimeout) {
				_ = m.pauseLocked(ctx, sb, "idle")
			}
		case StateStopped:
			if now.Sub(sb.stoppedAt) >= stoppedRetention {
				forget = append(forget, sb.info.ID)
			}
		case StateCreating, StateDeleted:
		default:
			panic(fmt.Sprintf("unhandled state %q", sb.info.State))
		}
		sb.mu.Unlock()
	}
	if len(forget) > 0 {
		m.mu.Lock()
		for _, id := range forget {
			delete(m.sandboxes, id)
		}
		m.mu.Unlock()
	}
}

// Run reaps on an interval until ctx ends.
func (m *Manager) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Reap(ctx)
		}
	}
}

// Stats counts sandboxes by state.
func (m *Manager) Stats() map[State]int {
	m.mu.Lock()
	all := slices.Collect(maps.Values(m.sandboxes))
	m.mu.Unlock()
	out := map[State]int{}
	for _, sb := range all {
		sb.mu.Lock()
		out[sb.info.State]++
		sb.mu.Unlock()
	}
	return out
}

// Close destroys every sandbox and snapshot.
func (m *Manager) Close(ctx context.Context) {
	m.mu.Lock()
	m.closed = true
	all := slices.Collect(maps.Values(m.sandboxes))
	m.sandboxes = map[string]*sandbox{}
	m.mu.Unlock()
	m.smu.Lock()
	m.snapshots = map[string]*userSnapshot{}
	m.smu.Unlock()
	var wg sync.WaitGroup
	for _, sb := range all {
		wg.Go(func() {
			sb.mu.Lock()
			defer sb.mu.Unlock()
			if sb.info.State != StateStopped {
				m.stopLocked(ctx, sb, "shutdown")
			}
		})
	}
	wg.Wait()
	_ = os.RemoveAll(m.opts.SnapshotDir)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
