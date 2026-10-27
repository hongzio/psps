package pf

import (
	"net/netip"
	"strings"
	"testing"
)

var dev = Network{Name: "gero-dev", Subnet4: "192.168.65.0/24", Gateway4: "192.168.65.1", Subnet6: "fd62::/64"}

func bridge(name string) func(string) string { return func(string) string { return name } }

func TestRulesMatchTheMeasuredFence(t *testing.T) {
	host := []HostAddr{
		{"en0", netip.MustParsePrefix("192.168.0.2/24")},
		{"utun4", netip.MustParsePrefix("100.66.116.81/32")},
		{"bridge101", netip.MustParsePrefix("192.168.65.1/24")},
	}
	got, err := Rules([]Network{dev}, host, bridge("bridge101"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"table <gero_private4> const {",
		"pass  quick inet proto { udp tcp } from 192.168.65.0/24 to 192.168.65.1 port 53",
		"block drop quick inet from 192.168.65.0/24 to <gero_private4>",
		"pass  quick on bridge101 inet6 proto icmp6",
		"block drop in quick on bridge101 inet6 to { ::1 fc00::/7 fe80::/10 ff00::/8 }",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Every host address here is private: no per-address tables needed.
	if strings.Contains(got, "gero_host4") || strings.Contains(got, "gero_host6") {
		t.Errorf("private host addresses listed explicitly:\n%s", got)
	}
	// DNS must be allowed before the gateway is blocked.
	if strings.Index(got, "port 53") > strings.Index(got, "<gero_private4>\n") {
		t.Error("DNS exception comes after the block")
	}
}

func TestPublicHostAddressesAreFenced(t *testing.T) {
	host := []HostAddr{
		{"en0", netip.MustParsePrefix("203.0.113.7/24")},
		{"en0", netip.MustParsePrefix("2001:db8:1:2::5/64")},
		{"en0", netip.MustParsePrefix("fe80::1/64")},
	}
	got, _ := Rules([]Network{dev}, host, bridge("bridge101"))
	for _, want := range []string{
		"table <gero_host4> { 203.0.113.0/24 203.0.113.7 }",
		"table <gero_host6> { 2001:db8:1:2::/64 2001:db8:1:2::5 }",
		"block drop quick inet from 192.168.65.0/24 to <gero_host4>",
		"block drop in quick on bridge101 inet6 to <gero_host6>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestIPv6WaitsForTheBridge(t *testing.T) {
	got, _ := Rules([]Network{dev}, nil, bridge(""))
	if strings.Contains(got, "inet6") {
		t.Errorf("IPv6 rules without an interface:\n%s", got)
	}
	if !strings.Contains(got, "block drop quick inet from 192.168.65.0/24") {
		t.Errorf("IPv4 fence must not wait for the bridge:\n%s", got)
	}
}

func TestBadNetworkIsRejected(t *testing.T) {
	bad := dev
	bad.Gateway4 = "10.0.0.1"
	if _, err := Rules([]Network{bad}, nil, bridge("")); err == nil {
		t.Fatal("gateway outside the subnet accepted")
	}
}

func TestNetworkNamesCannotInjectRules(t *testing.T) {
	bad := dev
	bad.Name = "x\npass quick all"
	if _, err := Rules([]Network{bad}, nil, bridge("")); err == nil {
		t.Fatal("a name with a newline was accepted")
	}
}

func TestCoversBridgeReadsTheLoadedRules(t *testing.T) {
	// What pfctl printed on the Mac for one fenced network.
	loaded := []string{
		"pass quick inet proto udp from 192.168.65.0/24 to 192.168.65.1 port = 53 keep state",
		"block drop quick inet from 192.168.65.0/24 to <gero_private4>",
		"pass quick on bridge101 inet6 proto ipv6-icmp all icmp6-type neighbrsol keep state",
		"block drop in quick on bridge101 inet6 from any to ::1",
		"block drop in quick on bridge101 inet6 from any to fc00::/7",
		"block drop in quick on bridge101 inet6 from any to fe80::/10",
		"block drop in quick on bridge101 inet6 from any to ff00::/8",
	}
	if !CoversBridge(loaded, "bridge101") {
		t.Fatal("the loaded rules cover bridge101")
	}
	for _, br := range []string{"bridge102", "bridge10", ""} {
		if CoversBridge(loaded, br) {
			t.Errorf("%q is not covered", br)
		}
	}
	// Rules loaded before the bridge existed have no IPv6 part at all.
	if CoversBridge(loaded[:2], "bridge101") {
		t.Fatal("IPv4-only rules cover no bridge")
	}
	// One destination missing is not coverage.
	if CoversBridge(append(loaded[:5:5], loaded[6]), "bridge101") {
		t.Fatal("a missing fe80::/10 block passed")
	}
}
