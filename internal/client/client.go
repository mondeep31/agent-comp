// Package client is a Go client for the agent-comp HTTP API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/mondeep31/agent-comp/internal/api"
	"github.com/mondeep31/agent-comp/internal/sandbox"
)

type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client for the API at base, e.g. https://ac.example.com.
func New(base, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{base: strings.TrimSuffix(base, "/"), token: token, http: hc}
}

// Error is a non-2xx API response. It unwraps to the matching sandbox error
// (sandbox.ErrNotFound, ...) so callers can use errors.Is.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("api: %s (%d): %s", e.Code, e.Status, e.Message) }

func (e *Error) Unwrap() error {
	switch e.Code {
	case "not_found":
		return sandbox.ErrNotFound
	case "invalid_argument":
		return sandbox.ErrInvalid
	case "quota_exceeded", "rate_limited":
		return sandbox.ErrQuota
	case "conflict":
		return sandbox.ErrConflict
	case "permission_denied":
		return sandbox.ErrPermission
	case "too_large":
		return sandbox.ErrTooLarge
	case "unavailable":
		return sandbox.ErrClosed
	default:
		return nil
	}
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var eb api.ErrorBody
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(data, &eb) != nil || eb.Error.Code == "" {
			eb.Error = api.ErrorDetail{Code: "http_" + strconv.Itoa(resp.StatusCode), Message: strings.TrimSpace(string(data))}
		}
		return nil, &Error{Status: resp.StatusCode, Code: eb.Error.Code, Message: eb.Error.Message}
	}
	return resp, nil
}

func (c *Client) json(ctx context.Context, method, path string, q url.Values, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(data), "application/json"
	}
	resp, err := c.do(ctx, method, path, q, body, ct)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func sb(id string, rest ...string) string {
	return "/v1/sandboxes/" + url.PathEscape(id) + strings.Join(rest, "")
}

func (c *Client) Create(ctx context.Context, req *api.CreateSandboxRequest) (*sandbox.Info, error) {
	var out sandbox.Info
	return &out, c.json(ctx, http.MethodPost, "/v1/sandboxes", nil, req, &out)
}

func (c *Client) List(ctx context.Context) ([]sandbox.Info, error) {
	var out api.SandboxList
	return out.Sandboxes, c.json(ctx, http.MethodGet, "/v1/sandboxes", nil, nil, &out)
}

// Images lists the server's images.
func (c *Client) Images(ctx context.Context) ([]sandbox.ImageInfo, error) {
	var out api.ImageList
	return out.Images, c.json(ctx, http.MethodGet, "/v1/images", nil, nil, &out)
}

func (c *Client) Get(ctx context.Context, id string) (*sandbox.Info, error) {
	var out sandbox.Info
	return &out, c.json(ctx, http.MethodGet, sb(id), nil, nil, &out)
}

func (c *Client) Delete(ctx context.Context, id string) error {
	return c.json(ctx, http.MethodDelete, sb(id), nil, nil, nil)
}

func (c *Client) Pause(ctx context.Context, id string) (*sandbox.Info, error) {
	var out sandbox.Info
	return &out, c.json(ctx, http.MethodPost, sb(id, "/pause"), nil, nil, &out)
}

func (c *Client) Resume(ctx context.Context, id string) (*sandbox.Info, error) {
	var out sandbox.Info
	return &out, c.json(ctx, http.MethodPost, sb(id, "/resume"), nil, nil, &out)
}

func (c *Client) Snapshot(ctx context.Context, id string) (*sandbox.SnapshotInfo, error) {
	var out sandbox.SnapshotInfo
	return &out, c.json(ctx, http.MethodPost, sb(id, "/snapshot"), nil, nil, &out)
}

func (c *Client) ListSnapshots(ctx context.Context) ([]sandbox.SnapshotInfo, error) {
	var out api.SnapshotList
	return out.Snapshots, c.json(ctx, http.MethodGet, "/v1/snapshots", nil, nil, &out)
}

func (c *Client) DeleteSnapshot(ctx context.Context, id string) error {
	return c.json(ctx, http.MethodDelete, "/v1/snapshots/"+url.PathEscape(id), nil, nil, nil)
}

func (c *Client) Exec(ctx context.Context, id string, req *sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	var out sandbox.ExecResult
	return &out, c.json(ctx, http.MethodPost, sb(id, "/exec"), nil, req, &out)
}

func (c *Client) Stat(ctx context.Context, id, path string) (*sandbox.FileEntry, error) {
	var out sandbox.FileEntry
	return &out, c.json(ctx, http.MethodGet, sb(id, "/stat"), url.Values{"path": {path}}, nil, &out)
}

func (c *Client) ListDir(ctx context.Context, id, path string) ([]sandbox.FileEntry, error) {
	var out api.DirListing
	return out.Entries, c.json(ctx, http.MethodGet, sb(id, "/ls"), url.Values{"path": {path}}, nil, &out)
}

// ReadFile copies a file's contents to w.
func (c *Client) ReadFile(ctx context.Context, id, path string, w io.Writer) error {
	resp, err := c.do(ctx, http.MethodGet, sb(id, "/files"), url.Values{"path": {path}}, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}

// WriteFile replaces a file (mode 0 keeps the server default of 0644).
func (c *Client) WriteFile(ctx context.Context, id, path string, mode uint32, r io.Reader) (*sandbox.FileEntry, error) {
	q := url.Values{"path": {path}}
	if mode != 0 {
		q.Set("mode", strconv.FormatUint(uint64(mode), 8))
	}
	resp, err := c.do(ctx, http.MethodPut, sb(id, "/files"), q, r, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out sandbox.FileEntry
	return &out, json.NewDecoder(resp.Body).Decode(&out)
}

// ExecStream runs a command over the WebSocket API, streaming stdin (if
// non-nil) and output. It returns the final "exit" message.
func (c *Client) ExecStream(ctx context.Context, id string, req *sandbox.ExecRequest, stdin io.Reader, stdout, stderr io.Writer) (*api.WSMessage, error) {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	u := strings.Replace(c.base, "http", "ws", 1) + sb(id, "/exec/ws")
	conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{ //nolint:bodyclose // the library owns the handshake body
		HTTPClient: c.http,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.token}},
	})
	if err != nil {
		if resp != nil && resp.StatusCode >= 300 {
			return nil, &Error{Status: resp.StatusCode, Code: "http_" + strconv.Itoa(resp.StatusCode), Message: err.Error()}
		}
		return nil, err
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(16 << 20)

	start := api.WSMessage{
		Type: api.WSStart, Argv: req.Argv, Command: req.Command, Env: req.Env, Cwd: req.Cwd, TimeoutSeconds: req.TimeoutSeconds,
	}
	if err := wsjson.Write(ctx, conn, start); err != nil {
		return nil, err
	}
	started := false
	for {
		var m api.WSMessage
		if err := wsjson.Read(ctx, conn, &m); err != nil {
			return nil, fmt.Errorf("exec stream: %w", err)
		}
		switch m.Type {
		case api.WSStarted:
			if !started && stdin != nil {
				go pumpStdin(ctx, conn, stdin)
			}
			started = true
		case api.WSStdout:
			if _, err := stdout.Write(m.Data); err != nil {
				return nil, err
			}
		case api.WSStderr:
			if _, err := stderr.Write(m.Data); err != nil {
				return nil, err
			}
		case api.WSExit:
			conn.Close(websocket.StatusNormalClosure, "")
			return &m, nil
		case api.WSError:
			e := &Error{Code: "unknown"}
			if m.Error != nil {
				e.Code, e.Message = m.Error.Code, m.Error.Message
			}
			return nil, e
		default:
			return nil, fmt.Errorf("exec stream: unexpected message %q", m.Type)
		}
	}
}

func pumpStdin(ctx context.Context, conn *websocket.Conn, r io.Reader) {
	buf := make([]byte, 64<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if wsjson.Write(ctx, conn, api.WSMessage{Type: api.WSStdin, Data: buf[:n]}) != nil {
				return
			}
		}
		if errors.Is(err, io.EOF) {
			_ = wsjson.Write(ctx, conn, api.WSMessage{Type: api.WSStdinEOF})
			return
		}
		if err != nil {
			return
		}
	}
}
