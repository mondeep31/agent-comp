// Package mcpserver exposes sandboxes to agents as MCP tools ("computers").
package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mondeep31/agent-comp/internal/api"
	"github.com/mondeep31/agent-comp/internal/sandbox"
)

const (
	// maxReadBytes keeps read_file results small enough for a model's context.
	maxReadBytes = 1 << 20
	// defaultCommandTimeout applies when run_command gets no timeout.
	defaultCommandTimeout = 120
	keepAlive             = 25 * time.Second
	progressInterval      = 15 * time.Second
)

const instructions = `Each computer is an isolated Linux microVM with a shell, its own filesystem and outbound internet access (no access to private networks).
Create one with create_computer, then use run_command, read_file, write_file and list_files with its computer_id.
Computers pause when idle and resume automatically on the next call; they are destroyed when their TTL expires.
Use snapshot_computer to save a computer's full state and create_computer with from_snapshot to fork it.
Call destroy_computer when you are done.
Images with a display (see list_images, e.g. "desktop") also have a screen with a browser: drive it with computer_action (screenshot, left_click, type, key, scroll, ...). Coordinates are screen pixels from the top left.`

// New builds an MCP server whose tools act through resolve.
func New(resolve Resolver, version string, log *slog.Logger) *mcp.Server {
	if log == nil {
		log = slog.Default()
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "agent-comp", Title: "Agent computers", Version: version}, &mcp.ServerOptions{
		Instructions: instructions,
		KeepAlive:    keepAlive,
		Logger:       log,
	})
	t := &tools{resolve: resolve, log: log.With("component", "mcp")}
	t.register(s)
	return s
}

// HTTPHandler serves s over streamable HTTP. It must sit behind the API's
// auth middleware.
func HTTPHandler(s *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{
		// The SDK's DNS-rebinding guard rejects loopback connections with a
		// public Host header, which is exactly how cloudflared delivers
		// requests. Every request already needs a bearer key a rebinding
		// page cannot have, so the guard adds nothing here.
		DisableLocalhostProtection: true,
		SessionTimeout:             time.Hour,
	})
}

type tools struct {
	resolve Resolver
	log     *slog.Logger
}

// publicError turns err into a message safe to show the model.
func (t *tools) publicError(err error) error {
	for _, known := range []error{
		sandbox.ErrNotFound, sandbox.ErrInvalid, sandbox.ErrQuota, sandbox.ErrConflict,
		sandbox.ErrPermission, sandbox.ErrTooLarge, sandbox.ErrClosed, sandbox.ErrUnavailable,
		errUnauthenticated, errReadTooLarge,
		context.DeadlineExceeded, context.Canceled,
	} {
		if errors.Is(err, known) {
			return err
		}
	}
	t.log.Error("tool failed", "err", err)
	return errors.New("internal error")
}

func text(format string, args ...any) []mcp.Content {
	return []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}
}

func boolPtr(b bool) *bool { return &b }

// handler adapts a backend-aware function to the SDK's typed tool handler.
func handler[In, Out any](t *tools, f func(ctx context.Context, req *mcp.CallToolRequest, b Backend, in In) (*mcp.CallToolResult, Out, error)) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var zero Out
		b, err := t.resolve(req)
		if err != nil {
			return nil, zero, t.publicError(err)
		}
		res, out, err := f(ctx, req, b, in)
		if err != nil {
			return nil, zero, t.publicError(err)
		}
		return res, out, nil
	}
}

type CreateInput struct {
	Image              string            `json:"image,omitempty" jsonschema:"image name (see list_images; e.g. desktop for a computer with a screen and browser); omit for the default image"`
	VCPU               int               `json:"vcpu,omitempty" jsonschema:"number of vCPUs; omit for the default"`
	MemoryMiB          int               `json:"memory_mib,omitempty" jsonschema:"memory in MiB; omit for the default"`
	TTLMinutes         int               `json:"ttl_minutes,omitempty" jsonschema:"minutes until the computer is destroyed; omit for the default"`
	IdleTimeoutMinutes int               `json:"idle_timeout_minutes,omitempty" jsonschema:"minutes of inactivity before the computer pauses"`
	FromSnapshot       string            `json:"from_snapshot,omitempty" jsonschema:"snapshot id to fork instead of starting fresh"`
	Env                map[string]string `json:"env,omitempty" jsonschema:"environment variables for every command"`
}

type ComputerInput struct {
	ComputerID string `json:"computer_id" jsonschema:"id returned by create_computer"`
}

type RunInput struct {
	ComputerID     string            `json:"computer_id" jsonschema:"id returned by create_computer"`
	Command        string            `json:"command" jsonschema:"shell command, run with /bin/sh -c"`
	Cwd            string            `json:"cwd,omitempty" jsonschema:"absolute working directory; defaults to /root"`
	Env            map[string]string `json:"env,omitempty" jsonschema:"extra environment variables"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" jsonschema:"kill the command after this many seconds (default 120)"`
	Stdin          string            `json:"stdin,omitempty" jsonschema:"text to pipe to the command's stdin"`
}

type PathInput struct {
	ComputerID string `json:"computer_id" jsonschema:"id returned by create_computer"`
	Path       string `json:"path" jsonschema:"absolute path inside the computer"`
}

type WriteInput struct {
	ComputerID string `json:"computer_id" jsonschema:"id returned by create_computer"`
	Path       string `json:"path" jsonschema:"absolute path; parent directories are created"`
	Content    string `json:"content" jsonschema:"file contents"`
	Encoding   string `json:"encoding,omitempty" jsonschema:"utf8 (default) or base64 for binary content"`
	Mode       string `json:"mode,omitempty" jsonschema:"octal permissions such as 0755; default 0644"`
}

type ComputerActionInput struct {
	ComputerID      string  `json:"computer_id" jsonschema:"id of a computer created from an image with a display"`
	Action          string  `json:"action" jsonschema:"one of: screenshot, zoom, cursor_position, mouse_move, left_click, right_click, middle_click, double_click, triple_click, left_click_drag, left_mouse_down, left_mouse_up, scroll, type, key, hold_key, wait"`
	Coordinate      []int   `json:"coordinate,omitempty" jsonschema:"[x, y] in screen pixels: target of clicks, mouse_move, scroll, and the end of left_click_drag"`
	StartCoordinate []int   `json:"start_coordinate,omitempty" jsonschema:"[x, y] where left_click_drag starts"`
	Text            string  `json:"text,omitempty" jsonschema:"text for type; key or chord for key and hold_key (e.g. Return, ctrl+l, ctrl+shift+Tab); modifiers held during a click or scroll"`
	ScrollDirection string  `json:"scroll_direction,omitempty" jsonschema:"up, down, left or right"`
	ScrollAmount    int     `json:"scroll_amount,omitempty" jsonschema:"wheel clicks to scroll (default 3)"`
	Duration        float64 `json:"duration,omitempty" jsonschema:"seconds, for wait and hold_key"`
	Region          []int   `json:"region,omitempty" jsonschema:"[x0, y0, x1, y1] to capture at full resolution, for zoom"`
	Screenshot      *bool   `json:"screenshot,omitempty" jsonschema:"return a screenshot after the action; default true except for cursor_position and wait"`
}

type ComputerActionOutput struct {
	CursorX int `json:"cursor_x"`
	CursorY int `json:"cursor_y"`
}

type ImagesOutput struct {
	Images []sandbox.ImageInfo `json:"images"`
}

type ListOutput struct {
	Computers []sandbox.Info `json:"computers"`
}

type FileOutput struct {
	Path     string `json:"path"`
	Size     int    `json:"size"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

type DirOutput struct {
	Path    string              `json:"path"`
	Entries []sandbox.FileEntry `json:"entries"`
}

type DestroyOutput struct {
	Destroyed string `json:"destroyed"`
}

func (t *tools) register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "create_computer",
		Title:       "Create computer",
		Description: "Start a new isolated Linux computer (Firecracker microVM) and return its id. Usually ready in well under a second.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: boolPtr(false)},
	}, handler(t, t.create))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_images",
		Title:       "List images",
		Description: "List the images computers can be created from, with default sizes and which ones have a screen.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)},
	}, handler(t, t.images))
	mcp.AddTool(s, &mcp.Tool{
		Name:  "computer_action",
		Title: "Use the screen",
		Description: "Look at and operate a computer's screen, keyboard and mouse. Works on computers from an image with a display " +
			"(e.g. create_computer image=\"desktop\"), which boot into a browser. Most actions return a screenshot afterwards. " +
			"Take a screenshot first to see the screen; coordinates are pixels from the top left.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(true)},
	}, handler(t, t.computer))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_computers",
		Title:       "List computers",
		Description: "List your computers with their state and expiry.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)},
	}, handler(t, t.list))
	mcp.AddTool(s, &mcp.Tool{
		Name:  "run_command",
		Title: "Run command",
		Description: "Run a shell command on a computer and return its exit code, stdout and stderr (each capped at 1 MiB). " +
			"For long-running servers, start them in the background (nohup ... &) and poll.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(true)},
	}, handler(t, t.run))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "read_file",
		Title:       "Read file",
		Description: "Read a file (up to 1 MiB). Text is returned as UTF-8, anything else as base64.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)},
	}, handler(t, t.read))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "write_file",
		Title:       "Write file",
		Description: "Create or replace a file, creating parent directories.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), IdempotentHint: true, OpenWorldHint: boolPtr(false)},
	}, handler(t, t.write))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_files",
		Title:       "List files",
		Description: "List a directory's entries with type, size and permissions.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)},
	}, handler(t, t.listFiles))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "snapshot_computer",
		Title:       "Snapshot computer",
		Description: "Save a computer's full memory and disk state. Pass the snapshot id to create_computer as from_snapshot to fork it.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: boolPtr(false)},
	}, handler(t, t.snapshot))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "destroy_computer",
		Title:       "Destroy computer",
		Description: "Destroy a computer and everything on it.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), IdempotentHint: true, OpenWorldHint: boolPtr(false)},
	}, handler(t, t.destroy))
}

func (t *tools) create(ctx context.Context, _ *mcp.CallToolRequest, b Backend, in CreateInput) (*mcp.CallToolResult, *sandbox.Info, error) {
	info, err := b.Create(ctx, &api.CreateSandboxRequest{
		Image: in.Image, VCPU: in.VCPU, MemoryMiB: in.MemoryMiB,
		TTLSeconds: in.TTLMinutes * 60, IdleTimeoutSeconds: in.IdleTimeoutMinutes * 60,
		FromSnapshot: in.FromSnapshot, Env: in.Env,
	})
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: text(
		"Created computer %s (image %s, %d vCPU, %d MiB%s) in %d ms. It expires at %s. Pass computer_id %q to the other tools.",
		info.ID, info.Image, info.VCPU, info.MemoryMiB, screenNote(info), info.StartupMs, info.ExpiresAt.Format(time.RFC3339), info.ID,
	)}, info, nil
}

func (t *tools) images(ctx context.Context, _ *mcp.CallToolRequest, b Backend, _ struct{}) (*mcp.CallToolResult, ImagesOutput, error) {
	imgs, err := b.Images(ctx)
	if err != nil {
		return nil, ImagesOutput{}, err
	}
	var sb strings.Builder
	for _, img := range imgs {
		fmt.Fprintf(&sb, "%s: %d vCPU, %d MiB", img.Name, img.VCPU, img.MemoryMiB)
		if img.Display != nil {
			fmt.Fprintf(&sb, ", %dx%d screen", img.Display.Width, img.Display.Height)
		}
		if img.Default {
			sb.WriteString(" (default)")
		}
		sb.WriteByte('\n')
	}
	return &mcp.CallToolResult{Content: text("%s", sb.String())}, ImagesOutput{Images: imgs}, nil
}

func (t *tools) computer(ctx context.Context, _ *mcp.CallToolRequest, b Backend, in ComputerActionInput) (*mcp.CallToolResult, ComputerActionOutput, error) {
	shot := in.Action != sandbox.ActionCursorPosition && in.Action != sandbox.ActionWait
	if in.Screenshot != nil {
		shot = *in.Screenshot
	}
	res, err := b.Computer(ctx, in.ComputerID, &sandbox.ComputerAction{
		Action: in.Action, Coordinate: in.Coordinate, StartCoordinate: in.StartCoordinate,
		Text: in.Text, ScrollDirection: in.ScrollDirection, ScrollAmount: in.ScrollAmount,
		Duration: in.Duration, Region: in.Region, Screenshot: shot,
	})
	if err != nil {
		return nil, ComputerActionOutput{}, err
	}
	out := ComputerActionOutput{CursorX: res.CursorX, CursorY: res.CursorY}
	content := text("Cursor at X=%d, Y=%d.", res.CursorX, res.CursorY)
	if res.Image != nil {
		content = append(content, &mcp.ImageContent{Data: res.Image.Data, MIMEType: res.Image.MIMEType})
	}
	return &mcp.CallToolResult{Content: content}, out, nil
}

func screenNote(info *sandbox.Info) string {
	if info.Display == nil {
		return ""
	}
	return fmt.Sprintf(", %dx%d screen: use computer_action", info.Display.Width, info.Display.Height)
}

func (t *tools) list(ctx context.Context, _ *mcp.CallToolRequest, b Backend, _ struct{}) (*mcp.CallToolResult, ListOutput, error) {
	list, err := b.List(ctx)
	if err != nil {
		return nil, ListOutput{}, err
	}
	if list == nil {
		list = []sandbox.Info{}
	}
	return nil, ListOutput{Computers: list}, nil
}

func (t *tools) run(ctx context.Context, req *mcp.CallToolRequest, b Backend, in RunInput) (*mcp.CallToolResult, *sandbox.ExecResult, error) {
	if strings.TrimSpace(in.Command) == "" {
		return nil, nil, fmt.Errorf("%w: command is required", sandbox.ErrInvalid)
	}
	timeout := in.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}
	// Progress notifications keep proxies (Cloudflare drops idle streams
	// after 100 s) from cutting off long commands.
	if token := req.Params.GetProgressToken(); token != nil {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			start := time.Now()
			tick := time.NewTicker(progressInterval)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ctx.Done():
					return
				case <-tick.C:
					elapsed := time.Since(start).Seconds()
					_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
						ProgressToken: token, Progress: elapsed, Total: float64(timeout),
						Message: fmt.Sprintf("running for %.0f s", elapsed),
					})
				}
			}
		}()
	}
	res, err := b.Exec(ctx, in.ComputerID, &sandbox.ExecRequest{
		Command: in.Command, Cwd: in.Cwd, Env: in.Env, TimeoutSeconds: float64(timeout), Stdin: in.Stdin,
	})
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: formatExec(res, timeout)}}}, res, nil
}

func formatExec(res *sandbox.ExecResult, timeout int) string {
	var sb strings.Builder
	switch {
	case res.TimedOut:
		fmt.Fprintf(&sb, "Timed out after %d s and was killed.\n", timeout)
	case res.Signal != 0:
		fmt.Fprintf(&sb, "Killed by signal %d.\n", res.Signal)
	default:
		fmt.Fprintf(&sb, "Exit code %d.\n", res.ExitCode)
	}
	section := func(name, body string, truncated bool) {
		if body == "" {
			return
		}
		fmt.Fprintf(&sb, "<%s>\n%s", name, body)
		if !strings.HasSuffix(body, "\n") {
			sb.WriteByte('\n')
		}
		if truncated {
			sb.WriteString("[output truncated at 1 MiB]\n")
		}
		fmt.Fprintf(&sb, "</%s>\n", name)
	}
	section("stdout", res.Stdout, res.StdoutTruncated)
	section("stderr", res.Stderr, res.StderrTruncated)
	return sb.String()
}

var errReadTooLarge = fmt.Errorf("%w: file is larger than 1 MiB; read part of it with run_command (head, tail, sed -n)", sandbox.ErrTooLarge)

// limitedBuffer fails writes past maxReadBytes.
type limitedBuffer struct {
	bytes.Buffer
	over bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.Len()+len(p) > maxReadBytes {
		l.over = true
		return 0, errReadTooLarge
	}
	return l.Buffer.Write(p)
}

func (t *tools) read(ctx context.Context, _ *mcp.CallToolRequest, b Backend, in PathInput) (*mcp.CallToolResult, FileOutput, error) {
	var buf limitedBuffer
	if err := b.ReadFile(ctx, in.ComputerID, in.Path, &buf); err != nil {
		if buf.over {
			err = errReadTooLarge
		}
		return nil, FileOutput{}, err
	}
	out := FileOutput{Path: in.Path, Size: buf.Len(), Encoding: "utf8", Content: buf.String()}
	if !utf8.Valid(buf.Bytes()) {
		out.Encoding, out.Content = "base64", base64.StdEncoding.EncodeToString(buf.Bytes())
	}
	if out.Encoding == "utf8" {
		return &mcp.CallToolResult{Content: text("%s", out.Content)}, out, nil
	}
	return &mcp.CallToolResult{Content: text("%s is binary (%d bytes); base64 content:\n%s", in.Path, out.Size, out.Content)}, out, nil
}

func (t *tools) write(ctx context.Context, _ *mcp.CallToolRequest, b Backend, in WriteInput) (*mcp.CallToolResult, *sandbox.FileEntry, error) {
	data := []byte(in.Content)
	switch in.Encoding {
	case "", "utf8", "utf-8":
	case "base64":
		var err error
		if data, err = base64.StdEncoding.DecodeString(in.Content); err != nil {
			return nil, nil, fmt.Errorf("%w: content is not valid base64", sandbox.ErrInvalid)
		}
	default:
		return nil, nil, fmt.Errorf("%w: encoding must be utf8 or base64", sandbox.ErrInvalid)
	}
	var mode uint64
	if in.Mode != "" {
		var err error
		if mode, err = strconv.ParseUint(in.Mode, 8, 32); err != nil {
			return nil, nil, fmt.Errorf("%w: mode must be octal, like 0755", sandbox.ErrInvalid)
		}
	}
	fe, err := b.WriteFile(ctx, in.ComputerID, in.Path, uint32(mode), bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: text("Wrote %d bytes to %s.", fe.Size, fe.Path)}, fe, nil
}

func (t *tools) listFiles(ctx context.Context, _ *mcp.CallToolRequest, b Backend, in PathInput) (*mcp.CallToolResult, DirOutput, error) {
	entries, err := b.ListDir(ctx, in.ComputerID, in.Path)
	if err != nil {
		return nil, DirOutput{}, err
	}
	if entries == nil {
		entries = []sandbox.FileEntry{}
	}
	var sb strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&sb, "%s %-7s %10d %s\n", e.Mode, e.Type, e.Size, e.Name)
	}
	if sb.Len() == 0 {
		sb.WriteString("(empty)\n")
	}
	return &mcp.CallToolResult{Content: text("%s", sb.String())}, DirOutput{Path: in.Path, Entries: entries}, nil
}

func (t *tools) snapshot(ctx context.Context, _ *mcp.CallToolRequest, b Backend, in ComputerInput) (*mcp.CallToolResult, *sandbox.SnapshotInfo, error) {
	snap, err := b.Snapshot(ctx, in.ComputerID)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: text(
		"Saved snapshot %s of %s. Fork it with create_computer from_snapshot=%q.", snap.ID, in.ComputerID, snap.ID,
	)}, snap, nil
}

func (t *tools) destroy(ctx context.Context, _ *mcp.CallToolRequest, b Backend, in ComputerInput) (*mcp.CallToolResult, DestroyOutput, error) {
	if err := b.Delete(ctx, in.ComputerID); err != nil {
		return nil, DestroyOutput{}, err
	}
	return &mcp.CallToolResult{Content: text("Destroyed %s.", in.ComputerID)}, DestroyOutput{Destroyed: in.ComputerID}, nil
}
