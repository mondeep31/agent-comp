//go:build linux && integration

package network_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentdv1 "github.com/mondeep31/agent-comp/gen/go/agentcomp/agentd/v1"
	"github.com/mondeep31/agent-comp/internal/config"
	"github.com/mondeep31/agent-comp/internal/golden"
	"github.com/mondeep31/agent-comp/internal/network"
	"github.com/mondeep31/agent-comp/internal/pool"
	"github.com/mondeep31/agent-comp/internal/testenv"
	"github.com/mondeep31/agent-comp/internal/vmm/firecracker"
)

func sh(ctx context.Context, t *testing.T, inst *pool.Instance, script string) (int32, string) {
	t.Helper()
	var out bytes.Buffer
	exit, err := inst.Guest.Run(ctx, &agentdv1.ExecStart{
		Argv: []string{"bash", "-c", script}, TimeoutMs: 20_000,
	}, nil, &out, &out)
	if err != nil {
		t.Fatalf("%q: %v", script, err)
	}
	return exit.GetExitCode(), strings.TrimSpace(out.String())
}

// diagnose logs both sides of a sandbox's network path.
func diagnose(ctx context.Context, t *testing.T, inst *pool.Instance) {
	t.Helper()
	_, guestView := sh(ctx, t, inst, "ip addr; ip route; ip neigh; cat /etc/resolv.conf; ping -c1 -W1 172.16.0.1")
	t.Logf("guest view:\n%s", guestView)
	ns := "--net=/var/run/netns/act-" + inst.ID
	for _, args := range [][]string{
		{"nsenter", ns, "--", "nft", "list", "ruleset"},
		{"nsenter", ns, "--", "ip", "-s", "link"},
		{"nsenter", ns, "--", "ip", "route"},
		{"nsenter", ns, "--", "sysctl", "net.ipv4.ip_forward"},
		{"nft", "list", "table", "inet", "agentcomp_t"},
		{"nsenter", ns, "--", "cat", "/proc/net/nf_conntrack"},
		{"iptables-save"},
		{"nft", "list", "tables"},
		{"sysctl", "net.ipv4.ip_forward"},
		{"ip", "route"},
	} {
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		t.Logf("$ %s (err=%v)\n%s", strings.Join(args, " "), err, out)
	}
}

// hostAddrs returns the host's own IPv4 addresses on non-loopback interfaces.
func hostAddrs(t *testing.T) []string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ip", "-4", "-o", "addr", "show").Output()
	if err != nil {
		t.Fatal(err)
	}
	var addrs []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[1] == "lo" || strings.HasPrefix(f[1], "acv") {
			continue
		}
		if p, err := netip.ParsePrefix(f[3]); err == nil {
			addrs = append(addrs, p.Addr().String())
		}
	}
	return addrs
}

func TestSandboxEgressPolicy(t *testing.T) {
	ci := testenv.CI(t)
	agentd, err := os.ReadFile(filepath.Join(testenv.RepoRoot(), "bin", "agentd"))
	if err != nil {
		t.Skipf("bin/agentd missing (make build-agentd): %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	cfg, err := network.ParseConfig("t", "10.201.0.0/24", "", config.Default().Network.Nameservers, config.DefaultDenyCIDRs(), 0)
	if err != nil {
		t.Fatal(err)
	}
	netp, err := network.New(ctx, cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	rootfs := filepath.Join(dir, "rootfs.ext4")
	testenv.Rootfs(t, ci, rootfs, map[string]testenv.File{
		"/usr/local/bin/agentd": {Data: agentd, Mode: 0o755},
	})
	d, err := firecracker.New(firecracker.Options{
		FirecrackerBin: "/usr/local/bin/firecracker",
		JailerBin:      "/usr/local/bin/jailer",
		ChrootBase:     filepath.Join(dir, "jail"),
		UIDBase:        340000,
		CgroupParent:   "agent-comp-test-network",
		MaxVMs:         8,
	})
	if err != nil {
		t.Fatal(err)
	}
	g, err := golden.New(golden.Options{
		Driver: d, Network: netp, Dir: filepath.Join(dir, "golden"),
		Images: []golden.Image{{Name: "ci", Kernel: filepath.Join(ci, "vmlinux"), Rootfs: rootfs}},
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := pool.New(pool.Options{Driver: d, Snapshots: g, Network: netp, Dir: filepath.Join(dir, "vms")})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(context.WithoutCancel(ctx))

	key := golden.Key{Image: "ci", Shape: golden.Shape{VCPU: 1, MemoryMiB: 256, DiskMiB: 512}}
	if _, err := g.Get(ctx, key); err != nil {
		t.Fatal(err)
	}
	acquire := func() *pool.Instance {
		start := time.Now()
		inst, err := p.Acquire(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if err := inst.Guest.WaitReady(ctx); err != nil {
			t.Fatal(err)
		}
		t.Logf("on-demand restore with network to agent ready: %v", time.Since(start))
		return inst
	}
	a, b := acquire(), acquire()

	if code, out := sh(ctx, t, a, "ip -4 -o addr show eth0"); code != 0 || !strings.Contains(out, "172.16.0.2/30") {
		t.Fatalf("guest eth0: %d %q", code, out)
	}

	t.Run("internet egress works", func(t *testing.T) {
		if code, out := sh(ctx, t, a, "getent hosts example.com"); code != 0 {
			t.Errorf("DNS failed: %q", out)
		}
		code, out := sh(ctx, t, a, "curl -sS -o /dev/null -w '%{http_code}' --max-time 10 https://example.com/")
		if code != 0 || out != "200" {
			t.Errorf("HTTPS to example.com: exit %d, %q", code, out)
		}
		// Clones restored from one snapshot must not share a broken network.
		if code, out := sh(ctx, t, b, "curl -sS -o /dev/null --max-time 10 https://example.com/"); code != 0 {
			t.Errorf("second sandbox egress failed: %q", out)
		}
		if t.Failed() {
			diagnose(ctx, t, a)
		}
	})

	blocked := map[string]string{
		"gateway in netns": "172.16.0.1:22",
		"cloud metadata":   "169.254.169.254:80",
		"other sandbox":    "10.201.0.6:22",
		"private range":    "192.168.1.1:80",
	}
	for _, addr := range hostAddrs(t) {
		blocked["host "+addr] = addr + ":22"
	}
	for name, target := range blocked {
		t.Run("blocked "+name, func(t *testing.T) {
			host, port, _ := strings.Cut(target, ":")
			script := "timeout 3 bash -c 'exec 3<>/dev/tcp/" + host + "/" + port + "' 2>&1"
			if code, out := sh(ctx, t, a, script); code == 0 {
				t.Errorf("connected to %s: %q", target, out)
			}
		})
	}

	for _, inst := range []*pool.Instance{a, b} {
		if err := inst.Destroy(ctx); err != nil {
			t.Error(err)
		}
	}
	entries, _ := os.ReadDir("/var/run/netns")
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "act-") {
			t.Errorf("namespace left behind: %s", e.Name())
		}
	}
}
