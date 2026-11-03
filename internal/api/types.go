package api

import (
	"time"

	"github.com/mondeep31/agent-comp/internal/sandbox"
)

// CreateSandboxRequest is the body of POST /v1/sandboxes. Zero values take
// the server's defaults.
type CreateSandboxRequest struct {
	Image              string            `json:"image,omitempty"`
	VCPU               int               `json:"vcpu,omitempty"`
	MemoryMiB          int               `json:"memory_mib,omitempty"`
	DiskMiB            int               `json:"disk_mib,omitempty"`
	TTLSeconds         int               `json:"ttl_seconds,omitempty"`
	IdleTimeoutSeconds int               `json:"idle_timeout_seconds,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	FromSnapshot       string            `json:"from_snapshot,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
}

func (r *CreateSandboxRequest) toService(owner string) sandbox.CreateRequest {
	return sandbox.CreateRequest{
		Owner:        owner,
		Image:        r.Image,
		VCPU:         r.VCPU,
		MemoryMiB:    r.MemoryMiB,
		DiskMiB:      r.DiskMiB,
		TTL:          time.Duration(r.TTLSeconds) * time.Second,
		IdleTimeout:  time.Duration(r.IdleTimeoutSeconds) * time.Second,
		Env:          r.Env,
		FromSnapshot: r.FromSnapshot,
		Metadata:     r.Metadata,
	}
}

type SandboxList struct {
	Sandboxes []sandbox.Info `json:"sandboxes"`
}

type SnapshotList struct {
	Snapshots []sandbox.SnapshotInfo `json:"snapshots"`
}

type DirListing struct {
	Path    string              `json:"path"`
	Entries []sandbox.FileEntry `json:"entries"`
}

// ErrorBody is every non-2xx response.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WebSocket exec protocol, at GET /v1/sandboxes/{id}/exec/ws. Every message is
// a JSON object with a "type".
//
// The client's first message is "start". The server answers "started", then
// streams "stdout"/"stderr" and ends with exactly one "exit" or "error"
// before closing. While running, the client may send "stdin", "stdin_eof"
// and "signal". Byte payloads are base64 (standard encoding).
const (
	WSStart    = "start"
	WSStdin    = "stdin"
	WSStdinEOF = "stdin_eof"
	WSSignal   = "signal"

	WSStarted = "started"
	WSStdout  = "stdout"
	WSStderr  = "stderr"
	WSExit    = "exit"
	WSError   = "error"
)

// WSMessage is the union of all WebSocket messages.
type WSMessage struct {
	Type string `json:"type"`

	// start
	Argv           []string          `json:"argv,omitempty"`
	Command        string            `json:"command,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	TimeoutSeconds float64           `json:"timeout_seconds,omitempty"`

	// stdin, stdout, stderr
	Data []byte `json:"data,omitempty"`

	// signal, exit
	Signal int32 `json:"signal,omitempty"`

	// exit
	ExitCode   *int32 `json:"exit_code,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	DurationMs uint64 `json:"duration_ms,omitempty"`

	// error
	Error *ErrorDetail `json:"error,omitempty"`
}
