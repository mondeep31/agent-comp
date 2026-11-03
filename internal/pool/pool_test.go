package pool

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mondeep31/agent-comp/internal/golden"
	"github.com/mondeep31/agent-comp/internal/vmm"
)

type fakeVM struct {
	id        string
	resumeErr error
	paused    atomic.Bool
	stopped   atomic.Bool
	done      chan struct{}
	once      sync.Once
}

func (v *fakeVM) ID() string { return v.id }
func (v *fakeVM) Pause(context.Context) error {
	v.paused.Store(true)
	return nil
}

func (v *fakeVM) Resume(context.Context) error {
	if v.resumeErr != nil {
		return v.resumeErr
	}
	v.paused.Store(false)
	return nil
}
func (v *fakeVM) Snapshot(context.Context, vmm.SnapshotFiles) error { return nil }

// DialVsock answers any request with an encoded Pong frame.
func (v *fakeVM) DialVsock(context.Context, uint32) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		var hdr [4]byte
		if _, err := io.ReadFull(server, hdr[:]); err != nil {
			return
		}
		if _, err := io.CopyN(io.Discard, server, int64(binary.BigEndian.Uint32(hdr[:]))); err != nil {
			return
		}
		_, _ = server.Write([]byte{0, 0, 0, 2, 0x1a, 0})
	}()
	return client, nil
}
func (v *fakeVM) OverlayPath() string   { return "" }
func (v *fakeVM) Console() []byte       { return nil }
func (v *fakeVM) Done() <-chan struct{} { return v.done }
func (v *fakeVM) Stop(context.Context) error {
	v.stopped.Store(true)
	v.once.Do(func() { close(v.done) })
	return nil
}

type fakeDriver struct {
	mu        sync.Mutex
	vms       []*fakeVM
	resumeErr error
}

func (d *fakeDriver) Boot(context.Context, vmm.BootSpec) (vmm.VM, error) {
	return nil, errors.New("not used")
}

func (d *fakeDriver) Restore(_ context.Context, spec vmm.RestoreSpec) (vmm.VM, error) {
	if _, err := os.Stat(spec.Disks.Overlay); err != nil {
		return nil, err
	}
	vm := &fakeVM{id: spec.ID, done: make(chan struct{}), resumeErr: d.resumeErr}
	vm.paused.Store(!spec.Resume)
	d.mu.Lock()
	d.vms = append(d.vms, vm)
	d.mu.Unlock()
	return vm, nil
}

func (d *fakeDriver) all() []*fakeVM {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*fakeVM(nil), d.vms...)
}

type fakeSnapshots struct{ overlay string }

func (s fakeSnapshots) Get(_ context.Context, key golden.Key) (*golden.Snapshot, error) {
	return &golden.Snapshot{Key: key, Overlay: s.overlay}, nil
}

var key = golden.Key{Image: "base", Shape: golden.Shape{VCPU: 1, MemoryMiB: 256, DiskMiB: 64}}

func newPool(t *testing.T, d *fakeDriver, warm int) *Pool {
	t.Helper()
	dir := t.TempDir()
	overlay := filepath.Join(dir, "golden.ext4")
	if err := os.WriteFile(overlay, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(Options{
		Driver:    d,
		Snapshots: fakeSnapshots{overlay: overlay},
		Dir:       filepath.Join(dir, "vms"),
		Warm:      map[golden.Key]int{key: warm},
	})
	if err != nil {
		t.Fatal(err)
	}
	p.Start(t.Context())
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p
}

func waitReady(t *testing.T, p *Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := p.Stats(); len(s) == 1 && s[0].Ready == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pool did not reach %d ready: %+v", n, p.Stats())
}

func TestPoolKeepsWarmVMsAndRefills(t *testing.T) {
	d := &fakeDriver{}
	p := newPool(t, d, 2)
	waitReady(t, p, 2)
	for _, vm := range d.all() {
		if !vm.paused.Load() {
			t.Errorf("pooled vm %s is running", vm.id)
		}
	}

	inst, err := p.Acquire(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if !inst.Warm || inst.VM.(*fakeVM).paused.Load() {
		t.Fatalf("want a resumed warm vm, got warm=%v", inst.Warm)
	}
	waitReady(t, p, 2)
	if got := len(d.all()); got != 3 {
		t.Fatalf("restores = %d, want 3", got)
	}
	if err := inst.Destroy(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !inst.VM.(*fakeVM).stopped.Load() {
		t.Fatal("Destroy did not stop the vm")
	}
}

func TestAcquireRestoresOnDemandForColdKeys(t *testing.T) {
	d := &fakeDriver{}
	p := newPool(t, d, 0)
	other := golden.Key{Image: "base", Shape: golden.Shape{VCPU: 2, MemoryMiB: 512, DiskMiB: 64}}
	inst, err := p.Acquire(t.Context(), other)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Warm || inst.Key != other {
		t.Fatalf("got warm=%v key=%v", inst.Warm, inst.Key)
	}
}

func TestAcquireDiscardsVMsThatFailToResume(t *testing.T) {
	d := &fakeDriver{resumeErr: errors.New("boom")}
	p := newPool(t, d, 1)
	waitReady(t, p, 1)
	d.mu.Lock()
	d.resumeErr = nil
	d.mu.Unlock()

	inst, err := p.Acquire(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	broken := d.all()[0]
	if inst.VM == vmm.VM(broken) || inst.VM.(*fakeVM).paused.Load() {
		t.Fatal("expected a different, running vm after the warm vm failed")
	}
	if !broken.stopped.Load() {
		t.Fatal("broken pooled vm was not destroyed")
	}
}

func TestCloseDestroysIdleVMs(t *testing.T) {
	d := &fakeDriver{}
	p := newPool(t, d, 2)
	waitReady(t, p, 2)
	if err := p.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, vm := range d.all() {
		if !vm.stopped.Load() {
			t.Errorf("vm %s still running after Close", vm.id)
		}
	}
	if _, err := p.Acquire(t.Context(), key); !errors.Is(err, ErrClosed) {
		t.Fatalf("Acquire after Close = %v", err)
	}
}
