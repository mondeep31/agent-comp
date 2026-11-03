package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	agentdv1 "github.com/mondeep31/agent-comp/gen/go/agentcomp/agentd/v1"
)

// Process is a command running in the guest.
type Process struct {
	cn   *conn
	wmu  sync.Mutex
	done chan struct{}
	exit *agentdv1.ExecExit
	err  error
}

// Start launches a command. Output is copied to stdout and stderr (either may
// be nil) from a background goroutine until the process exits. Cancelling ctx
// kills the process.
func (c *Client) Start(ctx context.Context, start *agentdv1.ExecStart, stdout, stderr io.Writer) (*Process, error) {
	if len(start.GetArgv()) == 0 {
		return nil, errors.New("guest: empty argv")
	}
	cn, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	if err := cn.send(&agentdv1.Frame{Kind: &agentdv1.Frame_ExecStart{ExecStart: start}}); err != nil {
		_ = cn.Close()
		return nil, fmt.Errorf("guest: write: %w", err)
	}
	p := &Process{cn: cn, done: make(chan struct{})}
	go p.readLoop(ctx, orDiscard(stdout), orDiscard(stderr))
	return p, nil
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

func (p *Process) readLoop(ctx context.Context, stdout, stderr io.Writer) {
	defer close(p.done)
	defer p.cn.Close()
	for {
		f, err := p.cn.recv(ctx)
		if err != nil {
			p.err = err
			return
		}
		switch k := f.GetKind().(type) {
		case *agentdv1.Frame_ExecOutput:
			w := stdout
			if k.ExecOutput.GetStream() == agentdv1.Stream_STREAM_STDERR {
				w = stderr
			}
			if _, err := w.Write(k.ExecOutput.GetData()); err != nil {
				p.err = fmt.Errorf("guest: copying output: %w", err)
				return
			}
		case *agentdv1.Frame_ExecExit:
			p.exit = k.ExecExit
			return
		default:
			p.err = unexpected(f, "ExecOutput or ExecExit")
			return
		}
	}
}

func (p *Process) send(f *agentdv1.Frame) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	select {
	case <-p.done:
		return errors.New("guest: process already exited")
	default:
	}
	if err := p.cn.send(f); err != nil {
		return fmt.Errorf("guest: write: %w", err)
	}
	return nil
}

// Write sends data to the process's stdin (requires ExecStart.Stdin).
func (p *Process) Write(b []byte) (int, error) {
	for off := 0; off < len(b); off += writeChunk {
		chunk := b[off:min(off+writeChunk, len(b))]
		if err := p.send(&agentdv1.Frame{Kind: &agentdv1.Frame_ExecStdin{ExecStdin: &agentdv1.ExecStdin{Data: chunk}}}); err != nil {
			return off, err
		}
	}
	return len(b), nil
}

// CloseStdin signals EOF on the process's stdin.
func (p *Process) CloseStdin() error {
	return p.send(&agentdv1.Frame{Kind: &agentdv1.Frame_ExecStdin{ExecStdin: &agentdv1.ExecStdin{Eof: true}}})
}

// Signal delivers a POSIX signal to the process group.
func (p *Process) Signal(sig int32) error {
	return p.send(&agentdv1.Frame{Kind: &agentdv1.Frame_ExecSignal{ExecSignal: &agentdv1.ExecSignal{Signal: sig}}})
}

// Done is closed once the process has exited or the connection failed.
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait blocks until the process exits and all output has been copied.
func (p *Process) Wait() (*agentdv1.ExecExit, error) {
	<-p.done
	return p.exit, p.err
}

// Kill abandons the process; agentd kills its process group when the
// connection closes.
func (p *Process) Kill() {
	_ = p.cn.Conn.Close()
	<-p.done
}

// Run starts a command, feeds it stdin (if non-nil) and waits for it.
func (c *Client) Run(ctx context.Context, start *agentdv1.ExecStart, stdin io.Reader, stdout, stderr io.Writer) (*agentdv1.ExecExit, error) {
	start.Stdin = stdin != nil
	p, err := c.Start(ctx, start, stdout, stderr)
	if err != nil {
		return nil, err
	}
	if stdin != nil {
		go func() {
			if _, err := io.Copy(p, stdin); err == nil {
				_ = p.CloseStdin()
			}
		}()
	}
	return p.Wait()
}
