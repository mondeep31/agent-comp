// Package daemon assembles acd: VMM driver, network, golden snapshots, warm
// pool, sandbox manager and API.
package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/mondeep31/agent-comp/internal/api"
	"github.com/mondeep31/agent-comp/internal/auth"
	"github.com/mondeep31/agent-comp/internal/config"
	"github.com/mondeep31/agent-comp/internal/sandbox"
)

const reapInterval = 10 * time.Second

// Daemon is a running acd.
type Daemon struct {
	cfg     config.Config
	log     *slog.Logger
	keys    *auth.Keys
	manager *sandbox.Manager
	handler http.Handler
	// closers run in order on Close.
	closers []func(context.Context) error
	cancel  context.CancelFunc
}

// Handler is the API handler, for serving on a custom listener.
func (d *Daemon) Handler() http.Handler { return d.handler }

func (d *Daemon) Manager() *sandbox.Manager { return d.manager }

// ReloadKeys re-reads the API keys file.
func (d *Daemon) ReloadKeys() error {
	if err := d.keys.Reload(); err != nil {
		return err
	}
	d.log.Info("api keys reloaded", "keys", d.keys.Len())
	return nil
}

// Serve serves the API on ln until ctx ends, then shuts down gracefully.
func (d *Daemon) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           d.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(d.log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	if serr := <-errc; !errors.Is(serr, http.ErrServerClosed) {
		err = errors.Join(err, serr)
	}
	return err
}

// Close destroys every sandbox and pooled VM.
func (d *Daemon) Close(ctx context.Context) error {
	d.cancel()
	var errs []error
	for _, c := range d.closers {
		errs = append(errs, c(ctx))
	}
	return errors.Join(errs...)
}

func newAPI(d *Daemon) {
	d.handler = api.New(api.Options{
		Manager:           d.manager,
		Auth:              d.keys,
		RequestsPerSecond: d.cfg.Limits.RequestsPerSecond,
		Burst:             d.cfg.Limits.RequestBurst,
		Logger:            d.log,
	}).Handler()
}
