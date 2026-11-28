//go:build linux && integration

package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mondeep31/agent-comp/internal/api"
	"github.com/mondeep31/agent-comp/internal/auth"
	"github.com/mondeep31/agent-comp/internal/client"
	"github.com/mondeep31/agent-comp/internal/config"
	"github.com/mondeep31/agent-comp/internal/daemon"
	"github.com/mondeep31/agent-comp/internal/sandbox"
	"github.com/mondeep31/agent-comp/internal/testenv"
)

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// startDaemon runs acd with one image behind a loopback listener and returns
// a client for it. Networking is covered by the network package's own test:
// both would manage the same host nftables table.
func startDaemon(t *testing.T, ctx context.Context, img config.Image, poolSize int) (c *client.Client, addr, token string) {
	t.Helper()
	dir := t.TempDir()
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
	cfg.Images = []config.Image{img}
	cfg.Sandbox.DiskMiB = 1024
	cfg.Pool.Size = poolSize
	cfg.Network.Enabled = false
	cfg.Auth.APIKeysFile = keysFile
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	d, err := daemon.New(ctx, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServe := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- d.Serve(serveCtx, ln) }()
	t.Cleanup(func() {
		stopServe()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
		if err := d.Close(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	addr = ln.Addr().String()
	return client.New("http://"+addr, token, nil), addr, token
}

func agentd(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(testenv.RepoRoot(), "bin", "agentd"))
	if err != nil {
		t.Skipf("bin/agentd missing (make build-agentd): %v", err)
	}
	return data
}

// TestEndToEnd drives real Firecracker sandboxes through the HTTP API.
func TestEndToEnd(t *testing.T) {
	ci := testenv.CI(t)
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	testenv.Rootfs(t, ci, rootfs, map[string]testenv.File{
		"/usr/local/bin/agentd": {Data: agentd(t), Mode: 0o755},
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	c, addr, token := startDaemon(t, ctx, config.Image{Name: "ci", Kernel: filepath.Join(ci, "vmlinux"), Rootfs: rootfs}, 2)
	if _, err := client.New("http://"+addr, "ac_bogus", nil).List(ctx); err == nil {
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

	// The same computer through MCP.
	mc := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "v0"}, nil)
	cs, err := mc.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   "http://" + addr + "/v1/mcp",
		HTTPClient: &http.Client{Transport: bearer(token)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mres, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "run_command", Arguments: map[string]any{
		"computer_id": first.ID, "command": "cat /work/notes.txt",
	}})
	if err != nil || mres.IsError || !strings.Contains(fmt.Sprint(mres.StructuredContent), "remember me") {
		t.Fatalf("mcp run_command = %+v, %v", mres, err)
	}
	_ = cs.Close()

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

// TestBaseImage boots the image built by `make image` and checks the tools
// agents rely on.
func TestBaseImage(t *testing.T) {
	const images = "/var/lib/agent-comp/images"
	img := config.Image{Name: "base", Kernel: filepath.Join(images, "vmlinux"), Rootfs: filepath.Join(images, "base.ext4")}
	for _, f := range []string{img.Kernel, img.Rootfs} {
		if _, err := os.Stat(f); err != nil {
			t.Skipf("%v (run make image)", err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	c, _, _ := startDaemon(t, ctx, img, 1)

	start := time.Now()
	sb, err := c.Create(ctx, &api.CreateSandboxRequest{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first create (includes golden build): %v", time.Since(start))

	script := `set -e
python3 --version; node --version; npm --version; git --version | head -1; gcc --version | head -1
make --version | head -1; curl --version | head -1; jq --version
test "$(id -u)" = 0; test "$PWD" = /root
python3 -c 'import json, sqlite3, venv, ssl; print("python stdlib ok")'
cd /work && python3 -m venv .venv && .venv/bin/python -c 'print("venv ok")'
printf 'int main(){return 7;}' > t.c && gcc t.c -o t && { ./t || echo "gcc ok: $?"; }
test -s /etc/resolv.conf && grep -q nameserver /etc/resolv.conf || echo "no resolv.conf (network disabled)"
echo "disk: $(df -h / | tail -1)"
`
	res, err := c.Exec(ctx, sb.ID, &sandbox.ExecRequest{Command: script, TimeoutSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("stdout:\n%s", res.Stdout)
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "venv ok") || !strings.Contains(res.Stdout, "gcc ok: 7") {
		t.Fatalf("exit %d, stderr:\n%s", res.ExitCode, res.Stderr)
	}
	if err := c.Delete(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
}

// TestDesktopImage boots the image built by `make image-desktop` behind the
// full acd stack and drives its screen: Chromium on a page, a click, typed
// Unicode text, keys, a screenshot of the result, pause and resume, and a
// fork from a snapshot. AC_DESKTOP_IMAGE overrides the rootfs path.
func TestDesktopImage(t *testing.T) {
	const images = "/var/lib/agent-comp/images"
	rootfs := os.Getenv("AC_DESKTOP_IMAGE")
	if rootfs == "" {
		rootfs = filepath.Join(images, "desktop.ext4")
	}
	img := config.Image{
		Name: "desktop", Kernel: filepath.Join(images, "vmlinux"), Rootfs: rootfs,
		Display: &config.Display{Width: 1280, Height: 800},
		Sandbox: &config.Shape{MemoryMiB: 1024},
	}
	for _, f := range []string{img.Kernel, img.Rootfs} {
		if _, err := os.Stat(f); err != nil {
			t.Skipf("%v (run make image-desktop)", err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	c, _, _ := startDaemon(t, ctx, img, 1)

	start := time.Now()
	sb, err := c.Create(ctx, &api.CreateSandboxRequest{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first create (includes golden build): %v", time.Since(start))
	if sb.Display == nil || sb.Display.Width != 1280 {
		t.Fatalf("sandbox has no display: %+v", sb)
	}

	shot := func(opts sandbox.ScreenshotOptions) *sandbox.Image {
		t.Helper()
		begin := time.Now()
		img, err := c.Screenshot(ctx, sb.ID, opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("screenshot %s %dx%d %d bytes in %v", img.MIMEType, img.Width, img.Height, len(img.Data), time.Since(begin))
		return img
	}
	first := shot(sandbox.ScreenshotOptions{})
	cfg, err := png.DecodeConfig(bytes.NewReader(first.Data))
	if err != nil || cfg.Width != 1280 || cfg.Height != 800 {
		t.Fatalf("png: %+v %v", cfg, err)
	}
	for range 3 {
		shot(sandbox.ScreenshotOptions{Format: "jpeg"})
	}

	run := func(cmd string) string {
		t.Helper()
		res, err := c.Exec(ctx, sb.ID, &sandbox.ExecRequest{Command: cmd, TimeoutSeconds: 30})
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("%s: exit %d\n%s%s", cmd, res.ExitCode, res.Stdout, res.Stderr)
		}
		return strings.TrimSpace(res.Stdout)
	}
	// Chromium runs as the agent user and opens URLs in the running browser.
	if who := run("ps -o user= -C chromium | sort -u | head -1"); who != "agent" {
		t.Fatalf("chromium runs as %q", who)
	}
	// The page mirrors its input into the window title, which xdotool reads.
	page := `<title>ready</title><input id=q autofocus style="font-size:28px;margin:40px;width:600px" oninput="document.title='T:'+this.value">`
	if _, err := c.WriteFile(ctx, sb.ID, "/tmp/page.html", 0o644, strings.NewReader(page)); err != nil {
		t.Fatal(err)
	}
	run(`runuser -u agent -- env DISPLAY=:0 HOME=/home/agent chromium file:///tmp/page.html >/dev/null 2>&1`)
	title := func() string {
		return run("xdotool getactivewindow getwindowname 2>/dev/null || true")
	}
	waitTitle := func(prefix string) string {
		t.Helper()
		var got string
		for range 100 {
			if got = title(); strings.HasPrefix(got, prefix) {
				return got
			}
			time.Sleep(100 * time.Millisecond)
		}
		if dir := os.Getenv("AC_SCREENSHOT_DIR"); dir != "" {
			if img, err := c.Screenshot(ctx, sb.ID, sandbox.ScreenshotOptions{}); err == nil {
				_ = os.WriteFile(filepath.Join(dir, "desktop-fail.png"), img.Data, 0o644) //nolint:gosec // test artifact
			}
			t.Logf("windows:\n%s", run("xdotool search --onlyvisible --name . getwindowname %@ 2>&1; xdotool getwindowfocus getwindowname 2>&1; tail -5 /var/log/agent-comp/session.log"))
		}
		t.Fatalf("window title %q, want prefix %q", got, prefix)
		return ""
	}
	waitTitle("ready")

	act := func(a sandbox.ComputerAction) *sandbox.ComputerResult {
		t.Helper()
		begin := time.Now()
		res, err := c.Computer(ctx, sb.ID, &a)
		if err != nil {
			t.Fatalf("%s: %v", a.Action, err)
		}
		t.Logf("%s in %v", a.Action, time.Since(begin))
		return res
	}
	// The input box sits at y 135-172 under Chromium's 87 px of toolbars.
	act(sandbox.ComputerAction{Action: "left_click", Coordinate: []int{300, 153}})
	act(sandbox.ComputerAction{Action: "type", Text: "hello wörld €"})
	if got := waitTitle("T:"); !strings.HasPrefix(got, "T:hello wörld €") {
		t.Fatalf("title = %q", got)
	}
	act(sandbox.ComputerAction{Action: "key", Text: "ctrl+a"})
	act(sandbox.ComputerAction{Action: "type", Text: "Zz"})
	if got := waitTitle("T:Zz"); !strings.HasPrefix(got, "T:Zz ") {
		t.Fatalf("title after ctrl+a and typing = %q", got)
	}
	res := act(sandbox.ComputerAction{Action: "mouse_move", Coordinate: []int{640, 400}, Screenshot: true})
	if res.CursorX != 640 || res.CursorY != 400 || res.Image == nil {
		t.Fatalf("mouse_move = %+v", res)
	}
	res = act(sandbox.ComputerAction{Action: "cursor_position"})
	if res.CursorX != 640 || res.CursorY != 400 {
		t.Fatalf("cursor = %+v", res)
	}
	zoom := act(sandbox.ComputerAction{Action: "zoom", Region: []int{0, 0, 640, 400}})
	if zoom.Image == nil || zoom.Image.Width != 640 {
		t.Fatalf("zoom = %+v", zoom)
	}
	act(sandbox.ComputerAction{Action: "scroll", Coordinate: []int{640, 400}, ScrollDirection: "down", ScrollAmount: 3})

	// After a pause the screen and the clock come back.
	if _, err := c.Pause(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	begin := time.Now()
	shot(sandbox.ScreenshotOptions{Format: "jpeg"})
	t.Logf("screenshot after pause (auto-resume): %v", time.Since(begin))
	skew := run("echo $(( $(date +%s) ))")
	if d := time.Since(time.Unix(atoi(t, skew), 0)); d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("guest clock off by %v after resume", d)
	}

	// A fork keeps the open browser window.
	snap, err := c.Snapshot(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	begin = time.Now()
	fork, err := c.Create(ctx, &api.CreateSandboxRequest{FromSnapshot: snap.ID})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fork from snapshot: %v", time.Since(begin))
	if _, err := c.Screenshot(ctx, fork.ID, sandbox.ScreenshotOptions{}); err != nil {
		t.Fatal(err)
	}
	out, err := c.Exec(ctx, fork.ID, &sandbox.ExecRequest{Command: "xdotool search --onlyvisible --class chromium | head -1"})
	if err != nil || strings.TrimSpace(out.Stdout) == "" {
		t.Fatalf("fork lost the browser window: %+v %v", out, err)
	}

	if dir := os.Getenv("AC_SCREENSHOT_DIR"); dir != "" {
		last := shot(sandbox.ScreenshotOptions{})
		_ = os.WriteFile(filepath.Join(dir, "desktop-e2e.png"), last.Data, 0o644) //nolint:gosec // test artifact
	}
	for _, id := range []string{sb.ID, fork.ID} {
		if err := c.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
}

func atoi(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil {
		t.Fatalf("not a number: %q", s)
	}
	return n
}
