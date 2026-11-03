package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/mondeep31/agent-comp/internal/auth"
	"github.com/mondeep31/agent-comp/internal/config"
	"github.com/mondeep31/agent-comp/internal/golden"
	"github.com/mondeep31/agent-comp/internal/network"
	"github.com/mondeep31/agent-comp/internal/pool"
	"github.com/mondeep31/agent-comp/internal/sandbox"
	"github.com/mondeep31/agent-comp/internal/vmm"
	"github.com/mondeep31/agent-comp/internal/vmm/firecracker"
)

// goldenHeadroom covers golden builds that run alongside a full host.
const goldenHeadroom = 4

// New builds and starts every component. Golden snapshots build lazily in the
// background as the pool fills.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (_ *Daemon, err error) {
	keys, err := auth.Load(cfg.Auth.APIKeysFile)
	if err != nil {
		return nil, err
	}
	if keys.Len() == 0 {
		log.Warn("no api keys configured; every request will be rejected", "file", cfg.Auth.APIKeysFile)
	}

	ctx, cancel := context.WithCancel(ctx)
	d := &Daemon{cfg: cfg, log: log, keys: keys, cancel: cancel}
	defer func() {
		if err != nil {
			_ = d.Close(context.WithoutCancel(ctx))
		}
	}()

	fc := cfg.Firecracker
	driver, err := firecracker.New(firecracker.Options{
		FirecrackerBin: fc.Binary,
		JailerBin:      fc.JailerBinary,
		ChrootBase:     fc.ChrootBase,
		UIDBase:        fc.UIDBase,
		CgroupParent:   fc.CgroupParent,
		MaxVMs:         cfg.Limits.MaxSandboxes + cfg.Pool.Size*len(cfg.Images) + goldenHeadroom,
		Logger:         log,
	})
	if err != nil {
		return nil, fmt.Errorf("vmm driver: %w", err)
	}

	var netProvider vmm.NetworkProvider
	if cfg.Network.Enabled {
		n := cfg.Network
		ncfg, err := network.ParseConfig(n.CIDR, n.Uplink, n.Nameservers, n.DenyCIDRs, cfg.Limits.NetBandwidthMbps)
		if err != nil {
			return nil, err
		}
		p, err := network.New(ctx, ncfg, log)
		if err != nil {
			return nil, fmt.Errorf("network: %w", err)
		}
		netProvider = p
	} else {
		log.Warn("sandbox networking is disabled")
	}

	images := make([]golden.Image, 0, len(cfg.Images))
	rootfs := map[string]string{}
	for _, img := range cfg.Images {
		images = append(images, golden.Image{Name: img.Name, Kernel: img.Kernel, Rootfs: img.Rootfs, BootArgs: img.BootArgs})
		rootfs[img.Name] = img.Rootfs
	}
	snapshots, err := golden.New(golden.Options{
		Driver:         driver,
		Network:        netProvider,
		Dir:            filepath.Join(cfg.DataDir, "golden"),
		Images:         images,
		FirecrackerBin: fc.Binary,
		ReadyTimeout:   30 * time.Second,
		Logger:         log,
	})
	if err != nil {
		return nil, err
	}

	s := cfg.Sandbox
	defaultShape := golden.Shape{VCPU: s.VCPU, MemoryMiB: s.MemoryMiB, DiskMiB: s.DiskMiB}
	warm := map[golden.Key]int{}
	for _, img := range cfg.Images {
		warm[golden.Key{Image: img.Name, Shape: defaultShape}] = cfg.Pool.Size
	}
	vms, err := pool.New(pool.Options{
		Driver:             driver,
		Snapshots:          snapshots,
		Network:            netProvider,
		Dir:                filepath.Join(cfg.DataDir, "pool"),
		Warm:               warm,
		RefillConcurrency:  cfg.Pool.RefillConcurrency,
		DiskBandwidthMiBps: cfg.Limits.DiskBandwidthMiBps,
		Logger:             log,
	})
	if err != nil {
		return nil, err
	}
	vms.Start(ctx)

	d.manager, err = sandbox.New(sandbox.Options{
		Source:       vms,
		Images:       rootfs,
		DefaultImage: cfg.Images[0].Name,
		Defaults:     cfg.Sandbox,
		Limits:       cfg.Limits,
		SnapshotDir:  filepath.Join(cfg.DataDir, "snapshots"),
		Logger:       log,
	})
	if err != nil {
		_ = vms.Close(ctx)
		return nil, err
	}
	go d.manager.Run(ctx, reapInterval)
	d.closers = []func(context.Context) error{
		func(ctx context.Context) error { d.manager.Close(ctx); return nil },
		vms.Close,
	}
	newAPI(d)
	log.Info("acd ready", "images", len(images), "warm_per_image", cfg.Pool.Size, "network", cfg.Network.Enabled, "keys", keys.Len())
	return d, nil
}
