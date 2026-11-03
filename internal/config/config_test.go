package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validConfig() Config {
	cfg := Default()
	cfg.Images = []Image{{Name: "base", Kernel: "/k", Rootfs: "/r"}}
	return cfg
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "defaults with an image are valid", mutate: func(*Config) {}},
		{name: "listen must be ip:port", mutate: func(c *Config) { c.Listen = "localhost" }, wantErr: "listen"},
		{name: "needs an image", mutate: func(c *Config) { c.Images = nil }, wantErr: "at least one image"},
		{
			name:    "duplicate image names",
			mutate:  func(c *Config) { c.Images = append(c.Images, c.Images[0]) },
			wantErr: "duplicate name",
		},
		{name: "memory floor", mutate: func(c *Config) { c.Sandbox.MemoryMiB = 64 }, wantErr: "memory_mib"},
		{name: "vcpu above limit", mutate: func(c *Config) { c.Sandbox.VCPU = 8 }, wantErr: "vcpu"},
		{name: "ttl above max", mutate: func(c *Config) { c.Sandbox.TTL = 48 * time.Hour }, wantErr: "ttl"},
		{name: "ipv6 pool rejected", mutate: func(c *Config) { c.Network.CIDR = "fd00::/64" }, wantErr: "network.cidr"},
		{name: "tiny pool rejected", mutate: func(c *Config) { c.Network.CIDR = "10.200.0.0/30" }, wantErr: "network.cidr"},
		{name: "bad deny prefix", mutate: func(c *Config) { c.Network.DenyCIDRs = []string{"nope"} }, wantErr: "deny_cidrs"},
		{
			name: "network checks skipped when disabled",
			mutate: func(c *Config) {
				c.Network.Enabled = false
				c.Network.CIDR = "garbage"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestDefaultDenyCoversTailnetAndMetadata(t *testing.T) {
	deny := strings.Join(DefaultDenyCIDRs(), ",")
	for _, must := range []string{"100.64.0.0/10", "169.254.0.0/16", "10.0.0.0/8", "fd7a:115c:a1e0::/48"} {
		if !strings.Contains(deny, must) {
			t.Errorf("default deny list is missing %s", must)
		}
	}
}

func TestLoadOverlaysDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acd.yaml")
	data := `
listen: 127.0.0.1:9000
images:
  - name: base
    kernel: /var/lib/agent-comp/images/base/vmlinux
    rootfs: /var/lib/agent-comp/images/base/rootfs.ext4
sandbox:
  ttl: 30m
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9000" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.Sandbox.TTL != 30*time.Minute {
		t.Errorf("TTL = %v, want 30m", cfg.Sandbox.TTL)
	}
	if cfg.Sandbox.MemoryMiB != Default().Sandbox.MemoryMiB {
		t.Errorf("MemoryMiB default not kept: %d", cfg.Sandbox.MemoryMiB)
	}
	if _, ok := cfg.Image("base"); !ok {
		t.Error("image base not found")
	}
}
