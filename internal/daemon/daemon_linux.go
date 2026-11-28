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
	"github.com/mondeep31/agent-comp/internal/metrics"
	"github.com/mondeep31/agent-comp/internal/network"
	"github.com/mondeep31/agent-comp/internal/pool"
	"github.com/mondeep31/agent-comp/internal/sandbox"
	"github.com/mondeep31/agent-comp/internal/vmm"
	"github.com/mondeep31/agent-comp/internal/vmm/firecracker"
)

const (
	// goldenHeadroom covers golden builds that run alongside a full host.
	goldenHeadroom = 4
	// goldenReadyTimeout covers a cold boot plus, for display images, the
	// desktop session start and a snapshot of a larger memory file.
	goldenReadyTimeout = 90 * time.Second
)

// bootArgs is the image's kernel command line, plus ac.display=WxH for images
// with a screen. golden fingerprints boot args, so changing the display
// rebuilds the snapshot.
func bootArgs(img config.Image) string {
	args := img.BootArgs
	if args == "" {
		args = golden.DefaultBootArgs
	}
	if d := img.Display; d != nil {
		args += fmt.Sprintf(" ac.display=%dx%d", d.Width, d.Height)
	}
	return args
}

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
	d := &Daemon{cfg: cfg, log: log, keys: keys, cancel: cancel, metrics: metrics.New()}
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
		MaxVMs:         cfg.Limits.MaxSandboxes + cfg.TotalPoolSize() + goldenHeadroom,
		MemoryMaxMiB:   fc.CgroupMemoryMaxMiB,
		CPUWeight:      fc.CgroupCPUWeight,
		Logger:         log,
	})
	if err != nil {
		return nil, fmt.Errorf("vmm driver: %w", err)
	}

	var netProvider vmm.NetworkProvider
	if cfg.Network.Enabled {
		n := cfg.Network
		ncfg, err := network.ParseConfig(n.Instance, n.CIDR, n.Uplink, n.Nameservers, n.DenyCIDRs, cfg.Limits.NetBandwidthMbps)
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
	specs := map[string]sandbox.ImageSpec{}
	warm := map[golden.Key]int{}
	for _, img := range cfg.Images {
		images = append(images, golden.Image{Name: img.Name, Kernel: img.Kernel, Rootfs: img.Rootfs, BootArgs: bootArgs(img)})
		s := cfg.ImageShape(img)
		shape := golden.Shape{VCPU: s.VCPU, MemoryMiB: s.MemoryMiB, DiskMiB: s.DiskMiB}
		specs[img.Name] = sandbox.ImageSpec{Rootfs: img.Rootfs, Shape: shape, Display: img.Display}
		warm[golden.Key{Image: img.Name, Shape: shape}] = cfg.ImagePoolSize(img)
	}
	snapshots, err := golden.New(golden.Options{
		Driver:         driver,
		Network:        netProvider,
		Dir:            filepath.Join(cfg.DataDir, "golden"),
		Images:         images,
		FirecrackerBin: fc.Binary,
		ReadyTimeout:   goldenReadyTimeout,
		Logger:         log,
	})
	if err != nil {
		return nil, err
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
		Images:       specs,
		DefaultImage: cfg.Images[0].Name,
		Defaults:     cfg.Sandbox,
		Limits:       cfg.Limits,
		SnapshotDir:  filepath.Join(cfg.DataDir, "snapshots"),
		Logger:       log,
		Events:       d.metrics.SandboxEvent,
	})
	if err != nil {
		_ = vms.Close(ctx)
		return nil, err
	}
	d.metrics.Watch(d.manager.Stats, vms.Stats)
	go d.manager.Run(ctx, reapInterval)
	d.closers = []func(context.Context) error{
		func(ctx context.Context) error { d.manager.Close(ctx); return nil },
		vms.Close,
	}
	newAPI(d)
	log.Info("acd ready", "images", len(images), "warm", cfg.TotalPoolSize(), "network", cfg.Network.Enabled, "keys", keys.Len())
	return d, nil
}
