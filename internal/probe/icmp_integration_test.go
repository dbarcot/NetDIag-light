//go:build integration && windows

package probe

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"netdiag/internal/sysapi"
)

func gateway(t *testing.T) netip.Addr {
	t.Helper()
	rt, err := sysapi.WinRoutes{}.BestRoute(context.Background(), sysapi.RouteProbeTarget)
	if err != nil || !rt.Gateway.IsValid() {
		t.Skip("no default gateway")
	}
	return rt.Gateway
}

func TestWinICMP_Loopback(t *testing.T) {
	s := ICMPProbe{ICMP: WinICMP{}, Dest: Loopback, Timeout: time.Second}.Run(context.Background())
	if s.Result != OK || s.RTT <= 0 {
		t.Fatalf("loopback ping: %+v", s)
	}
	t.Logf("127.0.0.1 RTT %.3f ms", s.RTTms())
}

func TestWinICMP_Gateway(t *testing.T) {
	gw := gateway(t)
	s := ICMPProbe{ICMP: WinICMP{}, Dest: gw, Timeout: time.Second}.Run(context.Background())
	if s.Result != OK || s.RTT <= 0 {
		t.Fatalf("gateway ping: %+v", s)
	}
	t.Logf("gateway %s RTT %.3f ms", gw, s.RTTms())
}

func TestWinICMP_UnreachableTestNetFailsWithinTimeout(t *testing.T) {
	start := time.Now()
	s := ICMPProbe{ICMP: WinICMP{}, Dest: netip.MustParseAddr("192.0.2.1"), Timeout: time.Second}.Run(context.Background())
	took := time.Since(start)
	if s.Result != Fail || s.ErrClass == ClassLocal {
		t.Fatalf("192.0.2.1: %+v", s)
	}
	if took > 1500*time.Millisecond {
		t.Errorf("took %s, want about the 1 s timeout", took)
	}
	t.Logf("192.0.2.1: %s (%s) in %s", s.ErrClass, s.Detail, took.Round(time.Millisecond))
}

// A context with under 1 ms left must not reach IcmpSendEcho2 with a 0 timeout.
func TestWinICMP_NearlyExpiredContextIsTimeout(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(500*time.Microsecond))
	defer cancel()
	r, err := WinICMP{}.Echo(ctx, EchoRequest{Dest: Loopback, Timeout: time.Second})
	if err != nil && ctx.Err() == nil {
		t.Fatalf("unexpected error %v", err)
	}
	if err == nil && r.Status != StatusTimedOut {
		t.Errorf("status %s, want timed out", r.Status)
	}
}

func TestWinICMP_TimerOverhead(t *testing.T) {
	d, err := TimerOverhead(context.Background(), WinICMP{}, 10)
	if err != nil || d <= 0 || d > 5*time.Millisecond {
		t.Fatalf("timer overhead %v, %v", d, err)
	}
	t.Logf("timer overhead %s", d)
}

// TTL 1 toward the internet must expire at the gateway: needed by TRC-01.
func TestWinICMP_TTLExpiresAtGateway(t *testing.T) {
	gw := gateway(t)
	r, err := WinICMP{}.Echo(context.Background(), EchoRequest{Dest: sysapi.RouteProbeTarget, TTL: 1, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusTTLExpiredTransit || r.From != gw {
		t.Errorf("TTL 1: status %s from %s, want TTL expired from %s", r.Status, r.From, gw)
	}
}

// A full-size DF probe returns success or "packet too big", never a local
// buffer error (SPEC §5 reply buffer note; needed by MTU-01).
func TestWinICMP_DFFullSizeIsNetworkResult(t *testing.T) {
	gateway(t)
	r, err := WinICMP{}.Echo(context.Background(), EchoRequest{Dest: sysapi.RouteProbeTarget, DF: true, Size: 1472, Timeout: time.Second})
	if err != nil {
		t.Fatalf("local error: %v", err)
	}
	switch r.Status {
	case StatusSuccess, StatusPacketTooBig, StatusTimedOut:
		t.Logf("DF 1472 to %s: %s from %s", sysapi.RouteProbeTarget, r.Status, r.From)
	default:
		t.Errorf("unexpected status %s", r.Status)
	}
}
