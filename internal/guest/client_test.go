package guest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	agentdv1 "github.com/mondeep31/agent-comp/gen/go/agentcomp/agentd/v1"
)

// fakeAgent serves one handler per connection over net.Pipe.
func fakeAgent(t *testing.T, handle func(r *bufio.Reader, w net.Conn)) *Client {
	t.Helper()
	return New(func(context.Context) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			handle(bufio.NewReader(server), server)
		}()
		return client, nil
	})
}

func mustRead(t *testing.T, r io.Reader) *agentdv1.Frame {
	t.Helper()
	f, err := readFrame(r)
	if err != nil {
		t.Errorf("fake agent read: %v", err)
		return &agentdv1.Frame{}
	}
	return f
}

func send(w io.Writer, f *agentdv1.Frame) { _ = writeFrame(w, f) }

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := &agentdv1.Frame{Kind: &agentdv1.Frame_Stat{Stat: &agentdv1.Stat{Path: "/etc"}}}
	if err := writeFrame(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := readFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.GetStat().GetPath(); got != "/etc" {
		t.Fatalf("path = %q", got)
	}
	if _, err := readFrame(&buf); !errors.Is(err, io.EOF) {
		t.Fatalf("want io.EOF at clean end, got %v", err)
	}
}

func TestReadFrameRejectsOversize(t *testing.T) {
	r := bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff})
	if _, err := readFrame(r); !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestPingAndErrors(t *testing.T) {
	c := fakeAgent(t, func(r *bufio.Reader, w net.Conn) {
		f := mustRead(t, r)
		if f.GetPing() != nil {
			send(w, &agentdv1.Frame{Kind: &agentdv1.Frame_Pong{Pong: &agentdv1.Pong{AgentVersion: "1.2.3"}}})
			return
		}
		send(w, &agentdv1.Frame{Kind: &agentdv1.Frame_Error{Error: &agentdv1.Error{
			Code: agentdv1.ErrorCode_ERROR_CODE_NOT_FOUND, Message: "nope",
		}}})
	})
	pong, err := c.Ping(t.Context())
	if err != nil || pong.GetAgentVersion() != "1.2.3" {
		t.Fatalf("Ping = %v, %v", pong, err)
	}
	_, err = c.Stat(t.Context(), "/missing")
	if !HasCode(err, agentdv1.ErrorCode_ERROR_CODE_NOT_FOUND) {
		t.Fatalf("Stat error = %v, want NOT_FOUND", err)
	}
}

func TestReadFileStreamsChunks(t *testing.T) {
	c := fakeAgent(t, func(r *bufio.Reader, w net.Conn) {
		mustRead(t, r)
		send(w, &agentdv1.Frame{Kind: &agentdv1.Frame_FileInfo{FileInfo: &agentdv1.FileInfo{Size: 6}}})
		for i, chunk := range []string{"abc", "def", ""} {
			send(w, &agentdv1.Frame{Kind: &agentdv1.Frame_FileData{FileData: &agentdv1.FileData{
				Data: []byte(chunk), Eof: i == 2,
			}}})
		}
	})
	var out bytes.Buffer
	info, err := c.ReadFile(t.Context(), "/f", 0, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "abcdef" || info.GetSize() != 6 {
		t.Fatalf("got %q, size %d", out.String(), info.GetSize())
	}
}

func TestWriteFileSendsEOF(t *testing.T) {
	got := make(chan string, 1)
	c := fakeAgent(t, func(r *bufio.Reader, w net.Conn) {
		mustRead(t, r)
		var data []byte
		for {
			d := mustRead(t, r).GetFileData()
			data = append(data, d.GetData()...)
			if d == nil || d.GetEof() {
				break
			}
		}
		got <- string(data)
		send(w, &agentdv1.Frame{Kind: &agentdv1.Frame_FileInfo{FileInfo: &agentdv1.FileInfo{Size: uint64(len(data))}}})
	})
	payload := strings.Repeat("x", writeChunk+10)
	info, err := c.WriteFile(t.Context(), &agentdv1.WriteFile{Path: "/f"}, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if s := <-got; s != payload || info.GetSize() != uint64(len(payload)) {
		t.Fatalf("agent received %d bytes, info size %d", len(s), info.GetSize())
	}
}

func TestWriteFileReportsEarlyAgentError(t *testing.T) {
	c := fakeAgent(t, func(r *bufio.Reader, w net.Conn) {
		mustRead(t, r)
		send(w, &agentdv1.Frame{Kind: &agentdv1.Frame_Error{Error: &agentdv1.Error{
			Code: agentdv1.ErrorCode_ERROR_CODE_TOO_LARGE, Message: "too big",
		}}})
	})
	_, err := c.WriteFile(t.Context(), &agentdv1.WriteFile{Path: "/f"}, strings.NewReader(strings.Repeat("x", 4*writeChunk)))
	if !HasCode(err, agentdv1.ErrorCode_ERROR_CODE_TOO_LARGE) {
		t.Fatalf("got %v, want TOO_LARGE", err)
	}
}

func TestRunCollectsOutputAndForwardsStdin(t *testing.T) {
	c := fakeAgent(t, func(r *bufio.Reader, w net.Conn) {
		if !mustRead(t, r).GetExecStart().GetStdin() {
			t.Error("stdin not requested")
		}
		// Echo stdin to stdout until EOF, like `cat`.
		for {
			in := mustRead(t, r).GetExecStdin()
			if in == nil || in.GetEof() {
				break
			}
			send(w, &agentdv1.Frame{Kind: &agentdv1.Frame_ExecOutput{ExecOutput: &agentdv1.ExecOutput{
				Stream: agentdv1.Stream_STREAM_STDOUT, Data: in.GetData(),
			}}})
		}
		send(w, &agentdv1.Frame{Kind: &agentdv1.Frame_ExecOutput{ExecOutput: &agentdv1.ExecOutput{
			Stream: agentdv1.Stream_STREAM_STDERR, Data: []byte("warn"),
		}}})
		send(w, &agentdv1.Frame{Kind: &agentdv1.Frame_ExecExit{ExecExit: &agentdv1.ExecExit{ExitCode: 3}}})
	})
	var stdout, stderr bytes.Buffer
	exit, err := c.Run(t.Context(), &agentdv1.ExecStart{Argv: []string{"cat"}},
		strings.NewReader("hello"), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "hello" || stderr.String() != "warn" || exit.GetExitCode() != 3 {
		t.Fatalf("stdout=%q stderr=%q exit=%v", stdout.String(), stderr.String(), exit)
	}
}

func TestStartCancelClosesConnection(t *testing.T) {
	closed := make(chan struct{})
	c := fakeAgent(t, func(r *bufio.Reader, _ net.Conn) {
		mustRead(t, r)
		_, _ = readFrame(r) // blocks until the host closes
		close(closed)
	})
	ctx, cancel := context.WithCancel(t.Context())
	p, err := c.Start(ctx, &agentdv1.ExecStart{Argv: []string{"sleep", "100"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("agent connection not closed after cancel")
	}
	if _, err := p.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context.Canceled", err)
	}
}
