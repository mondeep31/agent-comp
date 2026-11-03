// Command acd is the agent-comp daemon: it runs Firecracker microVMs and serves
// the sandbox API.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mondeep31/agent-comp/internal/config"
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("acd starting", "version", version.Version, "listen", cfg.Listen, "images", len(cfg.Images))
	<-ctx.Done()
	logger.Info("acd stopped")
	return nil
}
