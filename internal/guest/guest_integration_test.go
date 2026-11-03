//go:build linux && integration

package guest_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentdv1 "github.com/mondeep31/agent-comp/gen/go/agentcomp/agentd/v1"
	"github.com/mondeep31/agent-comp/internal/guest"
	"github.com/mondeep31/agent-comp/internal/storage"
	"github.com/mondeep31/agent-comp/internal/testenv"
	"github.com/mondeep31/agent-comp/internal/vmm"
	"github.com/mondeep31/agent-comp/internal/vmm/firecracker"
)

const bootArgs = "console=ttyS0 reboot=k panic=1 pci=off init=/usr/local/bin/agentd"

func client(vm vmm.VM) *guest.Client {
	return guest.New(func(ctx context.Context) (net.Conn, error) {
		return vm.DialVsock(ctx, guest.Port)
	})
}

// waitReady pings until agentd answers and returns how long that took.
func waitReady(ctx context.Context, t *testing.T, vm vmm.VM) time.Duration {
	t.Helper()
	c := client(vm)
	start := time.Now()
	for {
		pctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_, err := c.Ping(pctx)
		cancel()
		if err == nil {
			return time.Since(start)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("agentd not ready: %v; console:\n%s", err, vm.Console())
		case <-vm.Done():
			t.Fatalf("vm exited; console:\n%s", vm.Console())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func run(ctx context.Context, t *testing.T, c *guest.Client, argv ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	exit, err := c.Run(ctx, &agentdv1.ExecStart{Argv: argv}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("%v: %v", argv, err)
	}
	if exit.GetExitCode() != 0 {
		t.Fatalf("%v: exit %d, stderr %q", argv, exit.GetExitCode(), stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

func configure(ctx context.Context, t *testing.T, c *guest.Client, hostname string) {
	t.Helper()
	entropy := make([]byte, 64)
	_, _ = rand.Read(entropy)
	err := c.Configure(ctx, &agentdv1.Configure{
		Hostname:        hostname,
		WallClockUnixMs: time.Now().UnixMilli(),
		Entropy:         entropy,
		Env:             map[string]string{"SANDBOX_ID": hostname},
	})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
}

func TestAgentdInFirecracker(t *testing.T) {
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
		UIDBase:        310000,
		CgroupParent:   "agent-comp-test-guest",
		MaxVMs:         4,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	overlay := filepath.Join(dir, "overlay.ext4")
	if err := storage.CreateExt4(ctx, overlay, 512); err != nil {
		t.Fatal(err)
	}
	machine := vmm.Machine{VCPU: 1, MemoryMiB: 256}
	start := time.Now()
	vm, err := d.Boot(ctx, vmm.BootSpec{
		ID: "it-agentd", Kernel: filepath.Join(ci, "vmlinux"), BootArgs: bootArgs,
		Machine: machine, Disks: vmm.Disks{RootfsBase: rootfs, Overlay: overlay},
	})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	waitReady(ctx, t, vm)
	t.Logf("cold boot to agentd ready: %v", time.Since(start))
	c := client(vm)

	t.Run("configure", func(t *testing.T) {
		configure(ctx, t, c, "golden")
		if got := run(ctx, t, c, "hostname"); got != "golden" {
			t.Errorf("hostname = %q", got)
		}
		if got := run(ctx, t, c, "sh", "-c", "echo $SANDBOX_ID"); got != "golden" {
			t.Errorf("SANDBOX_ID = %q", got)
		}
	})

	t.Run("root is a writable overlay", func(t *testing.T) {
		if got := run(ctx, t, c, "sh", "-c", "grep ' / ' /proc/mounts | cut -d' ' -f3"); got != "overlay" {
			t.Errorf("root fs type = %q", got)
		}
		run(ctx, t, c, "sh", "-c", "echo baked > /etc/agent-comp-marker")
	})

	t.Run("files", func(t *testing.T) {
		payload := bytes.Repeat([]byte("0123456789"), 100_000)
		info, err := c.WriteFile(ctx, &agentdv1.WriteFile{Path: "/work/data.bin", CreateParents: true, Mode: 0o600}, bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if info.GetSize() != uint64(len(payload)) || info.GetMode() != 0o600 {
			t.Errorf("info = %v", info)
		}
		var got bytes.Buffer
		if _, err := c.ReadFile(ctx, "/work/data.bin", 0, &got); err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !bytes.Equal(got.Bytes(), payload) {
			t.Errorf("read back %d bytes, want %d", got.Len(), len(payload))
		}
		entries, err := c.ListDir(ctx, "/work")
		if err != nil || len(entries) != 1 || entries[0].GetName() != "data.bin" {
			t.Errorf("ListDir = %v, %v", entries, err)
		}
		if _, err := c.Stat(ctx, "/nope"); !guest.HasCode(err, agentdv1.ErrorCode_ERROR_CODE_NOT_FOUND) {
			t.Errorf("Stat missing = %v", err)
		}
	})

	t.Run("exec timeout", func(t *testing.T) {
		exit, err := c.Run(ctx, &agentdv1.ExecStart{Argv: []string{"sleep", "30"}, TimeoutMs: 200}, nil, nil, nil)
		if err != nil || !exit.GetTimedOut() {
			t.Errorf("exit = %v, err = %v", exit, err)
		}
	})

	// Snapshot the configured VM and check a clone keeps disk state but takes
	// its own identity.
	run(ctx, t, c, "sync")
	if err := vm.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	snap := vmm.SnapshotFiles{State: filepath.Join(dir, "vm.snap"), Memory: filepath.Join(dir, "vm.mem")}
	if err := vm.Snapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	type overlayed interface{ OverlayPath() string }
	cloneOverlay := filepath.Join(dir, "clone.ext4")
	if err := storage.CloneFile(vm.(overlayed).OverlayPath(), cloneOverlay); err != nil {
		t.Fatal(err)
	}
	if err := vm.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	start = time.Now()
	clone, err := d.Restore(ctx, vmm.RestoreSpec{
		ID: "it-agentd-clone", Snapshot: snap, Machine: machine,
		Disks: vmm.Disks{RootfsBase: rootfs, Overlay: cloneOverlay}, Resume: true,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	defer func() {
		if err := clone.Stop(context.WithoutCancel(ctx)); err != nil && !errors.Is(err, vmm.ErrNotRunning) {
			t.Errorf("Stop clone: %v", err)
		}
	}()
	waitReady(ctx, t, clone)
	t.Logf("restore to agentd ready: %v", time.Since(start))
	cc := client(clone)
	configure(ctx, t, cc, "clone-1")
	t.Logf("restore to configured: %v", time.Since(start))
	if got := run(ctx, t, cc, "hostname"); got != "clone-1" {
		t.Errorf("clone hostname = %q", got)
	}
	if got := run(ctx, t, cc, "cat", "/etc/agent-comp-marker"); got != "baked" {
		t.Errorf("clone marker = %q", got)
	}
}
