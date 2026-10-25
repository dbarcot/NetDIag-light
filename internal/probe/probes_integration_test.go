//go:build integration && windows

package probe

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

// Each real probe once against its default public target (docs/PHASES.md
// Phase 4 integration list). They need a normal home/office connection.

func realRun(t *testing.T, p Probe) Sample {
	t.Helper()
	s := p.Run(context.Background())
	t.Logf("%+v", s)
	return s
}

func TestReal_TCPInternet(t *testing.T) {
	s := realRun(t, TCPProbe{Dialer: &net.Dialer{}, Resolver: SystemResolver{}, Addr: "1.1.1.1:443", Timeout: 2 * time.Second})
	if s.Result != OK || s.RTT <= 0 {
		t.Errorf("NET-04: %+v", s)
	}
}

func TestReal_PublicDNS(t *testing.T) {
	for _, server := range []string{"1.1.1.1", "8.8.8.8"} {
		s := realRun(t, DNSProbe{Q: MiekgDNS{}, Server: netip.MustParseAddr(server), Name: "www.microsoft.com", Timeout: 2 * time.Second})
		if s.Result != OK {
			t.Errorf("DNS-02 %s: %+v", server, s)
		}
	}
}

func TestReal_STUNAndNAT(t *testing.T) {
	for _, server := range []string{"stun.cloudflare.com:3478", "stun.l.google.com:19302"} {
		s := realRun(t, STUNProbe{Resolver: SystemResolver{}, Server: server, Timeout: 2 * time.Second})
		if s.Result != OK || !s.Mapped.IsValid() {
			t.Errorf("RT-01 %s: %+v", server, s)
		}
	}
	s := realRun(t, NATProbe{Resolver: SystemResolver{}, Servers: []string{"stun.cloudflare.com:3478", "stun.l.google.com:19302"}, Timeout: 2 * time.Second})
	if s.Result != OK {
		t.Errorf("NAT-01: %+v", s)
	}
}

func TestReal_HTTPChecks(t *testing.T) {
	checks := []HTTPProbe{
		{URL: "http://www.msftconnecttest.com/connecttest.txt", ExpectStatus: 200, ExpectBody: "Microsoft Connect Test"},
		{URL: "http://connectivitycheck.gstatic.com/generate_204", ExpectStatus: 204},
	}
	for _, c := range checks {
		c.Timeout = 5 * time.Second
		if s := realRun(t, c); s.Result != OK {
			t.Errorf("HTTP-01 %s: %+v", c.URL, s)
		}
	}
}

func TestReal_ServicesTLS(t *testing.T) {
	for _, host := range []string{"teams.microsoft.com", "outlook.office365.com", "meet.google.com", "zoom.us"} {
		s := realRun(t, TLSProbe{Resolver: SystemResolver{}, Host: host, Timeout: 5 * time.Second})
		if s.Result != OK || s.RTT <= 0 {
			t.Errorf("SVC-01 %s: %+v", host, s)
		}
	}
}

func TestReal_MTU(t *testing.T) {
	gw := gateway(t)
	s := realRun(t, MTUProbe{ICMP: WinICMP{}, Resolver: SystemResolver{}, Host: gw.String(), Timeout: time.Second})
	if s.Result != OK || s.Value < MinMTU || s.Value > MaxMTU {
		t.Errorf("MTU-01 to the gateway: %+v", s)
	}
	s = realRun(t, MTUProbe{ICMP: WinICMP{}, Resolver: SystemResolver{}, Host: "1.1.1.1", Timeout: time.Second})
	if s.Result != OK || s.Value < MinMTU || s.Value > MaxMTU {
		t.Errorf("MTU-01 to 1.1.1.1: %+v", s)
	}
}

// The trace to 1.1.1.1 ends at the target: some hop is final and answers
// from 1.1.1.1.
func TestReal_TraceReachesTheTarget(t *testing.T) {
	var col collector
	tr := &Tracer{
		ICMP: WinICMP{}, Resolver: SystemResolver{}, Host: "1.1.1.1", MaxHops: 30,
		Interval: MinTraceInterval, Timeout: time.Second, Clock: realClock{},
		Emit: col.emit, Register: col.reg,
	}
	for range 40 { // 5 packets/s at most, like Run
		tr.Step(context.Background())
		if len(tr.Hops()) > 0 && tr.Hops()[len(tr.Hops())-1].Final {
			break
		}
		time.Sleep(MinTraceInterval)
	}
	hops := tr.Hops()
	if len(hops) == 0 {
		t.Fatal("no hops")
	}
	last := hops[len(hops)-1]
	for _, h := range hops {
		t.Logf("hop %d %v final=%v", h.TTL, h.IPs, h.Final)
	}
	if !last.Final || len(last.IPs) != 1 || last.IPs[0] != netip.MustParseAddr("1.1.1.1") {
		t.Errorf("last hop %+v, want the destination 1.1.1.1", last)
	}
}
