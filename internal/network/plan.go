// Package network gives every VM its own network namespace with a NATed path
// to the internet and nothing else.
//
// Layout for one VM (slot n, /30 number n inside the configured CIDR):
//
//	guest eth0 172.16.0.2/30 --- tap0 172.16.0.1/30          [netns ac<inst>-<id>]
//	                             acp<n> <cidr base + 4n + 2>/30
//	                                |  veth
//	                             acv<inst>-<n> <cidr base + 4n + 1>/30 [host netns]
//	                                |  forward, masquerade
//	                             uplink
//
// The guest side is identical in every namespace, which is what lets one
// golden snapshot restore anywhere. Inside the namespace, traffic from the
// guest is source-NATed to the veth address and filtered against the deny
// list; on the host, forwarding from acv* is only allowed to the uplink and
// sandboxes can never reach host-local addresses.
//
// Config.Instance tags every host-side name (namespaces, veths, the nftables
// table), so several providers can share a host, e.g. a test run next to the
// service. The trailing "-" keeps one instance's prefixes from matching
// another's.
package network

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"sync"
)

const (
	TapName    = "tap0"
	GuestMAC   = "06:00:ac:10:00:02"
	GuestCIDR  = "172.16.0.2/30"
	GatewayIP  = "172.16.0.1"
	nsIfPrefix = "acp"
)

var instanceRe = regexp.MustCompile(`^[a-z0-9]{0,4}$`)

func hostIfPrefix(instance string) string { return "acv" + instance + "-" }

func nsPrefix(instance string) string { return "ac" + instance + "-" }

func tableName(instance string) string {
	if instance == "" {
		return "agentcomp"
	}
	return "agentcomp_" + instance
}

// Config is the subset of acd's network configuration the provider needs.
type Config struct {
	// Instance is 0-4 lowercase letters or digits; "" for the service.
	Instance    string
	CIDR        netip.Prefix
	Uplink      string
	Nameservers []string
	DenyCIDRs   []netip.Prefix
	// BandwidthMbps is applied by Firecracker per direction; 0 disables.
	BandwidthMbps int
}

// KernelArgs configures the guest's eth0 at first boot.
func (c Config) KernelArgs() string {
	args := "ac.ip=" + GuestCIDR + " ac.gw=" + GatewayIP
	if len(c.Nameservers) > 0 {
		args += " ac.dns=" + strings.Join(c.Nameservers, ",")
	}
	return args
}

// lease is one VM's host-side addressing.
type lease struct {
	slot   int
	ns     string
	hostIf string
	nsIf   string
	hostIP netip.Addr
	nsIP   netip.Addr
}

func (l lease) nsPath() string { return "/var/run/netns/" + l.ns }

// allocator hands out /30 slots from the configured CIDR.
type allocator struct {
	instance string
	base     netip.Addr
	mu       sync.Mutex
	used     []bool
	byID     map[string]lease
	limit    int
}

var errExhausted = errors.New("network: address space exhausted")

func newAllocator(instance string, cidr netip.Prefix) (*allocator, error) {
	if !cidr.Addr().Is4() || cidr.Bits() > 28 {
		return nil, fmt.Errorf("network: %s must be an IPv4 prefix of /28 or larger", cidr)
	}
	n := 1 << (30 - cidr.Bits())
	// Interface names are capped at 15 bytes; four hex digits keep them short.
	n = min(n, 1<<16)
	return &allocator{instance: instance, base: cidr.Masked().Addr(), used: make([]bool, n), byID: map[string]lease{}, limit: n}, nil
}

func (a *allocator) get(id string) (lease, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	l, ok := a.byID[id]
	return l, ok
}

func (a *allocator) allocate(id string) (lease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if l, ok := a.byID[id]; ok {
		return l, nil
	}
	for slot, used := range a.used {
		if used {
			continue
		}
		a.used[slot] = true
		l := a.lease(id, slot)
		a.byID[id] = l
		return l, nil
	}
	return lease{}, errExhausted
}

func (a *allocator) lease(id string, slot int) lease {
	b := a.base.As4()
	off := binary.BigEndian.Uint32(b[:]) + uint32(slot)*4 //nolint:gosec // slot < 1<<16
	ip := func(delta uint32) netip.Addr {
		var v [4]byte
		binary.BigEndian.PutUint32(v[:], off+delta)
		return netip.AddrFrom4(v)
	}
	return lease{
		slot:   slot,
		ns:     nsPrefix(a.instance) + id,
		hostIf: fmt.Sprintf("%s%04x", hostIfPrefix(a.instance), slot),
		nsIf:   fmt.Sprintf("%s%04x", nsIfPrefix, slot),
		hostIP: ip(1),
		nsIP:   ip(2),
	}
}

func (a *allocator) release(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if l, ok := a.byID[id]; ok {
		delete(a.byID, id)
		a.used[l.slot] = false
	}
}

// hostRules is the host-namespace ruleset, replaced atomically at startup.
// The empty prerouting nat chain is required: without it, replies to
// masqueraded traffic are never translated back.
func hostRules(instance string, cidr netip.Prefix, uplink string) string {
	return fmt.Sprintf(`table inet %[4]s
delete table inet %[4]s
table inet %[4]s {
	chain input {
		type filter hook input priority filter - 10; policy accept;
		iifname "%[1]s*" counter drop
	}
	chain forward {
		type filter hook forward priority filter - 10; policy accept;
		iifname "%[1]s*" oifname != "%[2]s" counter drop
		iifname "%[1]s*" meta nfproto ipv6 counter drop
		oifname "%[1]s*" ct state established,related accept
		oifname "%[1]s*" counter drop
	}
	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
	}
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr %[3]s oifname "%[2]s" masquerade
	}
}
`, hostIfPrefix(instance), uplink, cidr.Masked(), tableName(instance))
}

// nsRules runs inside one VM's namespace.
func nsRules(l lease, deny []netip.Prefix) string {
	var v4, v6 []string
	for _, p := range deny {
		if p.Addr().Is4() {
			v4 = append(v4, p.Masked().String())
		} else {
			v6 = append(v6, p.Masked().String())
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "table inet sandbox {\n")
	if len(v4) > 0 {
		fmt.Fprintf(&sb, "\tset deny4 {\n\t\ttype ipv4_addr; flags interval; auto-merge;\n\t\telements = { %s }\n\t}\n", strings.Join(v4, ", "))
	}
	if len(v6) > 0 {
		fmt.Fprintf(&sb, "\tset deny6 {\n\t\ttype ipv6_addr; flags interval; auto-merge;\n\t\telements = { %s }\n\t}\n", strings.Join(v6, ", "))
	}
	fmt.Fprintf(&sb, "\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n")
	fmt.Fprintf(&sb, "\t\tiifname \"lo\" accept\n\t\tct state established,related accept\n\t\tcounter drop\n\t}\n")
	fmt.Fprintf(&sb, "\tchain forward {\n\t\ttype filter hook forward priority filter; policy drop;\n")
	fmt.Fprintf(&sb, "\t\tiifname %q oifname %q ct state established,related counter accept\n", l.nsIf, TapName)
	fmt.Fprintf(&sb, "\t\tiifname %q oifname %q meta nfproto ipv6 drop\n", TapName, l.nsIf)
	if len(v4) > 0 {
		fmt.Fprintf(&sb, "\t\tip daddr @deny4 counter drop\n")
	}
	if len(v6) > 0 {
		fmt.Fprintf(&sb, "\t\tip6 daddr @deny6 counter drop\n")
	}
	fmt.Fprintf(&sb, "\t\tiifname %q oifname %q counter accept\n\t\tcounter drop\n\t}\n", TapName, l.nsIf)
	// NAT replies are only translated back if a prerouting nat chain exists.
	fmt.Fprintf(&sb, "\tchain prerouting {\n\t\ttype nat hook prerouting priority dstnat; policy accept;\n\t}\n")
	fmt.Fprintf(&sb, "\tchain postrouting {\n\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
	fmt.Fprintf(&sb, "\t\toifname %q ip saddr 172.16.0.0/30 snat ip to %s\n\t}\n}\n", l.nsIf, l.nsIP)
	return sb.String()
}

// ParseConfig converts string settings into a Config.
func ParseConfig(instance, cidr, uplink string, nameservers, deny []string, bandwidthMbps int) (Config, error) {
	if !instanceRe.MatchString(instance) {
		return Config{}, fmt.Errorf("network: instance %q: want 0-4 lowercase letters or digits", instance)
	}
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return Config{}, fmt.Errorf("network: cidr: %w", err)
	}
	c := Config{Instance: instance, CIDR: p, Uplink: uplink, Nameservers: nameservers, BandwidthMbps: bandwidthMbps}
	for _, d := range deny {
		dp, err := netip.ParsePrefix(d)
		if err != nil {
			return Config{}, fmt.Errorf("network: deny cidr %q: %w", d, err)
		}
		c.DenyCIDRs = append(c.DenyCIDRs, dp)
	}
	return c, nil
}
