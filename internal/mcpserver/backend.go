package mcpserver

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mondeep31/agent-comp/internal/api"
	"github.com/mondeep31/agent-comp/internal/client"
	"github.com/mondeep31/agent-comp/internal/sandbox"
)

// Backend is what the tools need. It is served in-process by the sandbox
// manager (acd's /v1/mcp endpoint) or remotely by the HTTP API (acctl mcp).
type Backend interface {
	Create(ctx context.Context, req *api.CreateSandboxRequest) (*sandbox.Info, error)
	List(ctx context.Context) ([]sandbox.Info, error)
	Delete(ctx context.Context, id string) error
	Exec(ctx context.Context, id string, req *sandbox.ExecRequest) (*sandbox.ExecResult, error)
	ReadFile(ctx context.Context, id, path string, w io.Writer) error
	WriteFile(ctx context.Context, id, path string, mode uint32, r io.Reader) (*sandbox.FileEntry, error)
	ListDir(ctx context.Context, id, path string) ([]sandbox.FileEntry, error)
	Snapshot(ctx context.Context, id string) (*sandbox.SnapshotInfo, error)
	Images(ctx context.Context) ([]sandbox.ImageInfo, error)
	Computer(ctx context.Context, id string, a *sandbox.ComputerAction) (*sandbox.ComputerResult, error)
}

// Resolver returns the backend for one tool call.
type Resolver func(req *mcp.CallToolRequest) (Backend, error)

var errUnauthenticated = errors.New("unauthenticated")

// Local serves tools from an in-process manager. The caller is the key the
// API's auth middleware stamped into api.OwnerHeader.
func Local(m *sandbox.Manager) Resolver {
	return func(req *mcp.CallToolRequest) (Backend, error) {
		if req.Extra == nil || req.Extra.Header.Get(api.OwnerHeader) == "" {
			return nil, errUnauthenticated
		}
		return &local{m: m, owner: req.Extra.Header.Get(api.OwnerHeader)}, nil
	}
}

type local struct {
	m     *sandbox.Manager
	owner string
}

func (l *local) Create(ctx context.Context, req *api.CreateSandboxRequest) (*sandbox.Info, error) {
	info, err := l.m.Create(ctx, req.ToService(l.owner))
	return &info, err
}

func (l *local) List(context.Context) ([]sandbox.Info, error) { return l.m.List(l.owner), nil }

func (l *local) Delete(ctx context.Context, id string) error { return l.m.Delete(ctx, l.owner, id) }

func (l *local) Exec(ctx context.Context, id string, req *sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	return l.m.Exec(ctx, l.owner, id, req)
}

func (l *local) ReadFile(ctx context.Context, id, path string, w io.Writer) error {
	_, err := l.m.ReadFile(ctx, l.owner, id, path, w)
	return err
}

func (l *local) WriteFile(ctx context.Context, id, path string, mode uint32, r io.Reader) (*sandbox.FileEntry, error) {
	fe, err := l.m.WriteFile(ctx, l.owner, id, path, mode, false, r)
	return &fe, err
}

func (l *local) ListDir(ctx context.Context, id, path string) ([]sandbox.FileEntry, error) {
	return l.m.ListDir(ctx, l.owner, id, path)
}

func (l *local) Snapshot(ctx context.Context, id string) (*sandbox.SnapshotInfo, error) {
	info, err := l.m.Snapshot(ctx, l.owner, id)
	return &info, err
}

func (l *local) Images(context.Context) ([]sandbox.ImageInfo, error) { return l.m.Images(), nil }

func (l *local) Computer(ctx context.Context, id string, a *sandbox.ComputerAction) (*sandbox.ComputerResult, error) {
	return l.m.Computer(ctx, l.owner, id, a)
}

// Remote serves tools through the HTTP API, for stdio clients.
func Remote(c *client.Client) Resolver {
	b := &remote{Client: c}
	return func(*mcp.CallToolRequest) (Backend, error) { return b, nil }
}

type remote struct{ *client.Client }

// Exec streams over the WebSocket API: plain HTTP responses through the
// tunnel are cut off after 100 s, WebSockets are kept alive.
func (r *remote) Exec(ctx context.Context, id string, req *sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	limit := req.MaxOutputBytes
	if limit <= 0 || limit > sandbox.DefaultMaxOutputBytes {
		limit = sandbox.DefaultMaxOutputBytes
	}
	var stdin io.Reader
	if req.Stdin != "" {
		stdin = strings.NewReader(req.Stdin)
	}
	stdout, stderr := sandbox.NewCappedBuffer(limit), sandbox.NewCappedBuffer(limit)
	exit, err := r.ExecStream(ctx, id, req, stdin, stdout, stderr)
	if err != nil {
		return nil, err
	}
	res := &sandbox.ExecResult{
		Signal:          exit.Signal,
		TimedOut:        exit.TimedOut,
		DurationMs:      exit.DurationMs,
		Stdout:          stdout.String(),
		Stderr:          stderr.String(),
		StdoutTruncated: stdout.Truncated(),
		StderrTruncated: stderr.Truncated(),
	}
	if exit.ExitCode != nil {
		res.ExitCode = *exit.ExitCode
	}
	return res, nil
}
