package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	agentdv1 "github.com/mondeep31/agent-comp/gen/go/agentcomp/agentd/v1"
	"github.com/mondeep31/agent-comp/internal/config"
	"github.com/mondeep31/agent-comp/internal/golden"
	"github.com/mondeep31/agent-comp/internal/guest/guesttest"
	"github.com/mondeep31/agent-comp/internal/sandbox"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type fixture struct {
	m      *sandbox.Manager
	src    *guesttest.Source
	clock  *clock
	mu     sync.Mutex
	events []sandbox.Event
}

func (f *fixture) Events() []sandbox.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sandbox.Event(nil), f.events...)
}

func newFixture(t *testing.T, tweak func(*sandbox.Options)) *fixture {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	f := &fixture{src: guesttest.NewSource(dir), clock: &clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
	opts := sandbox.Options{
		Source:       f.src,
		Images:       map[string]sandbox.ImageSpec{"base": {Rootfs: "/images/base.ext4", Shape: golden.Shape{VCPU: cfg.Sandbox.VCPU, MemoryMiB: cfg.Sandbox.MemoryMiB, DiskMiB: cfg.Sandbox.DiskMiB}}},
		DefaultImage: "base",
		Defaults:     cfg.Sandbox,
		Limits:       cfg.Limits,
		SnapshotDir:  filepath.Join(dir, "snapshots"),
		Now:          f.clock.Now,
		Events: func(e sandbox.Event) {
			f.mu.Lock()
			f.events = append(f.events, e)
			f.mu.Unlock()
		},
	}
	if tweak != nil {
		tweak(&opts)
	}
	m, err := sandbox.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close(context.Background()) })
	f.m = m
	return f
}

func run(t *testing.T, m *sandbox.Manager, owner, id string, argv ...string) string {
	t.Helper()
	c, done, err := m.Use(t.Context(), owner, id)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	var out bytes.Buffer
	if _, err := c.Run(t.Context(), &agentdv1.ExecStart{Argv: argv}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestCreateConfiguresIdentity(t *testing.T) {
	f := newFixture(t, nil)
	info, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice", Env: map[string]string{"FOO": "bar"}})
	if err != nil {
		t.Fatal(err)
	}
	if info.State != sandbox.StateRunning || info.Image != "base" || info.VCPU != 1 || info.MemoryMiB != 256 {
		t.Fatalf("info = %+v", info)
	}
	if got := run(t, f.m, "alice", info.ID, "hostname"); got != info.ID+"\n" {
		t.Errorf("hostname = %q, want %q", got, info.ID)
	}
	if got := run(t, f.m, "alice", info.ID, "env", "FOO"); got != "bar\n" {
		t.Errorf("FOO = %q", got)
	}
	if !info.ExpiresAt.Equal(f.clock.Now().Add(time.Hour)) {
		t.Errorf("expires_at = %v", info.ExpiresAt)
	}
}

func TestCreateValidation(t *testing.T) {
	f := newFixture(t, nil)
	for name, req := range map[string]sandbox.CreateRequest{
		"image":    {Image: "nope"},
		"vcpu":     {VCPU: 99},
		"memory":   {MemoryMiB: 64},
		"disk":     {DiskMiB: 1 << 30},
		"ttl":      {TTL: 48 * time.Hour},
		"env name": {Env: map[string]string{"A=B": "x"}},
		"snapshot": {FromSnapshot: "snap-missing"},
	} {
		t.Run(name, func(t *testing.T) {
			req.Owner = "alice"
			_, err := f.m.Create(t.Context(), req)
			if !errors.Is(err, sandbox.ErrInvalid) && !errors.Is(err, sandbox.ErrNotFound) {
				t.Fatalf("err = %v, want invalid or not found", err)
			}
		})
	}
}

func TestOwnershipIsolation(t *testing.T) {
	f := newFixture(t, nil)
	info, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Get("mallory", info.ID); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("Get by other owner: %v", err)
	}
	if _, _, err := f.m.Use(t.Context(), "mallory", info.ID); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("Use by other owner: %v", err)
	}
	if err := f.m.Delete(t.Context(), "mallory", info.ID); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("Delete by other owner: %v", err)
	}
	if got := f.m.List("mallory"); len(got) != 0 {
		t.Errorf("List by other owner = %v", got)
	}
	if got := f.m.List("alice"); len(got) != 1 {
		t.Errorf("List = %v", got)
	}
}

func TestQuotas(t *testing.T) {
	f := newFixture(t, func(o *sandbox.Options) {
		o.Limits.MaxSandboxes = 3
		o.Limits.MaxSandboxesPerKey = 2
	})
	create := func(owner string) error {
		_, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: owner})
		return err
	}
	for range 2 {
		if err := create("alice"); err != nil {
			t.Fatal(err)
		}
	}
	if err := create("alice"); !errors.Is(err, sandbox.ErrQuota) {
		t.Fatalf("per-key quota: %v", err)
	}
	if err := create("bob"); err != nil {
		t.Fatal(err)
	}
	if err := create("carol"); !errors.Is(err, sandbox.ErrQuota) {
		t.Fatalf("host quota: %v", err)
	}
	// Deleting frees the slot.
	if err := f.m.Delete(t.Context(), "bob", f.m.List("bob")[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := create("carol"); err != nil {
		t.Fatal(err)
	}
}

func newDesktopFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, withDesktop)
	f.src.Displays = map[string]*agentdv1.Display{"desktop": {Width: 1280, Height: 800, Ready: true}}
	return f
}

func TestScreenshotAndComputer(t *testing.T) {
	f := newDesktopFixture(t)
	ctx := t.Context()
	desk, err := f.m.Create(ctx, sandbox.CreateRequest{Owner: "alice", Image: "desktop"})
	if err != nil {
		t.Fatal(err)
	}
	img, err := f.m.Screenshot(ctx, "alice", desk.ID, sandbox.ScreenshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if img.MIMEType != "image/png" || img.Width != 1280 || img.Height != 800 || len(img.Data) == 0 {
		t.Fatalf("image = %s %dx%d %d bytes", img.MIMEType, img.Width, img.Height, len(img.Data))
	}
	jpg, err := f.m.Screenshot(ctx, "alice", desk.ID, sandbox.ScreenshotOptions{Format: "jpeg", Region: []int{0, 0, 100, 50}})
	if err != nil || jpg.MIMEType != "image/jpeg" || jpg.Width != 100 {
		t.Fatalf("jpeg = %+v, %v", jpg, err)
	}

	res, err := f.m.Computer(ctx, "alice", desk.ID, &sandbox.ComputerAction{Action: "left_click", Coordinate: []int{640, 400}})
	if err != nil {
		t.Fatal(err)
	}
	if res.CursorX != 640 || res.CursorY != 400 || res.Image != nil {
		t.Fatalf("click = %+v", res)
	}
	res, err = f.m.Computer(ctx, "alice", desk.ID, &sandbox.ComputerAction{Action: "type", Text: "hi", Screenshot: true})
	if err != nil || res.Image == nil {
		t.Fatalf("type with screenshot = %+v, %v", res, err)
	}
	zoom, err := f.m.Computer(ctx, "alice", desk.ID, &sandbox.ComputerAction{Action: "zoom", Region: []int{10, 10, 210, 110}})
	if err != nil || zoom.Image == nil || zoom.Image.Width != 200 {
		t.Fatalf("zoom = %+v, %v", zoom, err)
	}
	if n := len(f.src.VMs()[0].Agent.Inputs()); n != 3 {
		t.Fatalf("guest saw %d events, want 3 (move, click, text)", n)
	}

	// Bad input never reaches the guest.
	if _, err := f.m.Computer(ctx, "alice", desk.ID, &sandbox.ComputerAction{Action: "left_click", Coordinate: []int{5000, 0}}); !errors.Is(err, sandbox.ErrInvalid) {
		t.Fatalf("off-screen click: %v", err)
	}
	// Other owners cannot see the screen.
	if _, err := f.m.Screenshot(ctx, "bob", desk.ID, sandbox.ScreenshotOptions{}); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("bob: %v", err)
	}
	// Headless sandboxes have no screen.
	base, err := f.m.Create(ctx, sandbox.CreateRequest{Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Screenshot(ctx, "alice", base.ID, sandbox.ScreenshotOptions{}); !errors.Is(err, sandbox.ErrInvalid) {
		t.Fatalf("headless: %v", err)
	}
}

func TestDisplayNotReadyIsUnavailable(t *testing.T) {
	f := newDesktopFixture(t)
	desk, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice", Image: "desktop"})
	if err != nil {
		t.Fatal(err)
	}
	// The guest lost its display (e.g. the X server crashed).
	f.src.VMs()[0].Agent.SetDisplay(nil)
	_, err = f.m.Screenshot(t.Context(), "alice", desk.ID, sandbox.ScreenshotOptions{})
	if !errors.Is(err, sandbox.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func withDesktop(o *sandbox.Options) {
	o.Images["desktop"] = sandbox.ImageSpec{
		Rootfs:  "/images/desktop.ext4",
		Shape:   golden.Shape{VCPU: 1, MemoryMiB: 1024, DiskMiB: 2048},
		Display: &config.Display{Width: 1280, Height: 800},
	}
}

func TestImageDefaultsAndDisplay(t *testing.T) {
	f := newFixture(t, withDesktop)
	info, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice", Image: "desktop"})
	if err != nil {
		t.Fatal(err)
	}
	if info.MemoryMiB != 1024 || info.Display == nil || info.Display.Width != 1280 {
		t.Fatalf("desktop info = %+v", info)
	}
	base, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if base.MemoryMiB != 256 || base.Display != nil {
		t.Fatalf("base info = %+v", base)
	}
	_, err = f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice", Image: "desktop", MemoryMiB: 512})
	if !errors.Is(err, sandbox.ErrInvalid) {
		t.Fatalf("small desktop: %v", err)
	}
	imgs := f.m.Images()
	if len(imgs) != 2 || imgs[0].Name != "base" || !imgs[0].Default || imgs[1].Display == nil {
		t.Fatalf("images = %+v", imgs)
	}
}

func TestMemoryBudget(t *testing.T) {
	f := newFixture(t, func(o *sandbox.Options) {
		withDesktop(o)
		o.Limits.MaxCommittedMemoryMiB = 2048 + 256
	})
	create := func(image string) (sandbox.Info, error) {
		return f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice", Image: image})
	}
	first, err := create("desktop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := create("desktop"); err != nil {
		t.Fatal(err)
	}
	if _, err := create("desktop"); !errors.Is(err, sandbox.ErrQuota) {
		t.Fatalf("over budget: %v", err)
	}
	// A small sandbox still fits in what is left.
	if _, err := create("base"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Delete(t.Context(), "alice", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := create("desktop"); err != nil {
		t.Fatalf("after delete: %v", err)
	}
}

func TestStartFailureReleasesQuota(t *testing.T) {
	f := newFixture(t, func(o *sandbox.Options) { o.Limits.MaxSandboxesPerKey = 1 })
	f.src.Err = errors.New("boom")
	if _, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice"}); err == nil {
		t.Fatal("want error")
	}
	f.src.Err = nil
	if _, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
}

func TestIdlePauseAndAutoResume(t *testing.T) {
	f := newFixture(t, nil)
	info, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice", IdleTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	vm := f.src.VMs()[0]

	// An operation in flight keeps the sandbox awake.
	_, done, err := f.m.Use(t.Context(), "alice", info.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(2 * time.Minute)
	f.m.Reap(t.Context())
	if vm.Paused() {
		t.Fatal("paused with an operation in flight")
	}
	done()

	f.clock.Advance(2 * time.Minute)
	f.m.Reap(t.Context())
	if got, _ := f.m.Get("alice", info.ID); got.State != sandbox.StatePaused || !vm.Paused() {
		t.Fatalf("state = %s, vm paused = %v", got.State, vm.Paused())
	}

	if got := run(t, f.m, "alice", info.ID, "echo", "hi"); got != "hi\n" {
		t.Fatalf("echo = %q", got)
	}
	if got, _ := f.m.Get("alice", info.ID); got.State != sandbox.StateRunning || vm.Paused() {
		t.Fatalf("not resumed: %s", got.State)
	}
}

func TestResumeResyncsClock(t *testing.T) {
	f := newFixture(t, nil)
	info, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	agent := f.src.VMs()[0].Agent
	if got := agent.ClockMs(); got != f.clock.Now().UnixMilli() {
		t.Fatalf("clock at create = %d", got)
	}
	if _, err := f.m.Pause(t.Context(), "alice", info.ID); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Hour)
	run(t, f.m, "alice", info.ID, "echo", "hi")
	if got := agent.ClockMs(); got != f.clock.Now().UnixMilli() {
		t.Fatalf("clock after auto-resume = %d, want %d", got, f.clock.Now().UnixMilli())
	}

	f.clock.Advance(time.Minute)
	if _, err := f.m.Snapshot(t.Context(), "alice", info.ID); err != nil {
		t.Fatal(err)
	}
	if got := agent.ClockMs(); got != f.clock.Now().UnixMilli() {
		t.Fatalf("clock after snapshot = %d", got)
	}
}

func TestTTLStopsAndForgets(t *testing.T) {
	f := newFixture(t, nil)
	info, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice", TTL: time.Minute, IdleTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Minute)
	f.m.Reap(t.Context())
	got, err := f.m.Get("alice", info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != sandbox.StateStopped || got.StopReason != "ttl expired" || !f.src.VMs()[0].Stopped() {
		t.Fatalf("after ttl: %+v", got)
	}
	if _, _, err := f.m.Use(t.Context(), "alice", info.ID); !errors.Is(err, sandbox.ErrConflict) {
		t.Fatalf("Use stopped: %v", err)
	}
	f.clock.Advance(time.Hour)
	f.m.Reap(t.Context())
	if _, err := f.m.Get("alice", info.ID); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("not forgotten: %v", err)
	}
}

func TestVMCrashMarksStopped(t *testing.T) {
	f := newFixture(t, nil)
	info, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	f.src.VMs()[0].Crash()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := f.m.Get("alice", info.ID)
		if got.State == sandbox.StateStopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state = %s", got.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPauseResumeDelete(t *testing.T) {
	f := newFixture(t, nil)
	info, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.m.Pause(t.Context(), "alice", info.ID); err != nil || got.State != sandbox.StatePaused {
		t.Fatalf("Pause = %+v, %v", got, err)
	}
	if got, err := f.m.Resume(t.Context(), "alice", info.ID); err != nil || got.State != sandbox.StateRunning {
		t.Fatalf("Resume = %+v, %v", got, err)
	}
	if err := f.m.Delete(t.Context(), "alice", info.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Delete(t.Context(), "alice", info.ID); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("second Delete: %v", err)
	}
	if !f.src.VMs()[0].Stopped() {
		t.Fatal("vm still running")
	}
	var stops int
	for _, e := range f.Events() {
		if e.To == sandbox.StateStopped {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("%d stop events, want 1", stops)
	}
}

func TestSnapshotFork(t *testing.T) {
	f := newFixture(t, func(o *sandbox.Options) { o.Limits.MaxSnapshotsPerKey = 1 })
	src, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	c, done, err := f.m.Use(t.Context(), "alice", src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteFile(t.Context(), &agentdv1.WriteFile{Path: "/work/state"}, bytes.NewReader([]byte("v1"))); err != nil {
		t.Fatal(err)
	}
	done()

	snap, err := f.m.Snapshot(t.Context(), "alice", src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := f.m.Get("alice", src.ID); got.State != sandbox.StateRunning {
		t.Fatalf("source state after snapshot = %s", got.State)
	}
	if _, err := f.m.Snapshot(t.Context(), "alice", src.ID); !errors.Is(err, sandbox.ErrQuota) {
		t.Fatalf("snapshot quota: %v", err)
	}
	if _, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "bob", FromSnapshot: snap.ID}); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("fork by other owner: %v", err)
	}

	fork, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice", FromSnapshot: snap.ID})
	if err != nil {
		t.Fatal(err)
	}
	if fork.FromSnapshot != snap.ID || fork.ID == src.ID {
		t.Fatalf("fork = %+v", fork)
	}
	c, done, err = f.m.Use(t.Context(), "alice", fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	var buf bytes.Buffer
	if _, err := c.ReadFile(t.Context(), "/work/state", 0, &buf); err != nil || buf.String() != "v1" {
		t.Fatalf("forked file = %q, %v", buf.String(), err)
	}
	if got := run(t, f.m, "alice", fork.ID, "hostname"); got != fork.ID+"\n" {
		t.Errorf("fork hostname = %q", got)
	}

	if got := f.m.ListSnapshots("alice"); len(got) != 1 {
		t.Fatalf("ListSnapshots = %v", got)
	}
	if err := f.m.DeleteSnapshot("alice", snap.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.m.DeleteSnapshot("alice", snap.ID); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("second DeleteSnapshot: %v", err)
	}
}

func TestCloseStopsEverything(t *testing.T) {
	f := newFixture(t, nil)
	for range 3 {
		if _, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice"}); err != nil {
			t.Fatal(err)
		}
	}
	f.m.Close(t.Context())
	for _, vm := range f.src.VMs() {
		if !vm.Stopped() {
			t.Errorf("%s still running", vm.ID())
		}
	}
	if _, err := f.m.Create(t.Context(), sandbox.CreateRequest{Owner: "alice"}); !errors.Is(err, sandbox.ErrClosed) {
		t.Fatalf("Create after Close: %v", err)
	}
}
