package probe

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// startDNS runs an in-test UDP DNS server on loopback; the handler decides
// the answer. It returns the server's port.
func startDNS(t *testing.T, h func(w dns.ResponseWriter, r *dns.Msg)) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(h)}
	started := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(started) }
	go srv.ActivateAndServe() //nolint:errcheck // stopped by the cleanup
	<-started
	t.Cleanup(func() { _ = srv.Shutdown() })
	_, port, _ := net.SplitHostPort(pc.LocalAddr().String())
	return port
}

func answerA(ip string) func(dns.ResponseWriter, *dns.Msg) {
	return func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
			A:   net.ParseIP(ip).To4(),
		})
		w.WriteMsg(m) //nolint:errcheck // test server
	}
}

func rcodeReply(rcode int) func(dns.ResponseWriter, *dns.Msg) {
	return func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetRcode(r, rcode)
		w.WriteMsg(m) //nolint:errcheck // test server
	}
}

func TestDNSProbe_Miekg(t *testing.T) {
	t.Parallel()
	lo := netip.MustParseAddr("127.0.0.1")
	tests := []struct {
		name       string
		handler    func(dns.ResponseWriter, *dns.Msg)
		wantResult Result
		wantDetail string
		wantClass  string
	}{
		{"answer", answerA("203.0.113.7"), OK, "www.example.test = 203.0.113.7", ""},
		{"SERVFAIL", rcodeReply(dns.RcodeServerFailure), Fail, "SERVFAIL", ClassOther},
		{"NXDOMAIN", rcodeReply(dns.RcodeNameError), Fail, "NXDOMAIN", ClassOther},
		{"REFUSED", rcodeReply(dns.RcodeRefused), Fail, "REFUSED", ClassOther},
		{"NOERROR without an A record", func(w dns.ResponseWriter, r *dns.Msg) {
			m := new(dns.Msg)
			m.SetReply(r)
			w.WriteMsg(m) //nolint:errcheck // test server
		}, Fail, "no A record", ClassOther},
		{"no answer at all", func(dns.ResponseWriter, *dns.Msg) {}, Fail, "timeout", ClassTimeout},
	}
	for _, tt := range tests {
		port := startDNS(t, tt.handler)
		p := DNSProbe{Q: MiekgDNS{Port: port}, Server: lo, Name: "www.example.test", Timeout: 300 * time.Millisecond}
		s := p.Run(context.Background())
		if s.Result != tt.wantResult || s.ErrClass != tt.wantClass || s.Detail != tt.wantDetail || s.From != lo {
			t.Errorf("%s: %+v, want %s/%q/%q", tt.name, s, tt.wantResult, tt.wantClass, tt.wantDetail)
		}
		if s.Result == OK && s.RTT <= 0 {
			t.Errorf("%s: no RTT", tt.name)
		}
	}
}

// Each server is queried on its own: a dead second server fails its own
// series while the working first one stays ok (SPEC §5 DNS-01).
func TestDNSProbe_EachServerSeparately(t *testing.T) {
	t.Parallel()
	good, bad := netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2")
	f := &FakeDNS{Reply: func(server netip.Addr, _ string) (DNSAnswer, error) {
		if server == bad {
			return DNSAnswer{}, context.DeadlineExceeded
		}
		return DNSAnswer{Addrs: []netip.Addr{netip.MustParseAddr("1.2.3.4")}}, nil
	}}
	pg := DNSProbe{Q: f, Server: good, Name: "www.microsoft.com", Timeout: time.Second}
	pb := DNSProbe{Q: f, Server: bad, Name: "www.microsoft.com", Timeout: time.Second}
	if s := pg.Run(context.Background()); s.Result != OK {
		t.Errorf("good server: %+v", s)
	}
	if s := pb.Run(context.Background()); s.Result != Fail || s.ErrClass != ClassTimeout {
		t.Errorf("bad server: %+v", s)
	}
	if len(f.Servers) != 2 || f.Servers[0] != good || f.Servers[1] != bad {
		t.Errorf("servers asked %v, want each exactly once", f.Servers)
	}
}

func TestDNSProbe_BusyServer(t *testing.T) {
	t.Parallel()
	f := &FakeDNS{Reply: func(netip.Addr, string) (DNSAnswer, error) { return DNSAnswer{}, ErrDNSBusy }}
	s := DNSProbe{Q: f, Server: netip.MustParseAddr("10.0.0.1"), Name: "x.test", Timeout: time.Second}.Run(context.Background())
	if s.Result != Fail || s.Detail != "previous DNS query still pending" {
		t.Errorf("got %+v", s)
	}
}
