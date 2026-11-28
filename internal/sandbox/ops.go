package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	agentdv1 "github.com/mondeep31/agent-comp/gen/go/agentcomp/agentd/v1"
	"github.com/mondeep31/agent-comp/internal/guest"
)

// DefaultMaxOutputBytes caps each captured stream of a synchronous exec.
const DefaultMaxOutputBytes = 1 << 20

// ExecRequest runs one command. Exactly one of Argv or Command is set;
// Command runs through /bin/sh -c.
type ExecRequest struct {
	Argv    []string          `json:"argv,omitempty"`
	Command string            `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	// TimeoutSeconds of 0 means the configured maximum.
	TimeoutSeconds float64 `json:"timeout_seconds,omitempty"`
	Stdin          string  `json:"stdin,omitempty"`
	// MaxOutputBytes caps stdout and stderr each; 0 means DefaultMaxOutputBytes.
	MaxOutputBytes int `json:"max_output_bytes,omitempty"`
}

type ExecResult struct {
	ExitCode        int32  `json:"exit_code"`
	Signal          int32  `json:"signal,omitempty"`
	TimedOut        bool   `json:"timed_out"`
	DurationMs      uint64 `json:"duration_ms"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
}

// ExecStart validates req and converts it to the guest message.
func (m *Manager) ExecStart(req *ExecRequest, stdin bool) (*agentdv1.ExecStart, error) {
	argv := req.Argv
	switch {
	case len(argv) > 0 && req.Command != "":
		return nil, invalid("set either argv or command, not both")
	case req.Command != "":
		argv = []string{"/bin/sh", "-c", req.Command}
	case len(argv) == 0:
		return nil, invalid("argv or command is required")
	}
	if req.Cwd != "" && !path.IsAbs(req.Cwd) {
		return nil, invalid("cwd must be absolute")
	}
	for k := range req.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return nil, invalid("invalid env var name %q", k)
		}
	}
	maxTimeout := m.opts.Limits.MaxExecTimeout
	timeout := time.Duration(req.TimeoutSeconds * float64(time.Second))
	if req.TimeoutSeconds < 0 || timeout > maxTimeout {
		return nil, invalid("timeout_seconds must be in [0, %d]", int(maxTimeout.Seconds()))
	}
	if timeout == 0 {
		timeout = maxTimeout
	}
	return &agentdv1.ExecStart{
		Argv:      argv,
		Env:       req.Env,
		Cwd:       req.Cwd,
		TimeoutMs: uint32(timeout.Milliseconds()), //nolint:gosec // bounded by MaxExecTimeout
		Stdin:     stdin,
	}, nil
}

// Exec runs a command to completion and captures its output.
func (m *Manager) Exec(ctx context.Context, owner, id string, req *ExecRequest) (*ExecResult, error) {
	start, err := m.ExecStart(req, req.Stdin != "")
	if err != nil {
		return nil, err
	}
	limit := req.MaxOutputBytes
	if limit <= 0 || limit > DefaultMaxOutputBytes {
		limit = DefaultMaxOutputBytes
	}
	c, done, err := m.Use(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	defer done()
	var stdin io.Reader
	if req.Stdin != "" {
		stdin = strings.NewReader(req.Stdin)
	}
	stdout, stderr := NewCappedBuffer(limit), NewCappedBuffer(limit)
	exit, err := c.Run(ctx, start, stdin, stdout, stderr)
	if err != nil {
		return nil, guestErr(err)
	}
	return &ExecResult{
		ExitCode:        exit.GetExitCode(),
		Signal:          exit.GetSignal(),
		TimedOut:        exit.GetTimedOut(),
		DurationMs:      exit.GetDurationMs(),
		Stdout:          stdout.String(),
		Stderr:          stderr.String(),
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
	}, nil
}

// StartExec starts an interactive command with stdin attached. Call done once
// the process has finished; until then the sandbox counts as active.
func (m *Manager) StartExec(ctx context.Context, owner, id string, req *ExecRequest, stdout, stderr io.Writer) (p *guest.Process, done func(), err error) {
	start, err := m.ExecStart(req, true)
	if err != nil {
		return nil, nil, err
	}
	c, done, err := m.Use(ctx, owner, id)
	if err != nil {
		return nil, nil, err
	}
	p, err = c.Start(ctx, start, stdout, stderr)
	if err != nil {
		done()
		return nil, nil, guestErr(err)
	}
	return p, done, nil
}

// CappedBuffer keeps the first max bytes written and discards the rest.
type CappedBuffer struct {
	strings.Builder
	max       int
	truncated bool
}

func NewCappedBuffer(limit int) *CappedBuffer { return &CappedBuffer{max: limit} }

// Truncated reports whether any bytes were discarded.
func (c *CappedBuffer) Truncated() bool { return c.truncated }

func (c *CappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room < len(p) {
		c.truncated = true
		c.Builder.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return c.Builder.Write(p)
}

// FileEntry is a file's metadata.
type FileEntry struct {
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Size    uint64    `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"mtime"`
}

func entry(fi *agentdv1.FileInfo) FileEntry {
	var t string
	switch fi.GetType() {
	case agentdv1.FileType_FILE_TYPE_FILE:
		t = "file"
	case agentdv1.FileType_FILE_TYPE_DIR:
		t = "dir"
	case agentdv1.FileType_FILE_TYPE_SYMLINK:
		t = "symlink"
	case agentdv1.FileType_FILE_TYPE_OTHER, agentdv1.FileType_FILE_TYPE_UNSPECIFIED:
		t = "other"
	default:
		// The guest is untrusted: unknown types must not crash acd.
		t = "other"
	}
	return FileEntry{
		Path:    fi.GetPath(),
		Name:    fi.GetName(),
		Type:    t,
		Size:    fi.GetSize(),
		Mode:    fmt.Sprintf("%04o", fi.GetMode()&0o7777),
		ModTime: time.UnixMilli(fi.GetMtimeUnixMs()).UTC(),
	}
}

func checkPath(p string) error {
	if !path.IsAbs(p) || strings.ContainsRune(p, 0) {
		return invalid("path must be absolute")
	}
	return nil
}

func (m *Manager) Stat(ctx context.Context, owner, id, p string) (FileEntry, error) {
	if err := checkPath(p); err != nil {
		return FileEntry{}, err
	}
	c, done, err := m.Use(ctx, owner, id)
	if err != nil {
		return FileEntry{}, err
	}
	defer done()
	fi, err := c.Stat(ctx, p)
	if err != nil {
		return FileEntry{}, guestErr(err)
	}
	return entry(fi), nil
}

func (m *Manager) ListDir(ctx context.Context, owner, id, p string) ([]FileEntry, error) {
	if err := checkPath(p); err != nil {
		return nil, err
	}
	c, done, err := m.Use(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	defer done()
	infos, err := c.ListDir(ctx, p)
	if err != nil {
		return nil, guestErr(err)
	}
	out := make([]FileEntry, len(infos))
	for i, fi := range infos {
		out[i] = entry(fi)
	}
	return out, nil
}

// ReadFile streams a file to w. The limit is the configured max_file_bytes.
func (m *Manager) ReadFile(ctx context.Context, owner, id, p string, w io.Writer) (FileEntry, error) {
	if err := checkPath(p); err != nil {
		return FileEntry{}, err
	}
	c, done, err := m.Use(ctx, owner, id)
	if err != nil {
		return FileEntry{}, err
	}
	defer done()
	fi, err := c.ReadFile(ctx, p, uint64(m.opts.Limits.MaxFileBytes), w) //nolint:gosec // validated > 0
	if err != nil {
		return FileEntry{}, guestErr(err)
	}
	return entry(fi), nil
}

// WriteFile replaces (or appends to) a file, creating parent directories.
func (m *Manager) WriteFile(ctx context.Context, owner, id, p string, mode uint32, appendTo bool, r io.Reader) (FileEntry, error) {
	if err := checkPath(p); err != nil {
		return FileEntry{}, err
	}
	if mode > 0o7777 {
		return FileEntry{}, invalid("mode must be at most 07777")
	}
	c, done, err := m.Use(ctx, owner, id)
	if err != nil {
		return FileEntry{}, err
	}
	defer done()
	limited := &limitReader{r: r, left: m.opts.Limits.MaxFileBytes}
	fi, err := c.WriteFile(ctx, &agentdv1.WriteFile{Path: p, Mode: mode, CreateParents: true, Append: appendTo}, limited)
	if limited.exceeded {
		return FileEntry{}, fmt.Errorf("%w: file exceeds %d bytes", ErrTooLarge, m.opts.Limits.MaxFileBytes)
	}
	if err != nil {
		return FileEntry{}, guestErr(err)
	}
	return entry(fi), nil
}

type limitReader struct {
	r        io.Reader
	left     int64
	exceeded bool
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.left <= 0 {
		// Probe for one more byte to tell "exactly at the limit" from "over".
		var one [1]byte
		if n, _ := l.r.Read(one[:]); n > 0 {
			l.exceeded = true
			return 0, errTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	return n, err
}

var errTooLarge = errors.New("too large")

// guestErr maps agentd errors onto the service errors adapters understand.
func guestErr(err error) error {
	var ge *guest.Error
	if !errors.As(err, &ge) {
		return err
	}
	switch ge.Code {
	case agentdv1.ErrorCode_ERROR_CODE_NOT_FOUND:
		return fmt.Errorf("%w: %s", ErrNotFound, ge.Message)
	case agentdv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT:
		return fmt.Errorf("%w: %s", ErrInvalid, ge.Message)
	case agentdv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED:
		return fmt.Errorf("%w: %s", ErrPermission, ge.Message)
	case agentdv1.ErrorCode_ERROR_CODE_ALREADY_EXISTS:
		return fmt.Errorf("%w: %s", ErrConflict, ge.Message)
	case agentdv1.ErrorCode_ERROR_CODE_TOO_LARGE:
		return fmt.Errorf("%w: %s", ErrTooLarge, ge.Message)
	case agentdv1.ErrorCode_ERROR_CODE_UNAVAILABLE:
		return fmt.Errorf("%w: %s", ErrUnavailable, ge.Message)
	case agentdv1.ErrorCode_ERROR_CODE_INTERNAL, agentdv1.ErrorCode_ERROR_CODE_UNSPECIFIED:
		return err
	default:
		// The guest is untrusted: unknown codes must not crash acd.
		return err
	}
}
