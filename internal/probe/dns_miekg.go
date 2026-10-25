package probe

import (
	"context"
	"net"
	"net/netip"
	"time"

	"github.com/miekg/dns"
)

// MiekgDNS implements DNSQuerier with a direct UDP query (DNS-02): it
// bypasses the Windows DNS client and its cache entirely. A truncated
// answer without records is retried over TCP.
type MiekgDNS struct {
	Port string // default "53"; tests use a loopback server
}

func (m MiekgDNS) Query(ctx context.Context, server netip.Addr, name string, timeout time.Duration) (DNSAnswer, error) {
	port := m.Port
	if port == "" {
		port = "53"
	}
	addr := net.JoinHostPort(server.String(), port)
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), dns.TypeA)
	q.RecursionDesired = true

	c := &dns.Client{Net: "udp", Timeout: timeout}
	r, _, err := c.ExchangeContext(ctx, q, addr)
	if err == nil && r.Truncated && len(r.Answer) == 0 {
		c.Net = "tcp"
		r, _, err = c.ExchangeContext(ctx, q, addr)
	}
	if err != nil {
		return DNSAnswer{}, err
	}
	ans := DNSAnswer{}
	if r.Rcode != dns.RcodeSuccess {
		ans.Rcode = dns.RcodeToString[r.Rcode]
		return ans, nil
	}
	for _, rr := range r.Answer {
		if a, ok := rr.(*dns.A); ok {
			if ip, ok := netip.AddrFromSlice(a.A.To4()); ok {
				ans.Addrs = append(ans.Addrs, ip)
			}
		}
	}
	return ans, nil
}
