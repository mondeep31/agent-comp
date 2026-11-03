package firecracker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shortTempDir keeps unix socket paths under the 104-byte macOS limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func serveUnix(t *testing.T, handler http.Handler) string {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "api.sock")
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return sock
}

func TestAPIClientSendsJSON(t *testing.T) {
	type got struct {
		method, path string
		body         machineConfig
	}
	gotCh := make(chan got, 1)
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var mc machineConfig
		_ = json.NewDecoder(r.Body).Decode(&mc)
		gotCh <- got{method: r.Method, path: r.URL.Path, body: mc}
		w.WriteHeader(http.StatusNoContent)
	}))

	c := newAPIClient(sock)
	defer c.close()
	want := machineConfig{VCPUCount: 2, MemSizeMiB: 512}
	if err := c.put(context.Background(), "/machine-config", want); err != nil {
		t.Fatalf("put: %v", err)
	}
	g := <-gotCh
	if g.method != http.MethodPut || g.path != "/machine-config" || g.body != want {
		t.Fatalf("server saw %+v", g)
	}
}

func TestAPIClientReturnsFault(t *testing.T) {
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"fault_message":"Invalid state"}`)
	}))
	c := newAPIClient(sock)
	defer c.close()

	err := c.patch(context.Background(), "/vm", vmState{State: "Paused"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Status != http.StatusBadRequest || apiErr.Fault != "Invalid state" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
}

func TestBandwidthLimiter(t *testing.T) {
	if bandwidthLimiter(0) != nil {
		t.Error("0 should disable the limiter")
	}
	rl := bandwidthLimiter(1 << 20)
	if rl == nil || rl.Bandwidth.Size != 1<<20 || rl.Bandwidth.RefillTime != 1000 {
		t.Errorf("limiter = %+v", rl)
	}
}

// fakeHybridVsock emulates Firecracker's host side of a hybrid vsock device.
func fakeHybridVsock(t *testing.T, reply string) string {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "v.sock")
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		if line != "CONNECT 1024\n" {
			return
		}
		_, _ = io.WriteString(conn, reply+"guest-data")
		time.Sleep(100 * time.Millisecond)
	}()
	return sock
}

func TestDialVsockHandshake(t *testing.T) {
	sock := fakeHybridVsock(t, "OK 1073741824\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := dialVsock(ctx, sock, 1024)
	if err != nil {
		t.Fatalf("dialVsock: %v", err)
	}
	defer conn.Close()
	buf := make([]byte, len("guest-data"))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "guest-data" {
		t.Fatalf("data after handshake = %q, %v", buf, err)
	}
}

func TestDialVsockRejectsBadReply(t *testing.T) {
	sock := fakeHybridVsock(t, "NOPE\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := dialVsock(ctx, sock, 1024); err == nil {
		t.Fatal("expected handshake error")
	}
}
