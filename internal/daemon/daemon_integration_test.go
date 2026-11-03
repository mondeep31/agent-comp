//go:build linux && integration

package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mondeep31/agent-comp/internal/api"
	"github.com/mondeep31/agent-comp/internal/auth"
	"github.com/mondeep31/agent-comp/internal/client"
	"github.com/mondeep31/agent-comp/internal/config"
	"github.com/mondeep31/agent-comp/internal/daemon"
	"github.com/mondeep31/agent-comp/internal/sandbox"
	"github.com/mondeep31/agent-comp/internal/testenv"
)

// TestEndToEnd drives real Firecracker sandboxes through the HTTP API.
// Networking is covered by the network package's own test.
func TestEndToEnd(t *testing.T) {
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

	token, line, err := auth.Generate("e2e")
	if err != nil {
		t.Fatal(err)
	}
	keysFile := filepath.Join(dir, "keys")
	if err := os.WriteFile(keysFile, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.Firecracker.ChrootBase = filepath.Join(dir, "jail")
	cfg.Firecracker.UIDBase = 350000
	cfg.Firecracker.CgroupParent = "agent-comp-test-daemon"
	cfg.Images = []config.Image{{Name: "ci", Kernel: filepath.Join(ci, "vmlinux"), Rootfs: rootfs}}
	cfg.Sandbox.DiskMiB = 1024
	cfg.Pool.Size = 2
	cfg.Network.Enabled = false
	cfg.Auth.APIKeysFile = keysFile
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	d, err := daemon.New(ctx, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := d.Close(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServe := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- d.Serve(serveCtx, ln) }()
	defer func() {
		stopServe()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()

	c := client.New("http://"+ln.Addr().String(), token, nil)
	if _, err := client.New("http://"+ln.Addr().String(), "ac_bogus", nil).List(ctx); err == nil {
		t.Fatal("bogus key accepted")
	}

	start := time.Now()
	first, err := c.Create(ctx, &api.CreateSandboxRequest{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first create (includes golden build): %v", time.Since(start))

	res, err := c.Exec(ctx, first.ID, &sandbox.ExecRequest{Command: "hostname; echo $HOME; id -u"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.HasPrefix(res.Stdout, first.ID+"\n") {
		t.Fatalf("exec = %+v", res)
	}

	if _, err := c.WriteFile(ctx, first.ID, "/work/notes.txt", 0o644, strings.NewReader("remember me")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := c.ReadFile(ctx, first.ID, "/work/notes.txt", &buf); err != nil || buf.String() != "remember me" {
		t.Fatalf("ReadFile = %q, %v", buf.String(), err)
	}
	entries, err := c.ListDir(ctx, first.ID, "/work")
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListDir = %+v, %v", entries, err)
	}

	buf.Reset()
	exit, err := c.ExecStream(ctx, first.ID, &sandbox.ExecRequest{Command: "tr a-z A-Z"}, strings.NewReader("shout"), &buf, os.Stderr)
	if err != nil || *exit.ExitCode != 0 || buf.String() != "SHOUT" {
		t.Fatalf("stream = %q, %+v, %v", buf.String(), exit, err)
	}

	res, err = c.Exec(ctx, first.ID, &sandbox.ExecRequest{Argv: []string{"sleep", "30"}, TimeoutSeconds: 0.5})
	if err != nil || !res.TimedOut {
		t.Fatalf("timeout = %+v, %v", res, err)
	}

	// Warm creates come from the pool.
	for i := range 3 {
		start := time.Now()
		info, err := c.Create(ctx, &api.CreateSandboxRequest{})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("create %d: %v over HTTP (server startup_ms=%d, warm=%v)", i, time.Since(start), info.StartupMs, info.Warm)
		if err := c.Delete(ctx, info.ID); err != nil {
			t.Fatal(err)
		}
	}

	snap, err := c.Snapshot(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	fork, err := c.Create(ctx, &api.CreateSandboxRequest{FromSnapshot: snap.ID})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fork from user snapshot: %v", time.Since(start))
	buf.Reset()
	if err := c.ReadFile(ctx, fork.ID, "/work/notes.txt", &buf); err != nil || buf.String() != "remember me" {
		t.Fatalf("fork file = %q, %v", buf.String(), err)
	}
	if res, err := c.Exec(ctx, fork.ID, &sandbox.ExecRequest{Argv: []string{"hostname"}}); err != nil || res.Stdout != fork.ID+"\n" {
		t.Fatalf("fork hostname = %+v, %v", res, err)
	}
	// The fork's writes do not leak into the source.
	if _, err := c.WriteFile(ctx, fork.ID, "/work/notes.txt", 0, strings.NewReader("forked")); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := c.ReadFile(ctx, first.ID, "/work/notes.txt", &buf); err != nil || buf.String() != "remember me" {
		t.Fatalf("source after fork write = %q, %v", buf.String(), err)
	}

	if _, err := c.Pause(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if res, err := c.Exec(ctx, first.ID, &sandbox.ExecRequest{Argv: []string{"true"}}); err != nil || res.ExitCode != 0 {
		t.Fatalf("exec after pause = %+v, %v", res, err)
	}

	for _, id := range []string{first.ID, fork.ID} {
		if err := c.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Get(ctx, first.ID); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("Get after delete: %v", err)
	}
	if err := c.DeleteSnapshot(ctx, snap.ID); err != nil {
		t.Fatal(err)
	}
}
