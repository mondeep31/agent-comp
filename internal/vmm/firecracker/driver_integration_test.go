//go:build linux && integration

package firecracker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mondeep31/agent-comp/internal/storage"
	"github.com/mondeep31/agent-comp/internal/vmm"
)

const testInit = `#!/bin/sh
mount -t proc proc /proc
i=0
while :; do i=$((i+1)); echo "TICK $i"; sleep 0.05; done
`

type fixture struct {
	dir    string
	kernel string
	rootfs string
}

// newFixture copies the Firecracker CI rootfs and installs a ticking init.
func newFixture(t *testing.T) fixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("integration tests need root (jailer)")
	}
	ci := os.Getenv("AC_CI_DIR")
	if ci == "" {
		ci = "/var/lib/agent-comp/ci"
	}
	kernel := filepath.Join(ci, "vmlinux")
	if _, err := os.Stat(kernel); err != nil {
		t.Skipf("CI artifacts missing (run scripts/host/fetch-ci-artifacts.sh): %v", err)
	}
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if err := storage.CloneFile(filepath.Join(ci, "rootfs.ext4"), rootfs); err != nil {
		t.Fatal(err)
	}
	mnt := filepath.Join(dir, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, "mount", "-o", "loop", rootfs, mnt)
	if err := os.WriteFile(filepath.Join(mnt, "ac-test-init"), []byte(testInit), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, "umount", mnt)
	if err := os.Chmod(rootfs, 0o444); err != nil {
		t.Fatal(err)
	}
	return fixture{dir: dir, kernel: kernel, rootfs: rootfs}
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.CommandContext(t.Context(), name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

func newTestDriver(t *testing.T) *Driver {
	t.Helper()
	d, err := New(Options{
		FirecrackerBin: "/usr/local/bin/firecracker",
		JailerBin:      "/usr/local/bin/jailer",
		ChrootBase:     filepath.Join(t.TempDir(), "jail"),
		UIDBase:        300000,
		CgroupParent:   "agent-comp-test",
		MaxVMs:         8,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func (f fixture) overlay(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(f.dir, name)
	if err := storage.CreateExt4(context.Background(), p, 256); err != nil {
		t.Fatal(err)
	}
	return p
}

func waitConsole(t *testing.T, vm vmm.VM, want string, timeout time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < timeout {
		if bytes.Contains(vm.Console(), []byte(want)) {
			return time.Since(start)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("vm %s: %q not seen in %v; console:\n%s", vm.ID(), want, timeout, tail(vm.Console(), 2048))
	return 0
}

func TestBootSnapshotRestoreClones(t *testing.T) {
	f := newFixture(t)
	d := newTestDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	machine := vmm.Machine{VCPU: 1, MemoryMiB: 256}
	start := time.Now()
	golden, err := d.Boot(ctx, vmm.BootSpec{
		ID:       "it-golden",
		Kernel:   f.kernel,
		BootArgs: "console=ttyS0 reboot=k panic=1 pci=off ro init=/ac-test-init",
		Machine:  machine,
		Disks:    vmm.Disks{RootfsBase: f.rootfs, Overlay: f.overlay(t, "golden.ext4")},
	})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	waitConsole(t, golden, "TICK 3", 20*time.Second)
	t.Logf("cold boot to TICK 3: %v", time.Since(start))

	if err := golden.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	snap := vmm.SnapshotFiles{State: filepath.Join(f.dir, "vm.snap"), Memory: filepath.Join(f.dir, "vm.mem")}
	if err := golden.Snapshot(ctx, snap); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	goldenOverlay := filepath.Join(f.dir, "golden-overlay.ext4")
	if err := storage.CloneFile(golden.(*jailedVM).OverlayPath(), goldenOverlay); err != nil {
		t.Fatal(err)
	}
	if err := golden.Stop(ctx); err != nil {
		t.Fatalf("Stop golden: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			overlay := filepath.Join(f.dir, fmt.Sprintf("clone-%d.ext4", i))
			if err := storage.CloneFile(goldenOverlay, overlay); err != nil {
				t.Error(err)
				return
			}
			start := time.Now()
			vm, err := d.Restore(ctx, vmm.RestoreSpec{
				ID:       fmt.Sprintf("it-clone-%d", i),
				Snapshot: snap,
				Machine:  machine,
				Disks:    vmm.Disks{RootfsBase: f.rootfs, Overlay: overlay},
				Resume:   true,
			})
			if err != nil {
				t.Errorf("Restore %d: %v", i, err)
				return
			}
			defer func() {
				if err := vm.Stop(ctx); err != nil {
					t.Errorf("Stop clone %d: %v", i, err)
				}
			}()
			loaded := time.Since(start)
			out := waitConsole(t, vm, "TICK", 5*time.Second)
			t.Logf("clone %d: restore API %v, first guest output after %v", i, loaded, loaded+out)
		}()
	}
	wg.Wait()

	entries, _ := os.ReadDir(filepath.Join(d.opts.ChrootBase, "firecracker"))
	if len(entries) != 0 {
		t.Errorf("chroots left behind: %v", entries)
	}
	cg, _ := os.ReadDir(filepath.Join(cgroupRoot, d.opts.CgroupParent))
	for _, e := range cg {
		if e.IsDir() {
			t.Errorf("cgroup left behind: %s", e.Name())
		}
	}
}
