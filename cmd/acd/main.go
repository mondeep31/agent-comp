// Command acd is the agent-comp daemon: it runs Firecracker microVMs and serves
// the sandbox API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mondeep31/agent-comp/internal/config"
	"github.com/mondeep31/agent-comp/internal/daemon"
	"github.com/mondeep31/agent-comp/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "acd:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/agent-comp/acd.yaml", "path to the config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		return nil
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(mustHost(cfg.Listen)); ip != nil && !ip.IsLoopback() {
		logger.Warn("listen address is not loopback; expose acd only through the tunnel", "listen", cfg.Listen)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("acd starting", "version", version.Version, "listen", cfg.Listen, "images", len(cfg.Images))
	d, err := daemon.New(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := d.Close(closeCtx); err != nil {
			logger.Error("shutdown", "err", err)
		}
		logger.Info("acd stopped")
	}()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if err := d.ReloadKeys(); err != nil {
				logger.Error("reloading api keys", "err", err)
			}
		}
	}()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return err
	}
	logger.Info("api listening", "addr", ln.Addr().String())
	if err := d.Serve(ctx, ln); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func mustHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return host
}
