// Package pool keeps restored, paused VMs ready so acquiring a sandbox costs a
// resume instead of a restore, and restores on demand when the pool is empty.
package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/mondeep31/agent-comp/internal/golden"
	"github.com/mondeep31/agent-comp/internal/guest"
	"github.com/mondeep31/agent-comp/internal/storage"
	"github.com/mondeep31/agent-comp/internal/vmm"
)

// Snapshots is satisfied by *golden.Manager.
type Snapshots interface {
	Get(ctx context.Context, key golden.Key) (*golden.Snapshot, error)
}

type Options struct {
	Driver    vmm.Driver
	Snapshots Snapshots
	// Network is nil when sandboxes have no network.
	Network vmm.NetworkProvider
	// Dir is scratch space for overlay copies before the driver adopts them.
	Dir string
	// Warm is the number of paused VMs to keep per key.
	Warm               map[golden.Key]int
	RefillConcurrency  int
	DiskBandwidthMiBps int
	Logger             *slog.Logger
}

// Instance is a running VM handed out by the pool. The caller owns it and must
// call Destroy.
type Instance struct {
	ID    string
	Key   golden.Key
	VM    vmm.VM
	Guest *guest.Client
	// Warm is true when the VM came from the pool rather than an on-demand restore.
	Warm bool

	destroy func(context.Context) error
	once    sync.Once
	err     error
}

// NewInstance wraps a VM started outside a pool, e.g. by tests or another source.
func NewInstance(id string, key golden.Key, vm vmm.VM, destroy func(context.Context) error) *Instance {
	return &Instance{ID: id, Key: key, VM: vm, Guest: guest.ForVM(vm), destroy: destroy}
}

// Destroy stops the VM and releases its network. Safe to call more than once.
func (i *Instance) Destroy(ctx context.Context) error {
	i.once.Do(func() { i.err = i.destroy(ctx) })
	return i.err
}

// Stats is a point-in-time view of one key's pool.
type Stats struct {
	Key     golden.Key
	Target  int
	Ready   int
	Filling int
}

type Pool struct {
	opts Options
	log  *slog.Logger
	sem  chan struct{}

	mu      sync.Mutex
	ready   map[golden.Key][]*Instance
	filling map[golden.Key]int
	closed  bool

	wake   map[golden.Key]chan struct{}
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var ErrClosed = errors.New("pool: closed")

func New(opts Options) (*Pool, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.RefillConcurrency < 1 {
		opts.RefillConcurrency = 1
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, err
	}
	p := &Pool{
		opts:    opts,
		log:     opts.Logger.With("component", "pool"),
		sem:     make(chan struct{}, opts.RefillConcurrency),
		ready:   map[golden.Key][]*Instance{},
		filling: map[golden.Key]int{},
		wake:    map[golden.Key]chan struct{}{},
	}
	for key := range opts.Warm {
		p.wake[key] = make(chan struct{}, 1)
	}
	return p, nil
}

// Start launches one refiller per warm key. They run until Close.
func (p *Pool) Start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)
	for key, n := range p.opts.Warm {
		if n <= 0 {
			continue
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.refill(ctx, key, n)
		}()
	}
}

// Acquire returns a running VM for key, from the pool when possible.
func (p *Pool) Acquire(ctx context.Context, key golden.Key) (*Instance, error) {
	for {
		inst, err := p.take(ctx, key)
		if err != nil {
			return nil, err
		}
		if inst == nil {
			break
		}
		if err := inst.VM.Resume(ctx); err != nil {
			p.log.Warn("discarding pooled vm that failed to resume", "id", inst.ID, "err", err)
			_ = inst.Destroy(context.WithoutCancel(ctx))
			continue
		}
		return inst, nil
	}
	return p.restore(ctx, key)
}

func (p *Pool) take(ctx context.Context, key golden.Key) (*Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrClosed
	}
	defer p.signal(key)
	for q := p.ready[key]; len(q) > 0; q = p.ready[key] {
		inst := q[0]
		p.ready[key] = q[1:]
		select {
		case <-inst.VM.Done():
			p.log.Warn("pooled vm exited while idle", "id", inst.ID)
			go func() { _ = inst.Destroy(context.WithoutCancel(ctx)) }()
		default:
			return inst, nil
		}
	}
	return nil, nil
}

// signal wakes key's refiller. Callers hold p.mu or do not care about ordering.
func (p *Pool) signal(key golden.Key) {
	if ch := p.wake[key]; ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (p *Pool) refill(ctx context.Context, key golden.Key, target int) {
	backoff := time.Second
	for {
		p.mu.Lock()
		need := target - len(p.ready[key]) - p.filling[key]
		if need > 0 {
			p.filling[key]++
		}
		p.mu.Unlock()

		if need <= 0 {
			select {
			case <-ctx.Done():
				return
			case <-p.wake[key]:
			case <-time.After(10 * time.Second):
			}
			continue
		}

		select {
		case p.sem <- struct{}{}:
		case <-ctx.Done():
			p.doneFilling(key)
			return
		}
		inst, err := p.prewarm(ctx, key)
		<-p.sem
		if err != nil {
			p.doneFilling(key)
			if ctx.Err() != nil {
				return
			}
			p.log.Error("refill failed", "key", key.String(), "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second

		p.mu.Lock()
		p.filling[key]--
		closed := p.closed
		if !closed {
			p.ready[key] = append(p.ready[key], inst)
		}
		p.mu.Unlock()
		if closed {
			_ = inst.Destroy(context.WithoutCancel(ctx))
			return
		}
	}
}

func (p *Pool) doneFilling(key golden.Key) {
	p.mu.Lock()
	p.filling[key]--
	p.mu.Unlock()
}

// prewarm restores a VM, lets agentd answer once so the guest pages it needs
// are faulted in from the memory file, then pauses it. This moves most of the
// first-request latency from Acquire into the background refill.
func (p *Pool) prewarm(ctx context.Context, key golden.Key) (*Instance, error) {
	inst, err := p.restore(ctx, key)
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err = inst.Guest.WaitReady(rctx)
	if err == nil {
		err = inst.VM.Pause(rctx)
	}
	if err != nil {
		_ = inst.Destroy(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("pool: prewarming %s: %w", key, err)
	}
	inst.Warm = true
	return inst, nil
}

// restore starts a new, running VM from key's golden snapshot.
func (p *Pool) restore(ctx context.Context, key golden.Key) (*Instance, error) {
	snap, err := p.opts.Snapshots.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return p.Restore(ctx, snap)
}

// Restore starts a new, running VM from any snapshot, such as a user's fork.
func (p *Pool) Restore(ctx context.Context, snap *golden.Snapshot) (inst *Instance, err error) {
	key := snap.Key
	id := NewID()
	overlay := filepath.Join(p.opts.Dir, id+".ext4")
	if err := storage.CloneFile(snap.Overlay, overlay); err != nil {
		return nil, fmt.Errorf("pool: cloning overlay: %w", err)
	}
	// The driver moves the overlay into the VM's chroot; this only matters on failure.
	defer os.Remove(overlay)

	var netw *vmm.Network
	if p.opts.Network != nil {
		if netw, err = p.opts.Network.Attach(ctx, id); err != nil {
			return nil, fmt.Errorf("pool: attaching network: %w", err)
		}
		defer func() {
			if err != nil {
				_ = p.opts.Network.Detach(context.WithoutCancel(ctx), id)
			}
		}()
	}
	vm, err := p.opts.Driver.Restore(ctx, vmm.RestoreSpec{
		ID:       id,
		Snapshot: snap.Files,
		Machine:  key.Shape.Machine(),
		Disks: vmm.Disks{
			RootfsBase:     snap.Rootfs,
			Overlay:        overlay,
			BandwidthMiBps: p.opts.DiskBandwidthMiBps,
		},
		Network: netw,
		Resume:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("pool: restoring %s: %w", key, err)
	}
	return NewInstance(id, key, vm, func(ctx context.Context) error { return p.destroy(ctx, id, vm) }), nil
}

func (p *Pool) destroy(ctx context.Context, id string, vm vmm.VM) error {
	err := vm.Stop(ctx)
	if errors.Is(err, vmm.ErrNotRunning) {
		err = nil
	}
	if p.opts.Network != nil {
		err = errors.Join(err, p.opts.Network.Detach(ctx, id))
	}
	return err
}

// Stats reports every warm key.
func (p *Pool) Stats() []Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Stats, 0, len(p.opts.Warm))
	for key, n := range p.opts.Warm {
		out = append(out, Stats{Key: key, Target: n, Ready: len(p.ready[key]), Filling: p.filling[key]})
	}
	return out
}

// Close stops the refillers and destroys every pooled VM.
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	var idle []*Instance
	for key, q := range p.ready {
		idle = append(idle, q...)
		delete(p.ready, key)
	}
	p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	var errs []error
	for _, inst := range idle {
		errs = append(errs, inst.Destroy(ctx))
	}
	return errors.Join(errs...)
}

// NewID returns a random sandbox/VM id.
func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "sb-" + hex.EncodeToString(b)
}
