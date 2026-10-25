package probe

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/stun/v3"
)

// startSTUN runs a minimal STUN binding responder on loopback. rewrite, if
// set, changes the address reported to the client (to simulate a NAT that
// maps per destination); answer false drops requests.
func startSTUN(t *testing.T, rewrite func(netip.AddrPort) netip.AddrPort, answer func() bool) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			req := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
			if req.Decode() != nil || req.Type != stun.BindingRequest {
				continue
			}
			if answer != nil && !answer() {
				continue
			}
			ua := from.(*net.UDPAddr)
			ap := netip.AddrPortFrom(netip.MustParseAddr("203.0.113.9"), uint16(ua.Port))
			if rewrite != nil {
				ap = rewrite(ap)
			}
			resp, err := stun.Build(stun.NewTransactionIDSetter(req.TransactionID), stun.BindingSuccess,
				&stun.XORMappedAddress{IP: ap.Addr().AsSlice(), Port: int(ap.Port())}, stun.Fingerprint)
			if err != nil {
				return
			}
			pc.WriteTo(resp.Raw, from) //nolint:errcheck // test responder
		}
	}()
	return pc.LocalAddr().String()
}

func TestSTUNProbe_PublicAddress(t *testing.T) {
	t.Parallel()
	srv := startSTUN(t, nil, nil)
	s := STUNProbe{Resolver: SystemResolver{}, Server: srv, Timeout: time.Second}.Run(context.Background())
	if s.Result != OK || s.RTT <= 0 {
		t.Fatalf("got %+v", s)
	}
	if s.Mapped.Addr() != netip.MustParseAddr("203.0.113.9") || s.Mapped.Port() == 0 {
		t.Errorf("mapped address %v", s.Mapped)
	}
	if s.From != netip.MustParseAddr("127.0.0.1") || s.Detail != "public "+s.Mapped.String() {
		t.Errorf("from %v detail %q", s.From, s.Detail)
	}
}

func TestSTUNProbe_Failures(t *testing.T) {
	t.Parallel()
	silent := startSTUN(t, nil, func() bool { return false })
	s := STUNProbe{Resolver: SystemResolver{}, Server: silent, Timeout: 150 * time.Millisecond}.Run(context.Background())
	if s.Result != Fail || s.ErrClass != ClassTimeout {
		t.Errorf("silent server: %+v", s)
	}
	res := &FakeResolver{}
	// unresolved name: skipped for RT-01 (it defines L4), a failure otherwise
	p := STUNProbe{Resolver: res, Server: "stun.invalid.test:3478", Timeout: time.Second, SkipUnresolved: true}
	if s := p.Run(context.Background()); s.Result != Skipped || s.Detail != "skipped: name not resolved" {
		t.Errorf("RT-01 with unresolved name: %+v, want skipped", s)
	}
	p.SkipUnresolved = false
	if s := p.Run(context.Background()); s.Result != Fail || s.ErrClass != ClassDNS {
		t.Errorf("unresolved name: %+v, want fail/dns", s)
	}
	if s := (STUNProbe{Resolver: res, Server: "no-port", Timeout: time.Second}).Run(context.Background()); s.ErrClass != ClassLocal {
		t.Errorf("bad server string: %+v", s)
	}
}

// A datagram that is not the answer to our request (wrong transaction ID,
// garbage) is ignored; the real answer that follows is used.
func TestSTUNProbe_IgnoresForeignDatagrams(t *testing.T) {
	t.Parallel()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	go func() {
		buf := make([]byte, 1500)
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		req := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
		_ = req.Decode()
		pc.WriteTo([]byte("garbage"), from) //nolint:errcheck // test responder
		other, _ := stun.Build(stun.TransactionID, stun.BindingSuccess,
			&stun.XORMappedAddress{IP: net.ParseIP("198.51.100.1"), Port: 1}, stun.Fingerprint)
		pc.WriteTo(other.Raw, from) //nolint:errcheck // wrong transaction ID
		good, _ := stun.Build(stun.NewTransactionIDSetter(req.TransactionID), stun.BindingSuccess,
			&stun.XORMappedAddress{IP: net.ParseIP("203.0.113.50"), Port: 4242}, stun.Fingerprint)
		pc.WriteTo(good.Raw, from) //nolint:errcheck // test responder
	}()
	s := STUNProbe{Resolver: SystemResolver{}, Server: pc.LocalAddr().String(), Timeout: time.Second}.Run(context.Background())
	if s.Result != OK || s.Mapped != netip.MustParseAddrPort("203.0.113.50:4242") {
		t.Errorf("got %+v", s)
	}
}

func TestNATProbe(t *testing.T) {
	t.Parallel()
	cone1, cone2 := startSTUN(t, nil, nil), startSTUN(t, nil, nil)
	s := NATProbe{Resolver: SystemResolver{}, Servers: []string{cone1, cone2}, Timeout: time.Second}.Run(context.Background())
	if s.Result != OK || s.Value != 0 {
		t.Errorf("same mapping from both servers: %+v, want ok, not symmetric", s)
	}
	// the second server sees another port: the NAT maps per destination
	sym := startSTUN(t, func(a netip.AddrPort) netip.AddrPort { return netip.AddrPortFrom(a.Addr(), a.Port()+1) }, nil)
	s = NATProbe{Resolver: SystemResolver{}, Servers: []string{cone1, sym}, Timeout: time.Second}.Run(context.Background())
	if s.Result != OK || s.Value != 1 {
		t.Errorf("different ports: %+v, want symmetric (Value 1)", s)
	}
	silent := startSTUN(t, nil, func() bool { return false })
	s = NATProbe{Resolver: SystemResolver{}, Servers: []string{cone1, silent}, Timeout: 150 * time.Millisecond}.Run(context.Background())
	if s.Result != Fail {
		t.Errorf("one server silent: %+v, want fail", s)
	}
	s = NATProbe{Resolver: SystemResolver{}, Servers: []string{cone1}, Timeout: time.Second}.Run(context.Background())
	if s.Result != Skipped {
		t.Errorf("one server configured: %+v, want skipped", s)
	}
}
