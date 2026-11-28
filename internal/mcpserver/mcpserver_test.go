package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	agentdv1 "github.com/mondeep31/agent-comp/gen/go/agentcomp/agentd/v1"
	"github.com/mondeep31/agent-comp/internal/api"
	"github.com/mondeep31/agent-comp/internal/client"
	"github.com/mondeep31/agent-comp/internal/config"
	"github.com/mondeep31/agent-comp/internal/golden"
	"github.com/mondeep31/agent-comp/internal/guest/guesttest"
	"github.com/mondeep31/agent-comp/internal/mcpserver"
	"github.com/mondeep31/agent-comp/internal/sandbox"
)

type keys map[string]string

func (k keys) Authenticate(token string) (string, bool) {
	name, ok := k[token]
	return name, ok
}

func newAPI(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
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
		Limits:       cfg.Limits,
		SnapshotDir:  filepath.Join(dir, "snapshots"),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := api.New(api.Options{
		Manager:           m,
		Auth:              keys{"ac_alice": "alice", "ac_bob": "bob"},
		RequestsPerSecond: 1000,
		Burst:             1000,
		Extra:             map[string]http.Handler{"/v1/mcp": mcpserver.HTTPHandler(mcpserver.New(mcpserver.Local(m), "test", nil))},
	}).Handler()
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		m.Close(context.Background())
	})
	return srv
}

// tunnel makes requests look like cloudflared's: loopback connection, public
// Host header, bearer key.
type tunnel struct {
	token string
	next  http.RoundTripper
}

func (tr tunnel) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = "agents.example.com"
	if tr.token != "" {
		r.Header.Set("Authorization", "Bearer "+tr.token)
	}
	r.Header.Set(api.OwnerHeader, "spoofed")
	return tr.next.RoundTrip(r)
}

func connectHTTP(t *testing.T, srv *httptest.Server, token string) (*mcp.ClientSession, error) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := c.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/v1/mcp",
		HTTPClient: &http.Client{Transport: tunnel{token: token, next: http.DefaultTransport}},
		MaxRetries: -1,
	}, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if out != nil && !res.IsError {
		data, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s: decoding %s: %v", name, data, err)
		}
	}
	return res
}

func textOf(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestToolsOverHTTP(t *testing.T) {
	srv := newAPI(t)
	cs, err := connectHTTP(t, srv, "ac_alice")
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for tool, err := range cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	want := []string{
		"computer_action", "create_computer", "destroy_computer", "list_computers", "list_files",
		"list_images", "read_file", "run_command", "snapshot_computer", "write_file",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v", names)
	}

	var info sandbox.Info
	res := call(t, cs, "create_computer", map[string]any{"env": map[string]string{"GREETING": "hi"}}, &info)
	if res.IsError || info.ID == "" || info.Owner != "alice" {
		t.Fatalf("create = %s, %+v", textOf(res), info)
	}
	id := info.ID

	var exec sandbox.ExecResult
	res = call(t, cs, "run_command", map[string]any{"computer_id": id, "command": "env GREETING"}, &exec)
	if res.IsError || exec.Stdout != "hi\n" || !strings.Contains(textOf(res), "Exit code 0") {
		t.Fatalf("run = %s, %+v", textOf(res), exec)
	}
	res = call(t, cs, "run_command", map[string]any{"computer_id": id, "command": "exit 2"}, &exec)
	if res.IsError || exec.ExitCode != 2 {
		t.Fatalf("exit 2 = %s", textOf(res))
	}

	call(t, cs, "write_file", map[string]any{"computer_id": id, "path": "/work/a.py", "content": "print(1)\n"}, nil)
	var file struct {
		Encoding, Content string
	}
	res = call(t, cs, "read_file", map[string]any{"computer_id": id, "path": "/work/a.py"}, &file)
	if res.IsError || file.Encoding != "utf8" || file.Content != "print(1)\n" || textOf(res) != "print(1)\n" {
		t.Fatalf("read = %s, %+v", textOf(res), file)
	}

	binary := []byte{0xff, 0x00, 0xfe}
	call(t, cs, "write_file", map[string]any{"computer_id": id, "path": "/work/b.bin", "content": base64.StdEncoding.EncodeToString(binary), "encoding": "base64"}, nil)
	call(t, cs, "read_file", map[string]any{"computer_id": id, "path": "/work/b.bin"}, &file)
	if file.Encoding != "base64" || file.Content != base64.StdEncoding.EncodeToString(binary) {
		t.Fatalf("binary read = %+v", file)
	}

	var dir struct{ Entries []sandbox.FileEntry }
	res = call(t, cs, "list_files", map[string]any{"computer_id": id, "path": "/work"}, &dir)
	if res.IsError || len(dir.Entries) != 2 || !strings.Contains(textOf(res), "a.py") {
		t.Fatalf("list_files = %s", textOf(res))
	}

	var snap sandbox.SnapshotInfo
	call(t, cs, "snapshot_computer", map[string]any{"computer_id": id}, &snap)
	var fork sandbox.Info
	call(t, cs, "create_computer", map[string]any{"from_snapshot": snap.ID}, &fork)
	call(t, cs, "read_file", map[string]any{"computer_id": fork.ID, "path": "/work/a.py"}, &file)
	if file.Content != "print(1)\n" {
		t.Fatalf("fork read = %+v", file)
	}

	var list struct{ Computers []sandbox.Info }
	call(t, cs, "list_computers", nil, &list)
	if len(list.Computers) != 2 {
		t.Fatalf("list = %+v", list)
	}

	res = call(t, cs, "run_command", map[string]any{"computer_id": id, "command": "nope"}, nil)
	if !res.IsError || !strings.Contains(textOf(res), "not found") {
		t.Fatalf("missing binary = %s", textOf(res))
	}
	res = call(t, cs, "read_file", map[string]any{"computer_id": id, "path": "relative"}, nil)
	if !res.IsError {
		t.Fatal("relative path accepted")
	}

	// Another key cannot see or touch alice's computers, even on its own session.
	bob, err := connectHTTP(t, srv, "ac_bob")
	if err != nil {
		t.Fatal(err)
	}
	res = call(t, bob, "run_command", map[string]any{"computer_id": id, "command": "echo pwned"}, nil)
	if !res.IsError || !strings.Contains(textOf(res), "not found") {
		t.Fatalf("bob ran on alice's computer: %s", textOf(res))
	}
	call(t, bob, "list_computers", nil, &list)
	if len(list.Computers) != 0 {
		t.Fatalf("bob lists %+v", list)
	}

	for _, c := range []string{id, fork.ID} {
		if res := call(t, cs, "destroy_computer", map[string]any{"computer_id": c}, nil); res.IsError {
			t.Fatalf("destroy: %s", textOf(res))
		}
	}
}

func imagesOf(res *mcp.CallToolResult) []*mcp.ImageContent {
	var out []*mcp.ImageContent
	for _, c := range res.Content {
		if ic, ok := c.(*mcp.ImageContent); ok {
			out = append(out, ic)
		}
	}
	return out
}

func TestComputerActionTool(t *testing.T) {
	srv := newAPI(t)
	cs, err := connectHTTP(t, srv, "ac_alice")
	if err != nil {
		t.Fatal(err)
	}
	res := call(t, cs, "list_images", nil, nil)
	if !strings.Contains(textOf(res), "desktop: 1 vCPU, 1024 MiB, 1280x800 screen") {
		t.Fatalf("list_images = %s", textOf(res))
	}

	var info sandbox.Info
	res = call(t, cs, "create_computer", map[string]any{"image": "desktop"}, &info)
	if res.IsError || !strings.Contains(textOf(res), "1280x800 screen") {
		t.Fatalf("create = %s", textOf(res))
	}

	res = call(t, cs, "computer_action", map[string]any{"computer_id": info.ID, "action": "screenshot"}, nil)
	imgs := imagesOf(res)
	if res.IsError || len(imgs) != 1 || imgs[0].MIMEType != "image/png" {
		t.Fatalf("screenshot = %s, %d images", textOf(res), len(imgs))
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(imgs[0].Data))
	if err != nil || cfg.Width != 1280 {
		t.Fatalf("decoded %+v, %v", cfg, err)
	}

	var out struct{ CursorX, CursorY int }
	res = call(t, cs, "computer_action", map[string]any{"computer_id": info.ID, "action": "left_click", "coordinate": []int{100, 200}}, &out)
	if res.IsError || len(imagesOf(res)) != 1 || !strings.Contains(textOf(res), "X=100, Y=200") {
		t.Fatalf("click = %s", textOf(res))
	}
	res = call(t, cs, "computer_action", map[string]any{"computer_id": info.ID, "action": "cursor_position"}, nil)
	if res.IsError || len(imagesOf(res)) != 0 || !strings.Contains(textOf(res), "X=100, Y=200") {
		t.Fatalf("cursor_position = %s", textOf(res))
	}
	res = call(t, cs, "computer_action", map[string]any{"computer_id": info.ID, "action": "type", "text": "hi", "screenshot": false}, nil)
	if res.IsError || len(imagesOf(res)) != 0 {
		t.Fatalf("type without screenshot = %s", textOf(res))
	}

	res = call(t, cs, "computer_action", map[string]any{"computer_id": info.ID, "action": "teleport"}, nil)
	if !res.IsError || !strings.Contains(textOf(res), "unknown action") {
		t.Fatalf("bad action = %s", textOf(res))
	}
	bob, err := connectHTTP(t, srv, "ac_bob")
	if err != nil {
		t.Fatal(err)
	}
	res = call(t, bob, "computer_action", map[string]any{"computer_id": info.ID, "action": "screenshot"}, nil)
	if !res.IsError || !strings.Contains(textOf(res), "not found") {
		t.Fatalf("bob saw alice's screen: %s", textOf(res))
	}
	var base sandbox.Info
	call(t, cs, "create_computer", nil, &base)
	res = call(t, cs, "computer_action", map[string]any{"computer_id": base.ID, "action": "screenshot"}, nil)
	if !res.IsError || !strings.Contains(textOf(res), "no display") {
		t.Fatalf("headless = %s", textOf(res))
	}
}

func TestHTTPRequiresKey(t *testing.T) {
	srv := newAPI(t)
	if _, err := connectHTTP(t, srv, ""); err == nil {
		t.Fatal("connected without a key")
	}
	if _, err := connectHTTP(t, srv, "ac_wrong"); err == nil {
		t.Fatal("connected with a bad key")
	}
}

// TestRemoteBackend covers acctl mcp: stdio tools backed by the HTTP API.
func TestRemoteBackend(t *testing.T) {
	srv := newAPI(t)
	server := mcpserver.New(mcpserver.Remote(client.New(srv.URL, "ac_alice", srv.Client())), "test", nil)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	var info sandbox.Info
	call(t, cs, "create_computer", nil, &info)
	var exec sandbox.ExecResult
	res := call(t, cs, "run_command", map[string]any{"computer_id": info.ID, "command": "cat", "stdin": "via websocket"}, &exec)
	if res.IsError || exec.Stdout != "via websocket" {
		t.Fatalf("remote run = %s, %+v", textOf(res), exec)
	}
	res = call(t, cs, "read_file", map[string]any{"computer_id": info.ID, "path": "/missing"}, nil)
	if !res.IsError || !strings.Contains(textOf(res), "not_found") {
		t.Fatalf("remote missing file = %s", textOf(res))
	}
}
