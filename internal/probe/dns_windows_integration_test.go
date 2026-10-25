//go:build integration && windows

package probe

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"netdiag/internal/sysapi"
)

// startDNSAt runs a DNS server on ip:53 (needs no admin on Windows, but the
// port may be taken: the test is skipped then) and counts its queries.
func startDNSAt(t *testing.T, ip string, h func(w dns.ResponseWriter, r *dns.Msg)) *atomic.Int32 {
	t.Helper()
	pc, err := net.ListenPacket("udp4", ip+":53")
	if err != nil {
		t.Skipf("cannot listen on %s:53: %v", ip, err)
	}
	var n atomic.Int32
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		n.Add(1)
		h(w, r)
	})}
	started := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(started) }
	go srv.ActivateAndServe() //nolint:errcheck // stopped by the cleanup
	<-started
	t.Cleanup(func() { _ = srv.Shutdown() })
	return &n
}

// DnsQuery_W with the server in pExtra reaches exactly that server, once
// per query (DNS_QUERY_BYPASS_CACHE: the second identical query is not
// answered from the cache), and a second server is judged on its own.
func TestWinDNS_QueriesTheGivenServerPastTheCache(t *testing.T) {
	good := startDNSAt(t, "127.0.0.1", answerA("203.0.113.7"))
	bad := startDNSAt(t, "127.0.0.2", rcodeReply(dns.RcodeServerFailure))
	w := &WinDNS{}
	ctx := context.Background()
	s1, s2 := netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")

	for range 2 {
		ans, err := w.Query(ctx, s1, "bypass.netdiag.test", 3*time.Second)
		if err != nil || ans.Rcode != "" || len(ans.Addrs) != 1 || ans.Addrs[0] != netip.MustParseAddr("203.0.113.7") {
			t.Fatalf("good server: %+v, %v", ans, err)
		}
	}
	if good.Load() != 2 {
		t.Errorf("good server saw %d queries for 2 identical lookups, want 2 (cache must be bypassed)", good.Load())
	}
	if bad.Load() != 0 {
		t.Errorf("the other server was asked %d times while only the first was queried", bad.Load())
	}
	ans, err := w.Query(ctx, s2, "bypass.netdiag.test", 3*time.Second)
	if err != nil || ans.Rcode != "SERVFAIL" {
		t.Errorf("failing server: %+v, %v, want SERVFAIL", ans, err)
	}
	if good.Load() != 2 {
		t.Errorf("the first server was asked again (%d) when the second was queried", good.Load())
	}
}

func TestWinDNS_TimeoutAndBusy(t *testing.T) {
	w := &WinDNS{}
	dead := netip.MustParseAddr("127.0.0.3")
	startDNSAt(t, "127.0.0.3", func(dns.ResponseWriter, *dns.Msg) {}) // listens, never answers
	_, err := w.Query(context.Background(), dead, "timeout.netdiag.test", 300*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want a timeout", err)
	}
	// the abandoned call is still running: the next query is refused, not stacked
	if _, err := w.Query(context.Background(), dead, "timeout.netdiag.test", 300*time.Millisecond); !errors.Is(err, ErrDNSBusy) {
		t.Errorf("second query: %v, want ErrDNSBusy", err)
	}
}

func TestWinDNS_UnreachableServerFailsFast(t *testing.T) {
	w := &WinDNS{}
	// nothing listens on port 53 of 127.0.0.4: ICMP port unreachable
	_, err := w.Query(context.Background(), netip.MustParseAddr("127.0.0.4"), "refused.netdiag.test", 3*time.Second)
	if err == nil {
		t.Fatal("expected an error")
	}
	if class, _ := ClassifyNetError(err); class != ClassUnreachable && class != ClassTimeout {
		t.Errorf("class %q for %v", class, err)
	}
}

// The system resolver's own servers answer a real name through DNS-01.
func TestWinDNS_SystemResolver(t *testing.T) {
	servers := systemDNSServers(t)
	if len(servers) == 0 {
		t.Skip("no DNS server found")
	}
	w := &WinDNS{}
	ok := false
	for _, s := range servers {
		ans, err := w.Query(context.Background(), s, "www.microsoft.com", 3*time.Second)
		t.Logf("%s: %+v %v", s, ans, err)
		ok = ok || (err == nil && len(ans.Addrs) > 0)
	}
	if !ok {
		t.Error("no system DNS server answered www.microsoft.com")
	}
}

// systemDNSServers returns the IPv4 DNS servers of every adapter that is up.
func systemDNSServers(t *testing.T) []netip.Addr {
	t.Helper()
	list, err := sysapi.WinAdapters{}.Adapters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []netip.Addr
	for _, a := range list {
		if a.Up && a.Type != sysapi.AdapterLoopback {
			out = append(out, a.DNSServers...)
		}
	}
	return out
}
