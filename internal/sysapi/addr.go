package sysapi

import "net/netip"

var (
	apipaPrefix     = netip.MustParsePrefix("169.254.0.0/16")
	loopbackPrefix  = netip.MustParsePrefix("127.0.0.0/8")
	privatePrefixes = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
	}
)

// IsAPIPA reports whether a is an IPv4 link-local (169.254.0.0/16) address.
func IsAPIPA(a netip.Addr) bool {
	return a.Is4() && apipaPrefix.Contains(a)
}

// IsPrivateIPv4 reports whether a is in an RFC 1918 range.
func IsPrivateIPv4(a netip.Addr) bool {
	if !a.Is4() {
		return false
	}
	for _, p := range privatePrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// IsLocalIPv4 reports whether a is reachable without the internet:
// RFC 1918, loopback or APIPA. CGNAT (100.64.0.0/10) is not local.
func IsLocalIPv4(a netip.Addr) bool {
	return IsPrivateIPv4(a) || IsAPIPA(a) || (a.Is4() && loopbackPrefix.Contains(a))
}
