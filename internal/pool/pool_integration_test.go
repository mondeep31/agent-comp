//go:build linux && integration

package pool_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	agentdv1 "github.com/mondeep31/agent-comp/gen/go/agentcomp/agentd/v1"
	"github.com/mondeep31/agent-comp/internal/golden"
	"github.com/mondeep31/agent-comp/internal/pool"
	"github.com/mondeep31/agent-comp/internal/testenv"
	"github.com/mondeep31/agent-comp/internal/vmm/firecracker"
)

// TestGoldenAndPool measures the numbers recorded in docs/BENCHMARKS.md.
func TestGoldenAndPool(t *testing.T) {
	ci := testenv.CI(t)
	agentd, err := os.ReadFile(filepath.Join(testenv.RepoRoot(), "bin", "agentd"))
	if err != nil {
		t.Skipf("bin/agentd missing (make build-agentd): %v", err)
	}
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "rootfs.ext4")
	testenv.Rootfs(t, ci, rootfs, map[string]testenv.File{
		"/usr/local/bin/agentd": {Data: agentd, Mode: 0o755},
	})
	d, err := firecracker.New(firecracker.Options{
		FirecrackerBin: "/usr/local/bin/firecracker",
		JailerBin:      "/usr/local/bin/jailer",
		ChrootBase:     filepath.Join(dir, "jail"),
		UIDBase:        320000,
		CgroupParent:   "agent-comp-test-pool",
		MaxVMs:         16,
	})
	if err != nil {
		t.Fatal(err)
	}
	images := []golden.Image{{Name: "ci", Kernel: filepath.Join(ci, "vmlinux"), Rootfs: rootfs}}
	newGolden := func() *golden.Manager {
		g, err := golden.New(golden.Options{
			Driver: d, Dir: filepath.Join(dir, "golden"), Images: images,
			FirecrackerBin: "/usr/local/bin/firecracker",
		})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	key := golden.Key{Image: "ci", Shape: golden.Shape{VCPU: 1, MemoryMiB: 256, DiskMiB: 1024}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	g := newGolden()
	snap, err := g.Get(ctx, key)
	if err != nil {
		t.Fatalf("golden build: %v", err)
	}
	t.Logf("golden build (cold boot + snapshot): %v", snap.BuildDur)
	start := time.Now()
	if _, err := newGolden().Get(ctx, key); err != nil {
		t.Fatal(err)
	}
	t.Logf("golden reuse from disk: %v", time.Since(start))

	// ready measures Acquire through a configured agentd, the moment a
	// sandbox becomes usable.
	ready := func(p *pool.Pool, i int) (time.Duration, *pool.Instance) {
		t.Helper()
		start := time.Now()
		inst, err := p.Acquire(ctx, key)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		acquired := time.Since(start)
		if err := inst.Guest.WaitReady(ctx); err != nil {
			t.Fatalf("WaitReady: %v\n%s", err, inst.VM.Console())
		}
		pinged := time.Since(start)
		err = inst.Guest.Configure(ctx, &agentdv1.Configure{
			Hostname: fmt.Sprintf("sb-%d", i), WallClockUnixMs: time.Now().UnixMilli(),
		})
		if err != nil {
			t.Fatalf("Configure: %v", err)
		}
		total := time.Since(start)
		if testing.Verbose() {
			t.Logf("  acquire %v, agent ready %v, configured %v", acquired, pinged, total)
		}
		return total, inst
	}

	t.Run("warm", func(t *testing.T) {
		p, err := pool.New(pool.Options{
			Driver: d, Snapshots: g, Dir: filepath.Join(dir, "vms-warm"),
			Warm: map[golden.Key]int{key: 2}, RefillConcurrency: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		p.Start(ctx)
		defer p.Close(context.WithoutCancel(ctx))
		var lat []time.Duration
		for i := range 8 {
			waitFull(t, p, 2)
			dur, inst := ready(p, i)
			if !inst.Warm {
				t.Errorf("acquire %d was not served warm", i)
			}
			lat = append(lat, dur)
			if err := inst.Destroy(ctx); err != nil {
				t.Error(err)
			}
		}
		t.Logf("warm acquire to configured: %s", summary(lat))
	})

	t.Run("on-demand", func(t *testing.T) {
		p, err := pool.New(pool.Options{Driver: d, Snapshots: g, Dir: filepath.Join(dir, "vms-cold")})
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close(context.WithoutCancel(ctx))
		var lat []time.Duration
		for i := range 5 {
			dur, inst := ready(p, i)
			lat = append(lat, dur)
			if err := inst.Destroy(ctx); err != nil {
				t.Error(err)
			}
		}
		t.Logf("on-demand restore to configured: %s", summary(lat))

		const n = 6
		var (
			mu   sync.Mutex
			par  []time.Duration
			wg   sync.WaitGroup
			all  []*pool.Instance
			wall = time.Now()
		)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				dur, inst := ready(p, 100+i)
				mu.Lock()
				par = append(par, dur)
				all = append(all, inst)
				mu.Unlock()
			}()
		}
		wg.Wait()
		t.Logf("%d parallel on-demand restores: %s, wall %v", n, summary(par), time.Since(wall))
		for _, inst := range all {
			if err := inst.Destroy(ctx); err != nil {
				t.Error(err)
			}
		}
	})
}

func waitFull(t *testing.T, p *pool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if s := p.Stats(); len(s) == 1 && s[0].Ready >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pool not refilled: %+v", p.Stats())
}

func summary(d []time.Duration) string {
	s := slices.Clone(d)
	slices.Sort(s)
	return fmt.Sprintf("min %v p50 %v max %v (n=%d)",
		s[0].Round(100*time.Microsecond), s[len(s)/2].Round(100*time.Microsecond),
		s[len(s)-1].Round(100*time.Microsecond), len(s))
}
