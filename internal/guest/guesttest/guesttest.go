// Package guesttest provides an in-memory agentd and VM for tests of the
// layers above the guest protocol (sandbox manager, API, MCP).
package guesttest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	agentdv1 "github.com/mondeep31/agent-comp/gen/go/agentcomp/agentd/v1"
	"github.com/mondeep31/agent-comp/internal/guest"
	"github.com/mondeep31/agent-comp/internal/vmm"
)

// Agent is a fake agentd with an in-memory filesystem. Exec understands a few
// shell-like commands:
//
//	echo ARGS...       prints ARGS
//	cat                copies stdin to stdout
//	sleep SECONDS      sleeps (honours timeouts and kills)
//	exit N             exits with status N
//	hostname           prints the configured hostname
//	env NAME           prints an environment variable
//
// Commands run through "sh -c CMD" or directly as argv.
type Agent struct {
	mu       sync.Mutex
	files    map[string][]byte
	hostname string
	env      map[string]string
	clockMs  int64
}

func NewAgent() *Agent {
	return &Agent{files: map[string][]byte{}, env: map[string]string{}}
}

// Clone copies the agent's state, like restoring a snapshot.
func (a *Agent) Clone() *Agent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return &Agent{files: maps.Clone(a.files), hostname: a.hostname, env: maps.Clone(a.env), clockMs: a.clockMs}
}

// ClockMs is the wall clock most recently set through Configure.
func (a *Agent) ClockMs() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.clockMs
}

func (a *Agent) Hostname() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hostname
}

func send(c net.Conn, kind any) {
	f := &agentdv1.Frame{}
	switch k := kind.(type) {
	case *agentdv1.Pong:
		f.Kind = &agentdv1.Frame_Pong{Pong: k}
	case *agentdv1.Ack:
		f.Kind = &agentdv1.Frame_Ack{Ack: k}
	case *agentdv1.FileInfo:
		f.Kind = &agentdv1.Frame_FileInfo{FileInfo: k}
	case *agentdv1.FileData:
		f.Kind = &agentdv1.Frame_FileData{FileData: k}
	case *agentdv1.DirListing:
		f.Kind = &agentdv1.Frame_DirListing{DirListing: k}
	case *agentdv1.ExecOutput:
		f.Kind = &agentdv1.Frame_ExecOutput{ExecOutput: k}
	case *agentdv1.ExecExit:
		f.Kind = &agentdv1.Frame_ExecExit{ExecExit: k}
	case *agentdv1.Error:
		f.Kind = &agentdv1.Frame_Error{Error: k}
	default:
		panic(fmt.Sprintf("guesttest: unsupported frame %T", kind))
	}
	_ = guest.WriteFrame(c, f)
}

func fail(c net.Conn, code agentdv1.ErrorCode, format string, args ...any) {
	send(c, &agentdv1.Error{Code: code, Message: fmt.Sprintf(format, args...)})
}

// Serve handles one connection, like agentd does.
func (a *Agent) Serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	f, err := guest.ReadFrame(r)
	if err != nil {
		return
	}
	switch k := f.GetKind().(type) {
	case *agentdv1.Frame_Ping:
		send(c, &agentdv1.Pong{AgentVersion: "fake"})
	case *agentdv1.Frame_Configure:
		a.mu.Lock()
		if k.Configure.GetHostname() != "" {
			a.hostname = k.Configure.GetHostname()
		}
		maps.Copy(a.env, k.Configure.GetEnv())
		if ms := k.Configure.GetWallClockUnixMs(); ms != 0 {
			a.clockMs = ms
		}
		a.mu.Unlock()
		send(c, &agentdv1.Ack{})
	case *agentdv1.Frame_Stat:
		a.stat(c, k.Stat.GetPath())
	case *agentdv1.Frame_ListDir:
		a.list(c, k.ListDir.GetPath())
	case *agentdv1.Frame_ReadFile:
		a.read(c, k.ReadFile)
	case *agentdv1.Frame_WriteFile:
		a.write(c, r, k.WriteFile)
	case *agentdv1.Frame_ExecStart:
		a.exec(c, r, k.ExecStart)
	default:
		fail(c, agentdv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "unexpected %T", k)
	}
}

func (a *Agent) info(p string) (*agentdv1.FileInfo, bool) {
	if data, ok := a.files[p]; ok {
		return &agentdv1.FileInfo{Path: p, Name: path.Base(p), Type: agentdv1.FileType_FILE_TYPE_FILE, Size: uint64(len(data)), Mode: 0o644}, true
	}
	prefix := strings.TrimSuffix(p, "/") + "/"
	for f := range a.files {
		if strings.HasPrefix(f, prefix) || p == "/" {
			return &agentdv1.FileInfo{Path: p, Name: path.Base(p), Type: agentdv1.FileType_FILE_TYPE_DIR, Mode: 0o755}, true
		}
	}
	return nil, false
}

func (a *Agent) stat(c net.Conn, p string) {
	a.mu.Lock()
	fi, ok := a.info(p)
	a.mu.Unlock()
	if !ok {
		fail(c, agentdv1.ErrorCode_ERROR_CODE_NOT_FOUND, "stat %s: not found", p)
		return
	}
	send(c, fi)
}

func (a *Agent) list(c net.Conn, dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if fi, ok := a.info(dir); !ok || fi.GetType() != agentdv1.FileType_FILE_TYPE_DIR {
		fail(c, agentdv1.ErrorCode_ERROR_CODE_NOT_FOUND, "listing %s: not a directory", dir)
		return
	}
	prefix := strings.TrimSuffix(dir, "/") + "/"
	seen := map[string]bool{}
	var out []*agentdv1.FileInfo
	for _, f := range slices.Sorted(maps.Keys(a.files)) {
		rest, ok := strings.CutPrefix(f, prefix)
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(rest, "/")
		if seen[name] {
			continue
		}
		seen[name] = true
		fi, _ := a.info(prefix + name)
		out = append(out, fi)
	}
	send(c, &agentdv1.DirListing{Entries: out})
}

func (a *Agent) read(c net.Conn, req *agentdv1.ReadFile) {
	a.mu.Lock()
	data, ok := a.files[req.GetPath()]
	a.mu.Unlock()
	if !ok {
		fail(c, agentdv1.ErrorCode_ERROR_CODE_NOT_FOUND, "opening %s: not found", req.GetPath())
		return
	}
	if req.GetMaxBytes() > 0 && uint64(len(data)) > req.GetMaxBytes() {
		fail(c, agentdv1.ErrorCode_ERROR_CODE_TOO_LARGE, "%s is too large", req.GetPath())
		return
	}
	send(c, &agentdv1.FileInfo{Path: req.GetPath(), Name: path.Base(req.GetPath()), Size: uint64(len(data)), Type: agentdv1.FileType_FILE_TYPE_FILE})
	send(c, &agentdv1.FileData{Data: data})
	send(c, &agentdv1.FileData{Eof: true})
}

func (a *Agent) write(c net.Conn, r *bufio.Reader, req *agentdv1.WriteFile) {
	var buf []byte
	for {
		f, err := guest.ReadFrame(r)
		if err != nil {
			return
		}
		d := f.GetFileData()
		buf = append(buf, d.GetData()...)
		if d.GetEof() {
			break
		}
	}
	a.mu.Lock()
	if req.GetAppend() {
		buf = append(a.files[req.GetPath()], buf...)
	}
	a.files[req.GetPath()] = buf
	a.mu.Unlock()
	send(c, &agentdv1.FileInfo{Path: req.GetPath(), Name: path.Base(req.GetPath()), Size: uint64(len(buf)), Type: agentdv1.FileType_FILE_TYPE_FILE, Mode: req.GetMode()})
}

func (a *Agent) exec(c net.Conn, r *bufio.Reader, start *agentdv1.ExecStart) {
	argv := start.GetArgv()
	if len(argv) == 3 && (argv[0] == "sh" || argv[0] == "/bin/sh" || argv[0] == "bash") && argv[1] == "-c" {
		argv = strings.Fields(argv[2])
	}
	if len(argv) == 0 {
		fail(c, agentdv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "empty argv")
		return
	}
	began := time.Now()
	out := func(s string) {
		send(c, &agentdv1.ExecOutput{Stream: agentdv1.Stream_STREAM_STDOUT, Data: []byte(s)})
	}
	exit := &agentdv1.ExecExit{}
	// Host closing the connection or sending a signal ends the command.
	gone := make(chan struct{})
	stdin := make(chan []byte, 16)
	go func() {
		defer close(gone)
		open := true
		closeStdin := func() {
			if open {
				close(stdin)
				open = false
			}
		}
		defer closeStdin()
		for {
			f, err := guest.ReadFrame(r)
			if err != nil {
				return
			}
			if in := f.GetExecStdin(); in != nil {
				if len(in.GetData()) > 0 && open {
					stdin <- in.GetData()
				}
				if in.GetEof() {
					closeStdin()
				}
			}
			if s := f.GetExecSignal(); s != nil {
				exit.Signal = s.GetSignal()
				return
			}
		}
	}()

	a.mu.Lock()
	host, env := a.hostname, maps.Clone(a.env)
	a.mu.Unlock()
	maps.Copy(env, start.GetEnv())
	switch argv[0] {
	case "echo":
		out(strings.Join(argv[1:], " ") + "\n")
	case "hostname":
		out(host + "\n")
	case "env":
		if len(argv) > 1 {
			out(env[argv[1]] + "\n")
		}
	case "exit":
		n, _ := strconv.Atoi(argv[len(argv)-1])
		exit.ExitCode = int32(n) //nolint:gosec // test input
	case "cat":
		if start.GetStdin() {
			for data := range stdin {
				out(string(data))
			}
		}
	case "sleep":
		secs, _ := strconv.ParseFloat(argv[len(argv)-1], 64)
		timeout := time.Duration(start.GetTimeoutMs()) * time.Millisecond
		if timeout == 0 {
			timeout = time.Hour
		}
		select {
		case <-time.After(time.Duration(secs * float64(time.Second))):
		case <-time.After(timeout):
			exit.TimedOut, exit.ExitCode, exit.Signal = true, -1, 9
		case <-gone:
			exit.ExitCode = -1
		}
	default:
		fail(c, agentdv1.ErrorCode_ERROR_CODE_NOT_FOUND, "starting %s: not found", argv[0])
		return
	}
	exit.DurationMs = uint64(time.Since(began).Milliseconds()) //nolint:gosec // non-negative
	send(c, exit)
}

// VM is a fake vmm.VM backed by an Agent.
type VM struct {
	IDValue string
	Agent   *Agent
	// Overlay is returned by OverlayPath; snapshots copy it.
	Overlay string

	mu      sync.Mutex
	paused  bool
	stopped bool
	done    chan struct{}
	// SnapshotAgent receives a clone of the agent on Snapshot, for forks.
	OnSnapshot func(files vmm.SnapshotFiles, agent *Agent)
}

func NewVM(id string, agent *Agent) *VM {
	return &VM{IDValue: id, Agent: agent, done: make(chan struct{})}
}

var _ vmm.VM = (*VM)(nil)

func (v *VM) ID() string { return v.IDValue }

func (v *VM) Pause(context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stopped {
		return vmm.ErrNotRunning
	}
	v.paused = true
	return nil
}

func (v *VM) Resume(context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stopped {
		return vmm.ErrNotRunning
	}
	v.paused = false
	return nil
}

func (v *VM) Paused() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.paused
}

func (v *VM) Snapshot(_ context.Context, files vmm.SnapshotFiles) error {
	v.mu.Lock()
	paused := v.paused
	v.mu.Unlock()
	if !paused {
		return errors.New("guesttest: snapshot of a running vm")
	}
	if v.OnSnapshot != nil {
		v.OnSnapshot(files, v.Agent.Clone())
	}
	return nil
}

func (v *VM) DialVsock(context.Context, uint32) (net.Conn, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stopped {
		return nil, vmm.ErrNotRunning
	}
	if v.paused {
		return nil, errors.New("guesttest: vm is paused")
	}
	client, server := net.Pipe()
	go v.Agent.Serve(server)
	return client, nil
}

func (v *VM) OverlayPath() string   { return v.Overlay }
func (v *VM) Console() []byte       { return nil }
func (v *VM) Done() <-chan struct{} { return v.done }

// Crash simulates the VMM exiting on its own.
func (v *VM) Crash() { _ = v.Stop(context.Background()) }

func (v *VM) Stop(context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stopped {
		return vmm.ErrNotRunning
	}
	v.stopped = true
	close(v.done)
	return nil
}

func (v *VM) Stopped() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stopped
}
