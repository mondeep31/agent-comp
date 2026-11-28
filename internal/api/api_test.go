package api_test

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentdv1 "github.com/mondeep31/agent-comp/gen/go/agentcomp/agentd/v1"
	"github.com/mondeep31/agent-comp/internal/api"
	"github.com/mondeep31/agent-comp/internal/client"
	"github.com/mondeep31/agent-comp/internal/config"
	"github.com/mondeep31/agent-comp/internal/golden"
	"github.com/mondeep31/agent-comp/internal/guest/guesttest"
	"github.com/mondeep31/agent-comp/internal/metrics"
	"github.com/mondeep31/agent-comp/internal/sandbox"
)

type keys map[string]string

func (k keys) Authenticate(token string) (string, bool) {
	name, ok := k[token]
	return name, ok
}

type env struct {
	srv     *httptest.Server
	alice   *client.Client
	bob     *client.Client
	metrics *metrics.Metrics
}

func setup(t *testing.T, tweak func(*config.Limits, *api.Options)) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	limits := cfg.Limits
	opts := api.Options{
		Auth:              keys{"ac_alice": "alice", "ac_bob": "bob"},
		RequestsPerSecond: 1000,
		Burst:             1000,
		Metrics:           metrics.New(),
	}
	if tweak != nil {
		tweak(&limits, &opts)
	}
	src := guesttest.NewSource(dir)
	src.Displays = map[string]*agentdv1.Display{"desktop": {Width: 1280, Height: 800, Ready: true}}
	m, err := sandbox.New(sandbox.Options{
		Source: src,
		Images: map[string]sandbox.ImageSpec{
			"base": {Rootfs: "/images/base.ext4", Shape: golden.Shape{VCPU: cfg.Sandbox.VCPU, MemoryMiB: cfg.Sandbox.MemoryMiB, DiskMiB: cfg.Sandbox.DiskMiB}},
			"desktop": {
				Rootfs: "/images/desktop.ext4", Shape: golden.Shape{VCPU: 1, MemoryMiB: 1024, DiskMiB: 2048},
				Display: &config.Display{Width: 1280, Height: 800},
			},
		},
		DefaultImage: "base",
		Defaults:     cfg.Sandbox,
		Limits:       limits,
		SnapshotDir:  filepath.Join(dir, "snapshots"),
	})
	if err != nil {
		t.Fatal(err)
	}
	opts.Manager = m
	srv := httptest.NewServer(api.New(opts).Handler())
	t.Cleanup(func() {
		srv.Close()
		m.Close(context.Background())
	})
	return &env{
		srv:     srv,
		alice:   client.New(srv.URL, "ac_alice", srv.Client()),
		bob:     client.New(srv.URL, "ac_bob", srv.Client()),
		metrics: opts.Metrics,
	}
}

func TestAuth(t *testing.T) {
	e := setup(t, nil)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, e.srv.URL+"/healthz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(resp.Header.Get("X-Request-Id")) != 16 {
		t.Fatalf("healthz = %d, request id %q", resp.StatusCode, resp.Header.Get("X-Request-Id"))
	}

	for _, token := range []string{"", "ac_wrong"} {
		_, err := client.New(e.srv.URL, token, nil).List(t.Context())
		var ae *client.Error
		if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized {
			t.Fatalf("token %q: %v", token, err)
		}
	}
}

func TestRateLimit(t *testing.T) {
	e := setup(t, func(_ *config.Limits, o *api.Options) { o.RequestsPerSecond, o.Burst = 0.001, 2 })
	for range 2 {
		if _, err := e.alice.List(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.alice.List(t.Context()); !errors.Is(err, sandbox.ErrQuota) {
		t.Fatalf("third request: %v", err)
	}
	// Limits are per key.
	if _, err := e.bob.List(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycle(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	info, err := e.alice.Create(ctx, &api.CreateSandboxRequest{TTLSeconds: 600, Metadata: map[string]string{"task": "t1"}})
	if err != nil {
		t.Fatal(err)
	}
	if info.State != sandbox.StateRunning || info.Metadata["task"] != "t1" || time.Duration(info.IdleTimeout) != 15*time.Minute {
		t.Fatalf("created %+v", info)
	}
	if _, err := e.bob.Get(ctx, info.ID); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("bob sees alice's sandbox: %v", err)
	}
	if list, err := e.alice.List(ctx); err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	if got, err := e.alice.Pause(ctx, info.ID); err != nil || got.State != sandbox.StatePaused {
		t.Fatalf("Pause = %+v, %v", got, err)
	}
	if got, err := e.alice.Resume(ctx, info.ID); err != nil || got.State != sandbox.StateRunning {
		t.Fatalf("Resume = %+v, %v", got, err)
	}
	if err := e.alice.Delete(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.alice.Get(ctx, info.ID); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("Get after delete: %v", err)
	}

	rec := httptest.NewRecorder()
	e.metrics.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/metrics", nil))
	for _, want := range []string{
		`agentcomp_api_requests_total{code="201",method="POST",route="POST /v1/sandboxes"} 1`,
		`agentcomp_api_requests_total{code="404",method="GET",route="GET /v1/sandboxes/{id}"}`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

func TestCreateErrors(t *testing.T) {
	e := setup(t, func(l *config.Limits, _ *api.Options) { l.MaxSandboxesPerKey = 1 })
	ctx := t.Context()
	if _, err := e.alice.Create(ctx, &api.CreateSandboxRequest{VCPU: 64}); !errors.Is(err, sandbox.ErrInvalid) {
		t.Fatalf("vcpu: %v", err)
	}
	if _, err := e.alice.Create(ctx, &api.CreateSandboxRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.alice.Create(ctx, &api.CreateSandboxRequest{}); !errors.Is(err, sandbox.ErrQuota) {
		t.Fatalf("quota: %v", err)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+"/v1/sandboxes", strings.NewReader(`{"bogus": 1}`))
	req.Header.Set("Authorization", "Bearer ac_bob")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", resp.StatusCode)
	}
}

func TestExec(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	info, err := e.alice.Create(ctx, &api.CreateSandboxRequest{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.alice.Exec(ctx, info.ID, &sandbox.ExecRequest{Command: "echo hello world"})
	if err != nil || res.Stdout != "hello world\n" || res.ExitCode != 0 {
		t.Fatalf("echo = %+v, %v", res, err)
	}
	res, err = e.alice.Exec(ctx, info.ID, &sandbox.ExecRequest{Argv: []string{"exit", "3"}})
	if err != nil || res.ExitCode != 3 {
		t.Fatalf("exit 3 = %+v, %v", res, err)
	}
	res, err = e.alice.Exec(ctx, info.ID, &sandbox.ExecRequest{Argv: []string{"cat"}, Stdin: "piped"})
	if err != nil || res.Stdout != "piped" {
		t.Fatalf("cat = %+v, %v", res, err)
	}
	res, err = e.alice.Exec(ctx, info.ID, &sandbox.ExecRequest{Argv: []string{"cat"}, Stdin: strings.Repeat("x", 100), MaxOutputBytes: 10})
	if err != nil || res.Stdout != strings.Repeat("x", 10) || !res.StdoutTruncated {
		t.Fatalf("truncation = %+v, %v", res, err)
	}
	res, err = e.alice.Exec(ctx, info.ID, &sandbox.ExecRequest{Argv: []string{"sleep", "10"}, TimeoutSeconds: 0.05})
	if err != nil || !res.TimedOut {
		t.Fatalf("timeout = %+v, %v", res, err)
	}
	if _, err := e.alice.Exec(ctx, info.ID, &sandbox.ExecRequest{}); !errors.Is(err, sandbox.ErrInvalid) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := e.alice.Exec(ctx, info.ID, &sandbox.ExecRequest{Argv: []string{"nope"}}); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("missing binary: %v", err)
	}
	if _, err := e.bob.Exec(ctx, info.ID, &sandbox.ExecRequest{Command: "echo"}); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("bob exec: %v", err)
	}
}

func TestExecStream(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	info, err := e.alice.Create(ctx, &api.CreateSandboxRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	exit, err := e.alice.ExecStream(ctx, info.ID, &sandbox.ExecRequest{Argv: []string{"cat"}}, strings.NewReader("streamed input"), &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "streamed input" || exit.ExitCode == nil || *exit.ExitCode != 0 {
		t.Fatalf("out = %q, exit = %+v", out.String(), exit)
	}
	if _, err := e.bob.ExecStream(ctx, info.ID, &sandbox.ExecRequest{Command: "echo"}, nil, nil, nil); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("bob stream: %v", err)
	}
	if _, err := client.New(e.srv.URL, "bad", nil).ExecStream(ctx, info.ID, &sandbox.ExecRequest{Command: "echo"}, nil, nil, nil); err == nil {
		t.Fatal("unauthenticated stream succeeded")
	}
}

func TestFiles(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	info, err := e.alice.Create(ctx, &api.CreateSandboxRequest{})
	if err != nil {
		t.Fatal(err)
	}
	fe, err := e.alice.WriteFile(ctx, info.ID, "/work/a.txt", 0o600, strings.NewReader("contents"))
	if err != nil || fe.Size != 8 || fe.Mode != "0600" {
		t.Fatalf("WriteFile = %+v, %v", fe, err)
	}
	var buf bytes.Buffer
	if err := e.alice.ReadFile(ctx, info.ID, "/work/a.txt", &buf); err != nil || buf.String() != "contents" {
		t.Fatalf("ReadFile = %q, %v", buf.String(), err)
	}
	if err := e.alice.ReadFile(ctx, info.ID, "/work/missing", io.Discard); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	entries, err := e.alice.ListDir(ctx, info.ID, "/work")
	if err != nil || len(entries) != 1 || entries[0].Name != "a.txt" || entries[0].Type != "file" {
		t.Fatalf("ListDir = %+v, %v", entries, err)
	}
	st, err := e.alice.Stat(ctx, info.ID, "/work")
	if err != nil || st.Type != "dir" {
		t.Fatalf("Stat = %+v, %v", st, err)
	}
	if _, err := e.alice.Stat(ctx, info.ID, "relative"); !errors.Is(err, sandbox.ErrInvalid) {
		t.Fatalf("relative path: %v", err)
	}
}

func TestWriteFileLimit(t *testing.T) {
	e := setup(t, func(l *config.Limits, _ *api.Options) { l.MaxFileBytes = 16 })
	ctx := t.Context()
	info, err := e.alice.Create(ctx, &api.CreateSandboxRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.alice.WriteFile(ctx, info.ID, "/f", 0, strings.NewReader(strings.Repeat("y", 16))); err != nil {
		t.Fatalf("at limit: %v", err)
	}
	if _, err := e.alice.WriteFile(ctx, info.ID, "/f", 0, strings.NewReader(strings.Repeat("y", 17))); !errors.Is(err, sandbox.ErrTooLarge) {
		t.Fatalf("over limit: %v", err)
	}
}

func TestSnapshots(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	info, err := e.alice.Create(ctx, &api.CreateSandboxRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.alice.WriteFile(ctx, info.ID, "/state", 0, strings.NewReader("saved")); err != nil {
		t.Fatal(err)
	}
	snap, err := e.alice.Snapshot(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if list, err := e.bob.ListSnapshots(ctx); err != nil || len(list) != 0 {
		t.Fatalf("bob snapshots = %v, %v", list, err)
	}
	fork, err := e.alice.Create(ctx, &api.CreateSandboxRequest{FromSnapshot: snap.ID})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := e.alice.ReadFile(ctx, fork.ID, "/state", &buf); err != nil || buf.String() != "saved" {
		t.Fatalf("fork state = %q, %v", buf.String(), err)
	}
	if err := e.alice.DeleteSnapshot(ctx, snap.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.alice.ListSnapshots(ctx); len(list) != 0 {
		t.Fatalf("snapshots after delete = %v", list)
	}
}

func TestScreenAPI(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	imgs, err := e.alice.Images(ctx)
	if err != nil || len(imgs) != 2 || imgs[1].Name != "desktop" || imgs[1].Display == nil {
		t.Fatalf("images = %+v, %v", imgs, err)
	}
	desk, err := e.alice.Create(ctx, &api.CreateSandboxRequest{Image: "desktop"})
	if err != nil {
		t.Fatal(err)
	}
	if desk.Display == nil || desk.MemoryMiB != 1024 {
		t.Fatalf("desktop = %+v", desk)
	}

	img, err := e.alice.Screenshot(ctx, desk.ID, sandbox.ScreenshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(img.Data))
	if err != nil || cfg.Width != 1280 || cfg.Height != 800 || img.MIMEType != "image/png" || img.Width != 1280 {
		t.Fatalf("png %+v %v (%s %dx%d)", cfg, err, img.MIMEType, img.Width, img.Height)
	}
	jpg, err := e.alice.Screenshot(ctx, desk.ID, sandbox.ScreenshotOptions{Format: "jpeg", Region: []int{0, 0, 320, 200}})
	if err != nil || jpg.MIMEType != "image/jpeg" || jpg.Width != 320 {
		t.Fatalf("jpeg = %s %d, %v", jpg.MIMEType, jpg.Width, err)
	}

	res, err := e.alice.Computer(ctx, desk.ID, &sandbox.ComputerAction{Action: "left_click", Coordinate: []int{10, 20}, Screenshot: true})
	if err != nil || res.CursorX != 10 || res.Image == nil || res.Image.MIMEType != "image/png" {
		t.Fatalf("click = %+v, %v", res, err)
	}

	var ce *client.Error
	_, err = e.alice.Computer(ctx, desk.ID, &sandbox.ComputerAction{Action: "fly"})
	if !errors.As(err, &ce) || ce.Status != http.StatusBadRequest {
		t.Fatalf("bad action: %v", err)
	}
	_, err = e.bob.Screenshot(ctx, desk.ID, sandbox.ScreenshotOptions{})
	if !errors.As(err, &ce) || ce.Status != http.StatusNotFound {
		t.Fatalf("bob: %v", err)
	}
	base, err := e.alice.Create(ctx, &api.CreateSandboxRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.alice.Screenshot(ctx, base.ID, sandbox.ScreenshotOptions{})
	if !errors.As(err, &ce) || ce.Status != http.StatusBadRequest {
		t.Fatalf("headless: %v", err)
	}
	_, err = e.alice.Screenshot(ctx, desk.ID, sandbox.ScreenshotOptions{Region: []int{0, 0, 9999, 10}})
	if !errors.As(err, &ce) || ce.Status != http.StatusBadRequest {
		t.Fatalf("bad region: %v", err)
	}
}
