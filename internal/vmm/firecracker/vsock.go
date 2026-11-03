package firecracker

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// dialVsock connects to a guest vsock port through Firecracker's hybrid vsock:
// connect to the device's unix socket and send "CONNECT <port>\n"; Firecracker
// answers "OK <host_port>\n" once the guest accepted.
func dialVsock(ctx context.Context, udsPath string, port uint32) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("dialing vsock socket: %w", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake: %w", err)
	}
	// Read the reply byte by byte so no guest data is buffered away from the caller.
	line, err := readLine(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake: %w", err)
	}
	if !strings.HasPrefix(line, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake to port %d: unexpected reply %q", port, line)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func readLine(conn net.Conn) (string, error) {
	r := bufio.NewReaderSize(oneByteReader{conn}, 16)
	var sb strings.Builder
	for sb.Len() < 64 {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\n' {
			return sb.String(), nil
		}
		sb.WriteByte(b)
	}
	return "", fmt.Errorf("handshake line too long: %q", sb.String())
}

// oneByteReader keeps bufio from reading past the handshake line.
type oneByteReader struct{ c net.Conn }

func (o oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return o.c.Read(p)
}
