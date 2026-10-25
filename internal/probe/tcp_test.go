package probe

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"netdiag/internal/clock"
)

func listenTCP(t *testing.T) (addr string, port string) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return ln.Addr().String(), port
}

func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestTCPProbe_OKAndRefused(t *testing.T) {
	t.Parallel()
	open, _ := listenTCP(t)
	p := TCPProbe{Dialer: &net.Dialer{}, Resolver: SystemResolver{}, Addr: open, Timeout: time.Second}
	s := p.Run(context.Background())
	if s.Result != OK || s.RTT <= 0 || s.From != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("open port: %+v", s)
	}
	p.Addr = closedPort(t)
	s = p.Run(context.Background())
	if s.Result != Fail || s.ErrClass != ClassRefused {
		t.Errorf("closed port: %+v, want fail/refused", s)
	}
}

func TestTCPProbe_ResolvesNamesAndTriesEveryAddress(t *testing.T) {
	t.Parallel()
	open, port := listenTCP(t)
	_ = open
	res := &FakeResolver{}
	// 127.0.0.2 does not listen on this port: the second address connects
	res.Set("svc.test", netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("127.0.0.1"))
	p := TCPProbe{Dialer: &net.Dialer{}, Resolver: res, Addr: net.JoinHostPort("svc.test", port), Timeout: time.Second}
	s := p.Run(context.Background())
	if s.Result != OK || s.From != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("got %+v, want ok via 127.0.0.1", s)
	}
	p.Addr = net.JoinHostPort("missing.test", port)
	s = p.Run(context.Background())
	if s.Result != Fail || s.ErrClass != ClassDNS {
		t.Errorf("unresolvable: %+v, want fail/dns", s)
	}
	p.Addr = "no-port"
	if s = p.Run(context.Background()); s.ErrClass != ClassLocal {
		t.Errorf("bad address: %+v, want local_error", s)
	}
}

type blockingDialer struct{}

func (blockingDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestTCPProbe_Timeout(t *testing.T) {
	t.Parallel()
	p := TCPProbe{Dialer: blockingDialer{}, Resolver: SystemResolver{}, Addr: "10.255.255.1:443", Timeout: 50 * time.Millisecond}
	s := p.Run(context.Background())
	if s.Result != Fail || s.ErrClass != ClassTimeout {
		t.Errorf("got %+v, want fail/timeout", s)
	}
}

func TestHostICMPProbe_ResolvesAndPings(t *testing.T) {
	t.Parallel()
	dest := netip.MustParseAddr("10.0.0.5")
	icmp := &FakeICMP{Reply: func(req EchoRequest) (EchoReply, error) {
		return EchoReply{Status: StatusSuccess, From: req.Dest, RTT: 3 * time.Millisecond}, nil
	}}
	res := &FakeResolver{}
	res.Set("nas.test", dest)
	p := HostICMPProbe{ICMP: icmp, Resolver: res, Host: "nas.test", Timeout: time.Second}
	if s := p.Run(context.Background()); s.Result != OK || s.From != dest {
		t.Errorf("got %+v", s)
	}
	p.Host = "gone.test"
	if s := p.Run(context.Background()); s.Result != Fail || s.ErrClass != ClassDNS {
		t.Errorf("unresolvable: %+v", s)
	}
}

// Custom targets re-resolve once per TTL (SPEC §4.2), and the address
// change is followed.
func TestCachedResolver_ReresolvesAfterTTL(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	inner := &FakeResolver{}
	a, b := netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2")
	inner.Set("h.test", a)
	r := &CachedResolver{Inner: inner, Clock: c}
	look := func() netip.Addr {
		ips, err := r.LookupIPv4(context.Background(), "h.test")
		if err != nil {
			t.Fatal(err)
		}
		return ips[0]
	}
	if look() != a {
		t.Fatal("first lookup")
	}
	inner.Set("h.test", b)
	c.Advance(59 * time.Second)
	if look() != a || inner.Lookups("h.test") != 1 {
		t.Errorf("within the TTL the cached address is used (lookups %d)", inner.Lookups("h.test"))
	}
	c.Advance(2 * time.Second)
	if look() != b || inner.Lookups("h.test") != 2 {
		t.Errorf("after the TTL the host is resolved again (lookups %d)", inner.Lookups("h.test"))
	}
	inner.Set("h.test") // gone
	c.Advance(61 * time.Second)
	if _, err := r.LookupIPv4(context.Background(), "h.test"); err == nil {
		t.Error("a failed re-resolution must be reported, not hidden by the stale entry")
	}
	if ips, _ := r.LookupIPv4(context.Background(), "192.0.2.9"); len(ips) != 1 {
		t.Error("IP literals pass through")
	}
}

func TestClassifyNetError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		want string
	}{
		{&net.DNSError{Name: "x", Err: "no such host"}, ClassDNS},
		{context.DeadlineExceeded, ClassTimeout},
		{&net.OpError{Op: "dial", Err: wsaeConnRefused}, ClassRefused},
		{&net.OpError{Op: "dial", Err: wsaeNetUnreach}, ClassUnreachable},
		{&net.OpError{Op: "dial", Err: wsaeHostUnreach}, ClassUnreachable},
		{&net.OpError{Op: "dial", Err: wsaeTimedOut}, ClassTimeout},
		{net.UnknownNetworkError("x"), ClassOther},
	}
	for _, tt := range tests {
		if got, _ := ClassifyNetError(tt.err); got != tt.want {
			t.Errorf("%v: %q, want %q", tt.err, got, tt.want)
		}
	}
	if c, _ := ClassifyNetError(nil); c != "" {
		t.Error("nil error has no class")
	}
}
