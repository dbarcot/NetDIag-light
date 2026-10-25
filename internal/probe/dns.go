package probe

import (
	"context"
	"errors"
	"net/netip"
	"time"
)

// DNSAnswer is the outcome of one A query that reached a server.
type DNSAnswer struct {
	Addrs []netip.Addr
	// Rcode is empty for NOERROR, otherwise the failure the server (or the
	// client library) reported: "NXDOMAIN", "SERVFAIL", "REFUSED", ...
	Rcode string
}

// ErrDNSBusy is returned when the previous query to the same server is
// still running (it cannot be cancelled); the probe fails with this detail.
var ErrDNSBusy = errors.New("previous DNS query still pending")

// DNSQuerier sends an A query for name to one specific server. It returns
// an error when no answer arrived (timeout, network failure); a server
// that answered with an error code gives a DNSAnswer with Rcode set.
type DNSQuerier interface {
	Query(ctx context.Context, server netip.Addr, name string, timeout time.Duration) (DNSAnswer, error)
}

// DNSProbe resolves Name at one server: DNS-01 (an adapter DNS server,
// through the system DNS client but straight to that server and past its
// cache) and DNS-02 (a public resolver over UDP 53). Each server is its
// own series, so a failing second resolver is never hidden by a working
// first one (SPEC §5).
type DNSProbe struct {
	Q       DNSQuerier
	Server  netip.Addr
	Name    string
	Timeout time.Duration
}

func (p DNSProbe) Run(ctx context.Context) Sample {
	elapsed := stopwatch()
	ans, err := p.Q.Query(ctx, p.Server, p.Name, p.Timeout)
	rtt := elapsed()
	switch {
	case errors.Is(err, ErrDNSBusy):
		return Sample{Result: Fail, ErrClass: ClassOther, Detail: err.Error(), From: p.Server}
	case err != nil:
		class, detail := ClassifyNetError(err)
		if class == ClassDNS {
			class = ClassOther
		}
		return Sample{Result: Fail, ErrClass: class, Detail: detail, From: p.Server}
	case ans.Rcode != "":
		return Sample{Result: Fail, ErrClass: ClassOther, Detail: ans.Rcode, From: p.Server}
	case len(ans.Addrs) == 0:
		return Sample{Result: Fail, ErrClass: ClassOther, Detail: "no A record", From: p.Server}
	}
	return Sample{Result: OK, RTT: rtt, From: p.Server, Detail: p.Name + " = " + ans.Addrs[0].String()}
}

// FakeDNS answers queries with a function and records the servers asked.
type FakeDNS struct {
	Reply   func(server netip.Addr, name string) (DNSAnswer, error)
	Servers []netip.Addr
}

func (f *FakeDNS) Query(_ context.Context, server netip.Addr, name string, _ time.Duration) (DNSAnswer, error) {
	f.Servers = append(f.Servers, server)
	return f.Reply(server, name)
}
