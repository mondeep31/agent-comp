package network

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestAllocatorAssignsDistinctSlash30s(t *testing.T) {
	a, err := newAllocator(netip.MustParsePrefix("10.200.0.0/28"))
	if err != nil {
		t.Fatal(err)
	}
	l0, _ := a.allocate("a")
	l1, _ := a.allocate("b")
	if l0.hostIP.String() != "10.200.0.1" || l0.nsIP.String() != "10.200.0.2" {
		t.Errorf("slot 0 = %s/%s", l0.hostIP, l0.nsIP)
	}
	if l1.hostIP.String() != "10.200.0.5" || l1.nsIP.String() != "10.200.0.6" {
		t.Errorf("slot 1 = %s/%s", l1.hostIP, l1.nsIP)
	}
	if l0.hostIf != "acv0000" || l1.nsIf != "acp0001" || l0.ns != "ac-a" {
		t.Errorf("names = %s %s %s", l0.hostIf, l1.nsIf, l0.ns)
	}
	if again, _ := a.allocate("a"); again != l0 {
		t.Error("allocate is not idempotent per id")
	}
	_, _ = a.allocate("c")
	_, _ = a.allocate("d")
	if _, err := a.allocate("e"); !errors.Is(err, errExhausted) {
		t.Fatalf("want exhaustion on a /28, got %v", err)
	}
	a.release("b")
	if l, _ := a.allocate("e"); l.slot != 1 {
		t.Errorf("released slot not reused: got %d", l.slot)
	}
}

func TestAllocatorRejectsSmallOrV6Prefixes(t *testing.T) {
	for _, p := range []string{"10.0.0.0/30", "fd00::/64"} {
		if _, err := newAllocator(netip.MustParsePrefix(p)); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
}

func TestAllocatorWrapsAcrossOctets(t *testing.T) {
	a, _ := newAllocator(netip.MustParsePrefix("10.200.0.0/16"))
	l := a.lease("x", 64)
	if l.hostIP.String() != "10.200.1.1" {
		t.Errorf("slot 64 host ip = %s", l.hostIP)
	}
}

func TestNamespaceRulesDenyBeforeAccept(t *testing.T) {
	l := lease{nsIf: "acp0007", nsIP: netip.MustParseAddr("10.200.0.30")}
	rules := nsRules(l, []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
	})
	for _, want := range []string{
		"elements = { 100.64.0.0/10 }",
		"elements = { fd7a:115c:a1e0::/48 }",
		"snat ip to 10.200.0.30",
		`iifname "tap0" oifname "acp0007" counter accept`,
		"chain prerouting",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("rules missing %q:\n%s", want, rules)
		}
	}
	if strings.Index(rules, "@deny4") > strings.Index(rules, `oifname "acp0007" counter accept`) {
		t.Error("deny rule must come before the accept rule")
	}
}

func TestHostRulesAreReplacedAtomically(t *testing.T) {
	r := hostRules(netip.MustParsePrefix("10.200.0.0/16"), "eth0")
	if !strings.HasPrefix(r, "table inet agentcomp\ndelete table inet agentcomp\n") {
		t.Errorf("host rules must recreate the table in one transaction:\n%s", r)
	}
	for _, want := range []string{`iifname "acv*" counter drop`, `oifname != "eth0"`, "ip saddr 10.200.0.0/16"} {
		if !strings.Contains(r, want) {
			t.Errorf("host rules missing %q", want)
		}
	}
}

func TestKernelArgs(t *testing.T) {
	c := Config{Nameservers: []string{"1.1.1.1", "8.8.8.8"}}
	if got := c.KernelArgs(); got != "ac.ip=172.16.0.2/30 ac.gw=172.16.0.1 ac.dns=1.1.1.1,8.8.8.8" {
		t.Errorf("KernelArgs = %q", got)
	}
}
