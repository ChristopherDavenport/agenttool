package mcpserver

import "net/netip"

// specialPrefixes are the address blocks of the IANA IPv4 and IPv6
// Special-Purpose Address Registries that are not globally routable, and
// a few the registries mark reachable that a webhook has no business
// calling (the 192.88.99.0/24 relay, the whole of 2001::/23). A callback
// that resolves to one is refused, whatever the DNS says.
//
// The list is stricter than the registries where a block holds both
// kinds: a refused callback is the cheap mistake. IPv4-mapped IPv6
// addresses (::ffff:0:0/96) and NAT64 ones (64:ff9b::/96) are judged by
// the IPv4 address they carry, see [isSpecialAddr].
var specialPrefixes = mustPrefixes(
	// IPv4.
	"0.0.0.0/8",       // "this network", includes 0.0.0.0
	"10.0.0.0/8",      // private use
	"100.64.0.0/10",   // shared address space, CGNAT
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link local, includes the 169.254.169.254 metadata address
	"172.16.0.0/12",   // private use
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation (TEST-NET-1)
	"192.88.99.0/24",  // deprecated 6to4 relay anycast
	"192.168.0.0/16",  // private use
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation (TEST-NET-2)
	"203.0.113.0/24",  // documentation (TEST-NET-3)
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, includes 255.255.255.255
	// IPv6.
	"::/96",          // unspecified, loopback and the deprecated IPv4-compatible block
	"64:ff9b:1::/48", // local-use IPv4/IPv6 translation
	"100::/64",       // discard-only
	"100:0:0:1::/64", // dummy IPv6 prefix
	"2001::/23",      // IETF protocol assignments, includes Teredo
	"2001:db8::/32",  // documentation
	"2002::/16",      // 6to4
	"3fff::/20",      // documentation
	"5f00::/16",      // segment routing SIDs
	"fc00::/7",       // unique local
	"fe80::/10",      // link local
	"fec0::/10",      // deprecated site local
	"ff00::/8",       // multicast
)

func mustPrefixes(ps ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ps))
	for i, p := range ps {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

// nat64 is the well-known prefix whose addresses carry an IPv4 address
// in their last 32 bits.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// isSpecialAddr says whether a is not for a webhook to call: an address
// of a special-purpose block. An IPv4-mapped IPv6 address is judged as
// the IPv4 address in it, a NAT64 one by the IPv4 address in its last
// four bytes, and an invalid address is special.
func isSpecialAddr(a netip.Addr) bool {
	if !a.IsValid() {
		return true
	}
	a = a.WithZone("").Unmap()
	if a.Is6() && nat64.Contains(a) {
		b := a.As16()
		a = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	for _, p := range specialPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
