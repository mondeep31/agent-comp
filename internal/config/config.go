// Package config loads and validates the acd configuration file.
//
// Everything host-specific (paths, CIDRs, pool sizes, limits) lives here so the
// same binaries run unchanged on the nested-virt dev host and on bare metal.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	// Listen is the API address. Keep it on loopback; ingress goes through the tunnel.
	Listen string `yaml:"listen"`
	// MetricsListen serves Prometheus metrics; empty disables. Keep it on loopback.
	MetricsListen string          `yaml:"metrics_listen"`
	DataDir       string          `yaml:"data_dir"`
	Firecracker   Firecracker     `yaml:"firecracker"`
	Images        []Image         `yaml:"images"`
	Sandbox       SandboxDefaults `yaml:"sandbox"`
	Limits        Limits          `yaml:"limits"`
	Pool          Pool            `yaml:"pool"`
	Network       Network         `yaml:"network"`
	Auth          Auth            `yaml:"auth"`
}

// Firecracker configures the VMM. Every VM always runs under jailer: snapshot
// clones depend on each VM seeing identical paths inside its own chroot.
type Firecracker struct {
	Binary       string `yaml:"binary"`
	JailerBinary string `yaml:"jailer_binary"`
	ChrootBase   string `yaml:"chroot_base"`
	// UIDBase is the first uid handed to jailed VMs; each VM slot gets UIDBase+slot.
	UIDBase int `yaml:"uid_base"`
	// CgroupParent is the cgroup v2 path (relative to /sys/fs/cgroup) that jailed VMs live under.
	CgroupParent string `yaml:"cgroup_parent"`
}

type Image struct {
	Name     string `yaml:"name"`
	Kernel   string `yaml:"kernel"`
	Rootfs   string `yaml:"rootfs"`
	BootArgs string `yaml:"boot_args"`
}

type SandboxDefaults struct {
	VCPU        int           `yaml:"vcpu"`
	MemoryMiB   int           `yaml:"memory_mib"`
	DiskMiB     int           `yaml:"disk_mib"`
	TTL         time.Duration `yaml:"ttl"`
	IdleTimeout time.Duration `yaml:"idle_timeout"`
}

type Limits struct {
	MaxSandboxes       int           `yaml:"max_sandboxes"`
	MaxSandboxesPerKey int           `yaml:"max_sandboxes_per_key"`
	MaxSnapshotsPerKey int           `yaml:"max_snapshots_per_key"`
	MaxVCPU            int           `yaml:"max_vcpu"`
	MaxMemoryMiB       int           `yaml:"max_memory_mib"`
	MaxDiskMiB         int           `yaml:"max_disk_mib"`
	MaxTTL             time.Duration `yaml:"max_ttl"`
	MaxExecTimeout     time.Duration `yaml:"max_exec_timeout"`
	MaxFileBytes       int64         `yaml:"max_file_bytes"`
	// RequestsPerSecond and RequestBurst rate-limit each API key.
	RequestsPerSecond float64 `yaml:"requests_per_second"`
	RequestBurst      int     `yaml:"request_burst"`
	// Per-VM I/O rate limits enforced by Firecracker; 0 disables.
	DiskBandwidthMiBps int `yaml:"disk_bandwidth_mibps"`
	NetBandwidthMbps   int `yaml:"net_bandwidth_mbps"`
}

type Pool struct {
	// Size is the number of pre-restored, paused VMs kept per image.
	Size              int `yaml:"size"`
	RefillConcurrency int `yaml:"refill_concurrency"`
}

type Network struct {
	Enabled bool `yaml:"enabled"`
	// CIDR is the address space carved into one /30 per sandbox.
	CIDR string `yaml:"cidr"`
	// Uplink is the host interface used for NAT; empty means the default route's interface.
	Uplink      string   `yaml:"uplink"`
	Nameservers []string `yaml:"nameservers"`
	// DenyCIDRs are unreachable from sandboxes (host LAN, tailnet, metadata, private ranges).
	DenyCIDRs []string `yaml:"deny_cidrs"`
}

type Auth struct {
	// APIKeysFile holds one "name:sha256hex" entry per line.
	APIKeysFile string `yaml:"api_keys_file"`
}

// DefaultDenyCIDRs blocks private, link-local, CGNAT (Tailscale) and metadata ranges.
func DefaultDenyCIDRs() []string {
	return []string{
		"0.0.0.0/8",
		"10.0.0.0/8",
		"100.64.0.0/10",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"224.0.0.0/4",
		"240.0.0.0/4",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
		"fd7a:115c:a1e0::/48",
	}
}

// Default returns a configuration suitable for a single dev host.
func Default() Config {
	return Config{
		Listen:        "127.0.0.1:8080",
		MetricsListen: "127.0.0.1:9464",
		DataDir:       "/var/lib/agent-comp",
		Firecracker: Firecracker{
			Binary:       "/usr/local/bin/firecracker",
			JailerBinary: "/usr/local/bin/jailer",
			ChrootBase:   "/srv/jailer",
			UIDBase:      200000,
			CgroupParent: "agent-comp",
		},
		Images: []Image{},
		Sandbox: SandboxDefaults{
			VCPU:        1,
			MemoryMiB:   256,
			DiskMiB:     2048,
			TTL:         time.Hour,
			IdleTimeout: 15 * time.Minute,
		},
		Limits: Limits{
			MaxSandboxes:       64,
			MaxSandboxesPerKey: 16,
			MaxSnapshotsPerKey: 8,
			MaxVCPU:            2,
			MaxMemoryMiB:       2048,
			MaxDiskMiB:         16384,
			MaxTTL:             24 * time.Hour,
			MaxExecTimeout:     30 * time.Minute,
			MaxFileBytes:       64 << 20,
			RequestsPerSecond:  20,
			RequestBurst:       40,
			DiskBandwidthMiBps: 200,
			NetBandwidthMbps:   200,
		},
		Pool: Pool{
			Size:              4,
			RefillConcurrency: 2,
		},
		Network: Network{
			Enabled:     true,
			CIDR:        "10.200.0.0/16",
			Nameservers: []string{"8.8.8.8", "1.0.0.1"},
			DenyCIDRs:   DefaultDenyCIDRs(),
		},
		Auth: Auth{APIKeysFile: "/etc/agent-comp/api-keys"}, //nolint:gosec // a path, not a credential
	}
}

// Load reads a YAML file on top of Default and validates the result.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading config: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

// Image returns the image with the given name.
func (c *Config) Image(name string) (Image, bool) {
	for _, img := range c.Images {
		if img.Name == name {
			return img, true
		}
	}
	return Image{}, false
}

func (c *Config) Validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}

	_, err := netip.ParseAddrPort(c.Listen)
	check(err == nil, "listen %q: must be ip:port", c.Listen)
	if c.MetricsListen != "" {
		_, err := netip.ParseAddrPort(c.MetricsListen)
		check(err == nil, "metrics_listen %q: must be ip:port", c.MetricsListen)
	}
	if c.MetricsListen != "" {
		_, err := netip.ParseAddrPort(c.MetricsListen)
		check(err == nil, "metrics_listen %q: must be ip:port", c.MetricsListen)
	}
	check(c.DataDir != "", "data_dir is required")
	check(c.Firecracker.Binary != "", "firecracker.binary is required")
	check(c.Firecracker.JailerBinary != "", "firecracker.jailer_binary is required")
	check(c.Firecracker.ChrootBase != "", "firecracker.chroot_base is required")
	check(c.Firecracker.UIDBase > 0, "firecracker.uid_base must be > 0")

	check(len(c.Images) > 0, "at least one image is required")
	seen := map[string]bool{}
	for i, img := range c.Images {
		check(img.Name != "", "images[%d].name is required", i)
		check(!seen[img.Name], "images[%d]: duplicate name %q", i, img.Name)
		check(img.Kernel != "" && img.Rootfs != "", "images[%d] (%s): kernel and rootfs are required", i, img.Name)
		seen[img.Name] = true
	}

	s, l := c.Sandbox, c.Limits
	check(s.VCPU >= 1 && s.VCPU <= l.MaxVCPU, "sandbox.vcpu must be in [1, limits.max_vcpu]")
	check(s.MemoryMiB >= 128 && s.MemoryMiB <= l.MaxMemoryMiB, "sandbox.memory_mib must be in [128, limits.max_memory_mib]")
	check(s.DiskMiB >= 64 && s.DiskMiB <= l.MaxDiskMiB, "sandbox.disk_mib must be in [64, limits.max_disk_mib]")
	check(s.TTL > 0 && s.TTL <= l.MaxTTL, "sandbox.ttl must be in (0, limits.max_ttl]")
	check(s.IdleTimeout > 0, "sandbox.idle_timeout must be > 0")
	check(l.MaxSandboxes > 0, "limits.max_sandboxes must be > 0")
	check(l.MaxSandboxesPerKey > 0, "limits.max_sandboxes_per_key must be > 0")
	check(l.MaxSnapshotsPerKey >= 0, "limits.max_snapshots_per_key must be >= 0")
	check(l.MaxFileBytes > 0, "limits.max_file_bytes must be > 0")
	check(l.MaxExecTimeout > 0, "limits.max_exec_timeout must be > 0")
	check(l.RequestsPerSecond > 0 && l.RequestBurst >= 1, "limits.requests_per_second must be > 0 and limits.request_burst >= 1")

	check(c.Pool.Size >= 0, "pool.size must be >= 0")
	check(c.Pool.RefillConcurrency >= 1, "pool.refill_concurrency must be >= 1")

	if c.Network.Enabled {
		prefix, err := netip.ParsePrefix(c.Network.CIDR)
		check(err == nil && prefix.Addr().Is4() && prefix.Bits() <= 28,
			"network.cidr %q: must be an IPv4 prefix of /28 or larger", c.Network.CIDR)
		for _, ns := range c.Network.Nameservers {
			_, err := netip.ParseAddr(ns)
			check(err == nil, "network.nameservers: invalid address %q", ns)
		}
		for _, d := range c.Network.DenyCIDRs {
			_, err := netip.ParsePrefix(d)
			check(err == nil, "network.deny_cidrs: invalid prefix %q", d)
		}
	}

	return errors.Join(errs...)
}
