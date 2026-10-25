//go:build integration && windows

package sysapi

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"
)

var traceTarget = netip.MustParseAddr("1.1.1.1") // default targets.trace / best-route probe target

// TestSysinfo_Print is the Phase 1 acceptance check: it prints the real
// system information this machine reports (run with -v).
func TestSysinfo_Print(t *testing.T) {
	ctx := context.Background()
	sys := NewSystem(nil)

	adapters, err := sys.Adapters.Adapters(ctx)
	if err != nil {
		t.Fatalf("adapters: %v", err)
	}
	if len(adapters) == 0 {
		t.Fatal("no adapters reported")
	}
	for _, a := range adapters {
		t.Logf("adapter #%d %-12q type=%-8s hardware=%-5v up=%-5v mtu=%d mac=%s ipv4=%v gw=%v dns=%v guid=%s",
			a.Index, a.Name, a.Type, a.Hardware, a.Up, a.MTU, a.MAC, a.IPv4, a.Gateways, a.DNSServers, a.GUID)
	}

	rt, err := sys.Routes.BestRoute(ctx, traceTarget)
	p := SelectPrimary(adapters, rt, err)
	t.Logf("primary adapter: %q (#%d) reason=%s link=%v address=%s gateway=%v onLink=%v",
		p.Adapter.Name, p.Adapter.Index, p.Reason, p.LinkUp(), p.Address(), p.Gateway, p.OnLink)
	if errors.Is(err, ErrNoRoute) {
		t.Skip("no route to 1.1.1.1 on this machine")
	}
	if p.Reason != PrimaryByRoute {
		t.Errorf("route exists but primary reason is %s", p.Reason)
	}
	if err != nil {
		t.Fatalf("best route: %v", err)
	}
	best, ok := FindAdapter(adapters, rt.InterfaceIndex)
	if !ok {
		t.Fatalf("best route interface %d not among adapters", rt.InterfaceIndex)
	}
	t.Logf("best route to %s: adapter %q (#%d), gateway %s, source %s", traceTarget, best.Name, rt.InterfaceIndex, rt.Gateway, rt.Source)
	if !best.Up {
		t.Error("best-route adapter is not up")
	}
	if !rt.Source.IsValid() || !rt.Source.Is4() {
		t.Errorf("source address %v", rt.Source)
	}

	if rt.Gateway.IsValid() {
		start := time.Now()
		r, err := sys.ARP.Resolve(ctx, rt.Gateway, rt.InterfaceIndex)
		if err != nil {
			t.Errorf("ARP for gateway %s: %v", rt.Gateway, err)
		} else {
			t.Logf("gateway MAC %s state=%s outcome=%s (in %s)", r.MAC, r.State, r.Outcome(), time.Since(start).Round(time.Millisecond))
			// A live gateway that just answered an ARP request must be confirmed, not stale.
			if r.Outcome() != ARPResolved {
				t.Errorf("gateway answered but outcome is %s", r.Outcome())
			}
		}
	}

	wifi, err := sys.WiFi.Status(ctx)
	if err != nil {
		t.Errorf("wifi: %v", err)
	}
	if len(wifi) == 0 {
		t.Log("Wi-Fi: no WLAN interface")
	}
	for _, w := range wifi {
		t.Logf("Wi-Fi %s %q connected=%v ssid=%q hidden=%v signal=%v rssi=%v channel=%v freq=%v band=%q (%s)",
			w.InterfaceGUID, w.Description, w.Connected, w.SSID, w.SSIDHidden,
			deref(w.SignalPct), deref(w.RSSI), deref(w.Channel), deref(w.FrequencyMHz), w.BandLabel(), w.BandSource)
	}

	m := NewCPUMeter(sys.CPU)
	if _, _, err := m.Sample(); err != nil {
		t.Fatalf("cpu: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	pct, ok, err := m.Sample()
	if err != nil || !ok || pct < 0 || pct > 100 {
		t.Errorf("cpu sample = %v, %v, %v", pct, ok, err)
	}
	t.Logf("CPU %.1f %% (%s)", pct, LevelForCPU(pct, 50, 75, 90))
}

func deref(p *int) any {
	if p == nil {
		return "-"
	}
	return *p
}

func TestWinARP_UnusedAddressIsUnresolved(t *testing.T) {
	ctx := context.Background()
	rt, err := WinRoutes{}.BestRoute(ctx, traceTarget)
	if err != nil || !rt.Gateway.IsValid() {
		t.Skip("no gateway")
	}
	// TEST-NET-1 is never on-link; resolution must fail cleanly, not hang.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := NewARP().Resolve(ctx, netip.MustParseAddr("192.0.2.1"), rt.InterfaceIndex); err == nil {
		t.Error("ARP for 192.0.2.1 succeeded")
	} else {
		t.Logf("ARP 192.0.2.1: %v", err)
	}
}

func TestWinRoutes_LoopbackRoute(t *testing.T) {
	rt, err := WinRoutes{}.BestRoute(context.Background(), netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatalf("loopback route: %v", err)
	}
	if rt.Gateway.IsValid() {
		t.Errorf("loopback must be on-link, got gateway %s", rt.Gateway)
	}
}

func TestWinWiFi_NoPanicAndCleanResult(t *testing.T) {
	list, err := NewWiFi().Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, w := range list {
		if w.InterfaceGUID == "" {
			t.Errorf("interface without GUID: %+v", w)
		}
	}
}

func TestWinChanges_WatchStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := WinChanges{Adapters: WinAdapters{}, Routes: WinRoutes{}}.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("watch channel not closed within 2 s of cancel")
		}
	}
}
