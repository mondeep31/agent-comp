package network

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/mondeep31/agent-comp/internal/vmm"
)

// Provider implements vmm.NetworkProvider with network namespaces, veth pairs
// and nftables. It must run as root.
type Provider struct {
	cfg   Config
	alloc *allocator
	log   *slog.Logger
}

var _ vmm.NetworkProvider = (*Provider)(nil)

// New installs the host ruleset, enables forwarding and removes namespaces
// left behind by a previous run.
func New(ctx context.Context, cfg Config, log *slog.Logger) (*Provider, error) {
	if log == nil {
		log = slog.Default()
	}
	alloc, err := newAllocator(cfg.Instance, cfg.CIDR)
	if err != nil {
		return nil, err
	}
	if cfg.Uplink == "" {
		if cfg.Uplink, err = defaultUplink(ctx); err != nil {
			return nil, err
		}
	}
	p := &Provider{cfg: cfg, alloc: alloc, log: log.With("component", "network")}
	p.cleanupStale(ctx)
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o600); err != nil {
		return nil, fmt.Errorf("network: enabling ip_forward: %w", err)
	}
	if err := run(ctx, hostRules(cfg.Instance, cfg.CIDR, cfg.Uplink), "nft", "-f", "-"); err != nil {
		return nil, fmt.Errorf("network: installing host rules: %w", err)
	}
	p.log.Info("network ready", "instance", cfg.Instance, "cidr", cfg.CIDR.String(), "uplink", cfg.Uplink)
	return p, nil
}

func (p *Provider) KernelArgs() string { return p.cfg.KernelArgs() }

// Attach creates the namespace, veth pair and tap for one VM.
func (p *Provider) Attach(ctx context.Context, id string) (_ *vmm.Network, err error) {
	l, err := p.alloc.allocate(id)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = p.Detach(context.WithoutCancel(ctx), id)
		}
	}()

	host := strings.Join([]string{
		"netns add " + l.ns,
		fmt.Sprintf("link add %s type veth peer name %s netns %s", l.hostIf, l.nsIf, l.ns),
		fmt.Sprintf("addr add %s/30 dev %s", l.hostIP, l.hostIf),
		fmt.Sprintf("link set %s up", l.hostIf),
	}, "\n") + "\n"
	if err := run(ctx, host, "ip", "-batch", "-"); err != nil {
		return nil, fmt.Errorf("network: host side of %s: %w", id, err)
	}
	inside := strings.Join([]string{
		"link set lo up",
		"tuntap add dev " + TapName + " mode tap",
		"addr add " + GatewayIP + "/30 dev " + TapName,
		"link set " + TapName + " up",
		fmt.Sprintf("addr add %s/30 dev %s", l.nsIP, l.nsIf),
		fmt.Sprintf("link set %s up", l.nsIf),
		fmt.Sprintf("route add default via %s", l.hostIP),
	}, "\n") + "\n"
	if err := run(ctx, inside, "ip", "-n", l.ns, "-batch", "-"); err != nil {
		return nil, fmt.Errorf("network: namespace side of %s: %w", id, err)
	}
	if err := inNetns(l.nsPath(), func() error {
		for path, v := range map[string]string{
			"/proc/sys/net/ipv4/ip_forward":                "1",
			"/proc/sys/net/ipv6/conf/all/disable_ipv6":     "1",
			"/proc/sys/net/ipv6/conf/default/disable_ipv6": "1",
		} {
			if err := os.WriteFile(path, []byte(v), 0o600); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("network: sysctls for %s: %w", id, err)
	}
	if err := run(ctx, nsRules(l, p.cfg.DenyCIDRs), "nsenter", "--net="+l.nsPath(), "--", "nft", "-f", "-"); err != nil {
		return nil, fmt.Errorf("network: rules for %s: %w", id, err)
	}
	return &vmm.Network{
		NetNSPath:     l.nsPath(),
		TapName:       TapName,
		GuestMAC:      GuestMAC,
		BandwidthMbps: p.cfg.BandwidthMbps,
	}, nil
}

// Detach deletes the namespace, which takes the veth pair and tap with it.
func (p *Provider) Detach(ctx context.Context, id string) error {
	l, ok := p.alloc.get(id)
	if !ok {
		return nil
	}
	err := run(ctx, "", "ip", "netns", "del", l.ns)
	if err != nil && !strings.Contains(err.Error(), "No such file") {
		return fmt.Errorf("network: deleting %s: %w", l.ns, err)
	}
	// The host veth normally disappears with its peer; remove it if it lingers.
	_ = run(ctx, "", "ip", "link", "del", l.hostIf)
	p.alloc.release(id)
	return nil
}

// cleanupStale removes namespaces and veths from a previous process.
func (p *Provider) cleanupStale(ctx context.Context) {
	entries, _ := os.ReadDir("/var/run/netns")
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), nsPrefix(p.cfg.Instance)) {
			if err := run(ctx, "", "ip", "netns", "del", e.Name()); err == nil {
				p.log.Info("removed stale namespace", "netns", e.Name())
			}
		}
	}
	links, _ := os.ReadDir("/sys/class/net")
	for _, e := range links {
		if strings.HasPrefix(e.Name(), hostIfPrefix(p.cfg.Instance)) {
			_ = run(ctx, "", "ip", "link", "del", e.Name())
		}
	}
}

func defaultUplink(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "ip", "-o", "route", "show", "default").Output()
	if err != nil {
		return "", fmt.Errorf("network: reading default route: %w", err)
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", errors.New("network: no default route; set network.uplink")
}

// inNetns runs fn on an OS thread switched into the namespace at path.
func inNetns(path string, fn func() error) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// The thread is never unlocked: if restoring the original namespace
		// failed it must die with the goroutine rather than be reused.
		orig, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			errc <- err
			return
		}
		defer orig.Close()
		target, err := os.Open(path)
		if err != nil {
			errc <- err
			return
		}
		defer target.Close()
		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			errc <- fmt.Errorf("setns: %w", err)
			return
		}
		ferr := fn()
		if err := unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET); err != nil {
			errc <- errors.Join(ferr, fmt.Errorf("restoring netns: %w", err))
			return
		}
		runtime.UnlockOSThread()
		errc <- ferr
	}()
	return <-errc
}

func run(ctx context.Context, stdin, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed tools (ip, nft, nsenter); args are generated

	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return nil
}
