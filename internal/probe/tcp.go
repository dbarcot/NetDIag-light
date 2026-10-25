package probe

import (
	"context"
	"net"
	"net/netip"
	"time"
)

// Dialer opens a connection; *net.Dialer implements it.
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// TCPProbe measures the TCP handshake time to host:port (NET-04, TEAMS-02,
// custom TCP targets). A hostname is resolved first with Resolver; every
// returned address is tried in order within the one timeout, and the
// address that connected is reported in Sample.From.
type TCPProbe struct {
	Dialer   Dialer
	Resolver Resolver
	Addr     string // host:port
	Timeout  time.Duration
}

func (p TCPProbe) Run(ctx context.Context) Sample {
	host, port, err := net.SplitHostPort(p.Addr)
	if err != nil {
		return Sample{Result: Fail, ErrClass: ClassLocal, Detail: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	ips, err := p.Resolver.LookupIPv4(ctx, host)
	if err == nil && len(ips) == 0 {
		err = noAddress(host)
	}
	if err != nil {
		return resolveFailure(host, err)
	}
	var last Sample
	for _, ip := range ips {
		last = p.dial(ctx, ip, port)
		if last.Result == OK || ctx.Err() != nil {
			break
		}
	}
	return last
}

func (p TCPProbe) dial(ctx context.Context, ip netip.Addr, port string) Sample {
	elapsed := stopwatch()
	conn, err := p.Dialer.DialContext(ctx, "tcp4", net.JoinHostPort(ip.String(), port))
	rtt := elapsed()
	if err != nil {
		class, detail := ClassifyNetError(err)
		return Sample{Result: Fail, ErrClass: class, Detail: detail, From: ip}
	}
	_ = conn.Close()
	return Sample{Result: OK, RTT: rtt, From: ip}
}

// HostICMPProbe pings a host that may be a name (custom ICMP targets). The
// name is resolved with Resolver, which the caller makes a CachedResolver
// for the 60 s re-resolution of SPEC §4.2.
type HostICMPProbe struct {
	ICMP     ICMP
	Resolver Resolver
	Host     string
	Timeout  time.Duration
}

func (p HostICMPProbe) Run(ctx context.Context) Sample {
	ips, err := p.Resolver.LookupIPv4(ctx, p.Host)
	if err == nil && len(ips) == 0 {
		err = noAddress(p.Host)
	}
	if err != nil {
		return resolveFailure(p.Host, err)
	}
	return ICMPProbe{ICMP: p.ICMP, Dest: ips[0], Timeout: p.Timeout}.Run(ctx)
}
