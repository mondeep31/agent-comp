// Package sandbox is the service layer: it owns sandbox records, their state
// machine, quotas and the reaper, and hands callers a guest client for I/O.
// The HTTP API and the MCP server are thin adapters over it.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/mondeep31/agent-comp/internal/config"
	"github.com/mondeep31/agent-comp/internal/golden"
	"github.com/mondeep31/agent-comp/internal/pool"
)

// State is a sandbox's lifecycle state.
//
//	creating -> running <-> paused
//	    |          |          |
//	    +----------+----------+--> stopped (VM gone; record kept briefly) --> deleted
type State string

const (
	StateCreating State = "creating"
	StateRunning  State = "running"
	StatePaused   State = "paused"
	StateStopped  State = "stopped"
	StateDeleted  State = "deleted"
)

var (
	ErrNotFound = errors.New("not found")
	ErrQuota    = errors.New("quota exceeded")
	ErrInvalid  = errors.New("invalid request")
	ErrConflict = errors.New("conflict")
	ErrClosed   = errors.New("shutting down")
	// ErrPermission and ErrTooLarge come from inside the guest.
	ErrPermission = errors.New("permission denied")
	ErrTooLarge   = errors.New("too large")
	// ErrUnavailable means the guest cannot serve the request right now,
	// e.g. its display is still starting.
	ErrUnavailable = errors.New("unavailable")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// ImageSpec is what the manager knows about one configured image.
type ImageSpec struct {
	Rootfs string
	// Shape is the default sandbox shape for the image.
	Shape golden.Shape
	// Display is nil for headless images.
	Display *config.Display
}

// ImageInfo describes an image to API clients.
type ImageInfo struct {
	Name      string          `json:"name"`
	Default   bool            `json:"default"`
	VCPU      int             `json:"vcpu"`
	MemoryMiB int             `json:"memory_mib"`
	DiskMiB   int             `json:"disk_mib"`
	Display   *config.Display `json:"display,omitempty"`
}

// CreateRequest describes a new sandbox. Zero values take config defaults.
type CreateRequest struct {
	Owner       string
	Image       string
	VCPU        int
	MemoryMiB   int
	DiskMiB     int
	TTL         time.Duration
	IdleTimeout time.Duration
	Env         map[string]string
	// FromSnapshot forks a sandbox snapshot instead of a fresh golden image.
	FromSnapshot string
	Metadata     map[string]string
}

// Info is a point-in-time view of a sandbox.
type Info struct {
	ID           string            `json:"id"`
	Owner        string            `json:"owner"`
	State        State             `json:"state"`
	Image        string            `json:"image"`
	Display      *config.Display   `json:"display,omitempty"`
	VCPU         int               `json:"vcpu"`
	MemoryMiB    int               `json:"memory_mib"`
	DiskMiB      int               `json:"disk_mib"`
	CreatedAt    time.Time         `json:"created_at"`
	ExpiresAt    time.Time         `json:"expires_at"`
	LastActiveAt time.Time         `json:"last_active_at"`
	IdleTimeout  Duration          `json:"idle_timeout"`
	FromSnapshot string            `json:"from_snapshot,omitempty"`
	Warm         bool              `json:"warm_start"`
	StartupMs    int64             `json:"startup_ms"`
	StopReason   string            `json:"stop_reason,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// SnapshotInfo describes a user snapshot that new sandboxes can fork from.
type SnapshotInfo struct {
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	Sandbox   string    `json:"source_sandbox"`
	Image     string    `json:"image"`
	VCPU      int       `json:"vcpu"`
	MemoryMiB int       `json:"memory_mib"`
	DiskMiB   int       `json:"disk_mib"`
	CreatedAt time.Time `json:"created_at"`
}

// Duration marshals as whole seconds, which is friendlier to agents than ns.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, "%d", int64(time.Duration(d)/time.Second)), nil
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	secs, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return fmt.Errorf("duration: want whole seconds, got %s", b)
	}
	*d = Duration(time.Duration(secs) * time.Second)
	return nil
}

// VMSource is satisfied by *pool.Pool.
type VMSource interface {
	Acquire(ctx context.Context, key golden.Key) (*pool.Instance, error)
	Restore(ctx context.Context, snap *golden.Snapshot) (*pool.Instance, error)
}
