//go:build !linux

package daemon

import (
	"context"
	"errors"
	"log/slog"

	"github.com/mondeep31/agent-comp/internal/config"
)

// New fails: acd needs KVM, Firecracker and Linux network namespaces.
func New(context.Context, config.Config, *slog.Logger) (*Daemon, error) {
	return nil, errors.New("acd runs on linux only")
}
