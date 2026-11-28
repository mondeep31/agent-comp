// Package guest is the host-side client for agentd, the in-VM agent.
//
// Every call opens its own connection, mirroring agentd's one operation per
// connection protocol, so calls are independent and safe for concurrent use.
package guest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	agentdv1 "github.com/mondeep31/agent-comp/gen/go/agentcomp/agentd/v1"
)

// Port is the vsock port agentd listens on.
const Port = 1024

const writeChunk = 256 << 10

// Dialer opens a fresh stream to agentd.
type Dialer func(ctx context.Context) (net.Conn, error)

// Error is a failure reported by agentd.
type Error struct {
	Code    agentdv1.ErrorCode
	Message string
}

func (e *Error) Error() string { return "agentd: " + e.Message }

// HasCode reports whether err is an agentd error with the given code.
func HasCode(err error, code agentdv1.ErrorCode) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Client talks to one agentd instance.
type Client struct {
	dial Dialer
}

func New(dial Dialer) *Client { return &Client{dial: dial} }

// VsockDialer is satisfied by vmm.VM.
type VsockDialer interface {
	DialVsock(ctx context.Context, port uint32) (net.Conn, error)
}

// ForVM returns a client for the agentd inside vm.
func ForVM(vm VsockDialer) *Client {
	return New(func(ctx context.Context) (net.Conn, error) { return vm.DialVsock(ctx, Port) })
}

type conn struct {
	net.Conn
	r    *bufio.Reader
	stop func() bool
}

// open dials and arranges for the connection to close when ctx ends, which
// also makes agentd abort the operation.
func (c *Client) open(ctx context.Context) (*conn, error) {
	nc, err := c.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("guest: dial: %w", err)
	}
	return &conn{
		Conn: nc,
		r:    bufio.NewReaderSize(nc, 64<<10),
		stop: context.AfterFunc(ctx, func() { _ = nc.Close() }),
	}, nil
}

func (c *conn) Close() error {
	c.stop()
	return c.Conn.Close()
}

func (c *conn) send(f *agentdv1.Frame) error { return WriteFrame(c.Conn, f) }

// recv returns the next frame, converting Error frames into *Error.
func (c *conn) recv(ctx context.Context) (*agentdv1.Frame, error) {
	f, err := ReadFrame(c.r)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("guest: read: %w", err)
	}
	if e := f.GetError(); e != nil {
		return nil, &Error{Code: e.GetCode(), Message: e.GetMessage()}
	}
	return f, nil
}

func unexpected(f *agentdv1.Frame, want string) error {
	return fmt.Errorf("guest: expected %s, got %T", want, f.GetKind())
}

// call performs a single request/response exchange.
func (c *Client) call(ctx context.Context, req *agentdv1.Frame) (*agentdv1.Frame, error) {
	cn, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	defer cn.Close()
	if err := cn.send(req); err != nil {
		return nil, fmt.Errorf("guest: write: %w", err)
	}
	return cn.recv(ctx)
}

// ErrNoDisplay is returned for display operations on a headless guest.
var ErrNoDisplay = errors.New("guest: no display")

func (c *Client) Ping(ctx context.Context) (*agentdv1.Pong, error) {
	f, err := c.call(ctx, &agentdv1.Frame{Kind: &agentdv1.Frame_Ping{Ping: &agentdv1.Ping{}}})
	if err != nil {
		return nil, err
	}
	if p := f.GetPong(); p != nil {
		return p, nil
	}
	return nil, unexpected(f, "Pong")
}

// WaitReady pings until agentd answers or ctx ends. Each attempt is bounded so
// a connection accepted before the guest's listener exists cannot hang it.
func (c *Client) WaitReady(ctx context.Context) error {
	_, err := c.Ready(ctx)
	return err
}

// WaitDisplay pings until the guest's display is ready. It fails at once for
// a headless guest, and with the guest's reason when ctx ends first.
func (c *Client) WaitDisplay(ctx context.Context) (*agentdv1.Display, error) {
	const pause = 100 * time.Millisecond
	var last *agentdv1.Display
	for {
		pong, err := c.Ready(ctx)
		if err != nil {
			return nil, err
		}
		d := pong.GetDisplay()
		if d == nil {
			return nil, ErrNoDisplay
		}
		if d.GetReady() {
			return d, nil
		}
		last = d
		select {
		case <-ctx.Done():
			reason := "still starting"
			if last.GetError() != "" {
				reason = last.GetError()
			}
			return nil, fmt.Errorf("guest: display not ready (%s): %w", reason, ctx.Err())
		case <-time.After(pause):
		}
	}
}

// Ready is WaitReady that also returns agentd's first Pong.
func (c *Client) Ready(ctx context.Context) (*agentdv1.Pong, error) {
	const attempt, pause = 250 * time.Millisecond, 5 * time.Millisecond
	for {
		actx, cancel := context.WithTimeout(ctx, attempt)
		pong, err := c.Ping(actx)
		cancel()
		if err == nil {
			return pong, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("guest: agent not ready: %w", errors.Join(ctx.Err(), err))
		case <-time.After(pause):
		}
	}
}

func (c *Client) Configure(ctx context.Context, cfg *agentdv1.Configure) error {
	f, err := c.call(ctx, &agentdv1.Frame{Kind: &agentdv1.Frame_Configure{Configure: cfg}})
	if err != nil {
		return err
	}
	if f.GetAck() == nil {
		return unexpected(f, "Ack")
	}
	return nil
}

func (c *Client) Stat(ctx context.Context, path string) (*agentdv1.FileInfo, error) {
	f, err := c.call(ctx, &agentdv1.Frame{Kind: &agentdv1.Frame_Stat{Stat: &agentdv1.Stat{Path: path}}})
	if err != nil {
		return nil, err
	}
	if fi := f.GetFileInfo(); fi != nil {
		return fi, nil
	}
	return nil, unexpected(f, "FileInfo")
}

func (c *Client) ListDir(ctx context.Context, path string) ([]*agentdv1.FileInfo, error) {
	f, err := c.call(ctx, &agentdv1.Frame{Kind: &agentdv1.Frame_ListDir{ListDir: &agentdv1.ListDir{Path: path}}})
	if err != nil {
		return nil, err
	}
	if l := f.GetDirListing(); l != nil {
		return l.GetEntries(), nil
	}
	return nil, unexpected(f, "DirListing")
}

// ReadFile streams a file into w. maxBytes 0 uses the agent's limit.
func (c *Client) ReadFile(ctx context.Context, path string, maxBytes uint64, w io.Writer) (*agentdv1.FileInfo, error) {
	cn, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	defer cn.Close()
	req := &agentdv1.ReadFile{Path: path, MaxBytes: maxBytes}
	if err := cn.send(&agentdv1.Frame{Kind: &agentdv1.Frame_ReadFile{ReadFile: req}}); err != nil {
		return nil, fmt.Errorf("guest: write: %w", err)
	}
	f, err := cn.recv(ctx)
	if err != nil {
		return nil, err
	}
	info := f.GetFileInfo()
	if info == nil {
		return nil, unexpected(f, "FileInfo")
	}
	for {
		f, err := cn.recv(ctx)
		if err != nil {
			return nil, err
		}
		d := f.GetFileData()
		if d == nil {
			return nil, unexpected(f, "FileData")
		}
		if _, err := w.Write(d.GetData()); err != nil {
			return nil, fmt.Errorf("guest: writing %s locally: %w", path, err)
		}
		if d.GetEof() {
			return info, nil
		}
	}
}

// WriteFile streams r into a file in the guest.
func (c *Client) WriteFile(ctx context.Context, req *agentdv1.WriteFile, r io.Reader) (*agentdv1.FileInfo, error) {
	cn, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	defer cn.Close()
	if err := cn.send(&agentdv1.Frame{Kind: &agentdv1.Frame_WriteFile{WriteFile: req}}); err != nil {
		return nil, fmt.Errorf("guest: write: %w", err)
	}
	// agentd may reject the request (or the size) before we finish sending;
	// read concurrently so its Error frame wins over our broken pipe.
	type result struct {
		f   *agentdv1.Frame
		err error
	}
	reply := make(chan result, 1)
	go func() {
		f, err := cn.recv(ctx)
		reply <- result{f, err}
	}()

	sendErr := sendChunks(cn, r)
	if sendErr != nil {
		// Unblock the reader if the guest is still waiting for data.
		_ = cn.Conn.Close()
	}
	res := <-reply
	if res.err != nil {
		var agentErr *Error
		if errors.As(res.err, &agentErr) || sendErr == nil {
			return nil, res.err
		}
	}
	if sendErr != nil {
		return nil, sendErr
	}
	if fi := res.f.GetFileInfo(); fi != nil {
		return fi, nil
	}
	return nil, unexpected(res.f, "FileInfo")
}

func sendChunks(cn *conn, r io.Reader) error {
	buf := make([]byte, writeChunk)
	for {
		n, err := io.ReadFull(r, buf)
		eof := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
		if err != nil && !eof {
			return fmt.Errorf("guest: reading local data: %w", err)
		}
		data := &agentdv1.FileData{Data: buf[:n], Eof: eof}
		if err := cn.send(&agentdv1.Frame{Kind: &agentdv1.Frame_FileData{FileData: data}}); err != nil {
			return fmt.Errorf("guest: write: %w", err)
		}
		if eof {
			return nil
		}
	}
}
