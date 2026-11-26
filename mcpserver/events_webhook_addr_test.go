package mcpserver

import (
	"net/netip"
	"testing"
)

// TestEventsWebhookSpecialAddresses holds events:webhook:ssrf-reject-non-routable:
// every block of the special-purpose registries is refused at both ends,
// and the addresses beside them, which are routable, are not.
func TestEventsWebhookSpecialAddresses(t *testing.T) {
	for _, p := range specialPrefixes {
		first := p.Masked().Addr()
		last := lastAddr(p)
		for _, a := range []netip.Addr{first, last} {
			if !isSpecialAddr(a) {
				t.Errorf("%s of %s is not special", a, p)
			}
		}
	}
	tests := []struct {
		addr    string
		special bool
	}{
		// IPv4 edges: the last routable address below and the first above each block.
		{"9.255.255.255", false}, {"11.0.0.0", false},
		{"100.63.255.255", false}, {"100.128.0.0", false},
		{"126.255.255.255", false}, {"128.0.0.0", false},
		{"169.253.255.255", false}, {"169.255.0.0", false},
		{"172.15.255.255", false}, {"172.32.0.0", false},
		{"191.255.255.255", false}, {"192.0.1.0", false},
		{"192.0.3.0", false}, {"192.88.98.255", false}, {"192.88.100.0", false},
		{"192.167.255.255", false}, {"192.169.0.0", false},
		{"198.17.255.255", false}, {"198.20.0.0", false},
		{"198.51.99.255", false}, {"198.51.101.0", false},
		{"203.0.112.255", false}, {"203.0.114.0", false},
		{"223.255.255.255", false}, {"224.0.0.0", true}, {"239.255.255.255", true},
		{"240.0.0.0", true}, {"255.255.255.255", true},
		// Named addresses.
		{"0.0.0.0", true}, {"127.0.0.1", true}, {"10.1.2.3", true}, {"192.168.1.1", true},
		{"169.254.169.254", true}, {"100.100.100.200", true}, {"172.16.0.1", true}, {"172.31.255.255", true},
		{"8.8.8.8", false}, {"1.1.1.1", false}, {"93.184.216.34", false},
		// IPv6.
		{"::", true}, {"::1", true}, {"::2", true}, {"::8.8.8.8", true},
		{"2606:4700:4700::1111", false}, {"2001:4860:4860::8888", false}, {"2a00:1450:4001:81b::200e", false},
		{"2000::1", false}, {"2001:1::1", true}, {"2001:200::1", false}, {"2001:db8::1", true}, {"2001:db7::1", false},
		{"2002::1", true}, {"2003::1", false}, {"3fff::1", true}, {"3ffe::1", false}, {"3fff:1000::1", false},
		{"5f00::1", true}, {"5eff::1", false}, {"fc00::1", true}, {"fd12:3456::1", true}, {"fbff::1", false},
		{"fe80::1", true}, {"fe80::1%eth0", true}, {"febf::1", true}, {"fec0::1", true}, {"fe7f::1", false},
		{"ff02::1", true}, {"ff00::", true},
		{"100::1", true}, {"100:0:0:1::1", true}, {"100:0:0:2::1", false}, {"64:ff9b:1::1", true},
		// IPv4 inside IPv6: judged by the IPv4 address.
		{"::ffff:127.0.0.1", true}, {"::ffff:10.0.0.1", true}, {"::ffff:169.254.169.254", true},
		{"::ffff:8.8.8.8", false}, {"64:ff9b::7f00:1", true}, {"64:ff9b::a00:1", true}, {"64:ff9b::808:808", false},
	}
	for _, tc := range tests {
		a, err := netip.ParseAddr(tc.addr)
		if err != nil {
			t.Fatalf("%s: %v", tc.addr, err)
		}
		if got := isSpecialAddr(a); got != tc.special {
			t.Errorf("isSpecialAddr(%s) = %v, want %v", tc.addr, got, tc.special)
		}
	}
	if !isSpecialAddr(netip.Addr{}) {
		t.Error("the invalid address is not special")
	}
}

func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr()
	b := a.AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 1 << (7 - i%8)
	}
	out, _ := netip.AddrFromSlice(b)
	return out
}
