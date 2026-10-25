package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/config"
	"netdiag/internal/probe"
	"netdiag/internal/sysapi"
)

func TestBuildProbe_TypesAndTargets(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Targets.Ping = []config.PingTarget{{Name: "nas", Host: "nas.lan", Rate: 1}}
	cfg.Targets.TCP = []config.TCPTarget{{Name: "erp", Host: "erp.lan:443", Rate: 1}}
	p := sysapi.SelectPrimary([]sysapi.Adapter{wifiAdapter("192.168.1.23/24", "192.168.1.1")},
		sysapi.Route{InterfaceIndex: 7, Gateway: netip.MustParseAddr("192.168.1.1")}, nil)
	d := Deps{Clock: clock.NewFake(t0), CPU: sysapi.NewCPUMeter(&sysapi.FakeCPU{})}
	specs := BuildSeries(cfg, p)

	want := map[string]string{
		"SYS-01": "*probe.CPUProbe", "LAN-01": "probe.ICMPProbe", "LAN-02": "probe.ARPProbe",
		"NET-01": "probe.ICMPProbe", "NET-04": "probe.TCPProbe", "RT-01": "probe.STUNProbe",
		"NAT-01": "probe.NATProbe", "DNS-01": "probe.DNSProbe", "DNS-02": "probe.DNSProbe",
		"HTTP-01": "probe.HTTPProbe", "SVC-01": "probe.TLSProbe", "MTU-01": "*probe.MTUProbe",
		"CUST-ICMP": "probe.HostICMPProbe", "CUST-TCP": "probe.TCPProbe",
	}
	got := map[string]bool{}
	for _, sp := range specs {
		pr, ok := BuildProbe(cfg, p, sp, d)
		if wantType, impl := want[sp.Probe]; impl {
			if !ok {
				t.Errorf("%s: no probe", sp.ID)
				continue
			}
			if typ := typeName(pr); typ != wantType {
				t.Errorf("%s: %s, want %s", sp.ID, typ, wantType)
			}
			got[sp.Probe] = true
		} else if ok {
			t.Errorf("%s: probe built for a check that is not implemented before Phase 8", sp.ID)
		}
	}
	for id := range want {
		if !got[id] {
			t.Errorf("no series of probe %s in the catalog", id)
		}
	}

	// target details
	for _, sp := range specs {
		pr, _ := BuildProbe(cfg, p, sp, d)
		switch sp.ID {
		case "LAN-01:gateway":
			if pr.(probe.ICMPProbe).Dest != netip.MustParseAddr("192.168.1.1") || pr.(probe.ICMPProbe).Timeout != time.Second {
				t.Errorf("LAN-01 %+v", pr)
			}
		case "LAN-02:gateway":
			if a := pr.(probe.ARPProbe); a.IfIndex != 7 || a.Gateway != netip.MustParseAddr("192.168.1.1") {
				t.Errorf("LAN-02 %+v", a)
			}
		case "DNS-01:192.168.1.1":
			if q := pr.(probe.DNSProbe); q.Server != netip.MustParseAddr("192.168.1.1") || q.Name != "www.microsoft.com" || q.Timeout != 2*time.Second {
				t.Errorf("DNS-01 %+v", q)
			}
		case "HTTP-01:http://www.msftconnecttest.com/connecttest.txt":
			if h := pr.(probe.HTTPProbe); h.ExpectStatus != 200 || h.ExpectBody != "Microsoft Connect Test" || h.Timeout != 5*time.Second {
				t.Errorf("HTTP-01 %+v", h)
			}
		case "RT-01:stun.cloudflare.com:3478":
			if s := pr.(probe.STUNProbe); !s.SkipUnresolved {
				t.Error("RT-01 must skip, not fail, on an unresolved name (SPEC §6)")
			}
		}
	}
}

func typeName(v any) string { return fmt.Sprintf("%T", v) }

// scripted answers every series with an ok sample and records builds.
type scripted struct {
	mu       sync.Mutex
	gateways []string // LAN-01 targets, one per generation
}

func (s *scripted) build(sp Spec) (probe.Probe, bool) {
	switch sp.Probe {
	case "TEAMS-01", "TEAMS-02", "SRC-01", "MEET-01", "MEDIA-01":
		return nil, false // no probe before Phase 8
	}
	if sp.Probe == "LAN-01" {
		s.mu.Lock()
		s.gateways = append(s.gateways, sp.Target)
		s.mu.Unlock()
	}
	value := 10.0 // CPU %
	if sp.Probe == "MTU-01" {
		value = 1500
	}
	return probe.Func(func(context.Context) probe.Sample {
		return probe.Sample{Result: probe.OK, RTT: 2 * time.Millisecond, Value: value}
	}), true
}

func (s *scripted) built() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.gateways...)
}

type liveFixture struct {
	opts     LiveOptions
	adapters *sysapi.FakeAdapters
	routes   *sysapi.FakeRoutes
	changes  *sysapi.FakeChanges
	script   *scripted
	out      *safeBuffer
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newLiveFixture(t *testing.T) *liveFixture {
	t.Helper()
	cfg := config.Default()
	cfg.Trace.ReverseDNS = false
	ad := &sysapi.FakeAdapters{}
	ad.Set([]sysapi.Adapter{wifiAdapter("192.168.1.23/24", "192.168.1.1")})
	rt := &sysapi.FakeRoutes{}
	rt.Set(sysapi.Route{InterfaceIndex: 7, Gateway: netip.MustParseAddr("192.168.1.1")}, nil)
	ch := &sysapi.FakeChanges{C: make(chan struct{}, 1)}
	sc := &scripted{}
	c := clock.NewScaled(100)
	res := &probe.FakeResolver{}
	icmp := &probe.FakeICMP{Reply: func(req probe.EchoRequest) (probe.EchoReply, error) {
		if req.TTL > 0 && req.TTL < 3 {
			return probe.EchoReply{Status: probe.StatusTTLExpiredTransit, From: netip.MustParseAddr("10.0.0.1"), RTT: time.Millisecond}, nil
		}
		return probe.EchoReply{Status: probe.StatusSuccess, From: req.Dest, RTT: time.Millisecond}, nil
	}}
	out := &safeBuffer{}
	return &liveFixture{
		opts: LiveOptions{
			Cfg: cfg, Clock: c, Out: out,
			Sys:  sysapi.System{Adapters: ad, Routes: rt, Changes: ch},
			Deps: Deps{Clock: c, ICMP: icmp, Resolver: res, Build: sc.build},
		},
		adapters: ad, routes: rt, changes: ch, script: sc, out: out,
	}
}

// waitFor polls a condition for up to 5 s of real time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// RunLive runs the real wiring end to end: gates open, the verdict is OK,
// the tracer finds its hops, and everything stops with the context.
func TestRunLive_OKAndTrace(t *testing.T) {
	t.Parallel()
	f := newLiveFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Snapshot, 1)
	go func() {
		snap, err := RunLive(ctx, f.opts)
		if err != nil {
			t.Error(err)
		}
		done <- snap
	}()
	waitFor(t, "verdict OK", func() bool { return strings.Contains(f.out.String(), "OK [exit 0]") })
	cancel()
	var snap Snapshot
	select {
	case snap = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunLive did not return after cancel")
	}
	out := f.out.String()
	if !strings.HasPrefix(out, "primary adapter: Wi-Fi (route), gateway 192.168.1.1\n") {
		t.Errorf("output:\n%s", out)
	}
	if !strings.Contains(out, "stopped: OK [exit 0]") {
		t.Errorf("no final line:\n%s", out)
	}
	if len(snap.Hops) == 0 || snap.Hops[0].TTL != 1 {
		t.Errorf("hops %+v, want the trace running (TRC-01)", snap.Hops)
	}
	hopSeries := 0
	for _, sp := range snap.Specs {
		if sp.Probe == "TRC-01" {
			hopSeries++
		}
	}
	if hopSeries == 0 {
		t.Error("no hop series were added to the engine")
	}
	// Teams checks have no probe before Phase 8: they are not in the catalog
	for _, sp := range snap.Specs {
		if strings.HasPrefix(sp.Probe, "TEAMS") || sp.Probe == "MEDIA-01" {
			t.Errorf("series %s without a probe", sp.ID)
		}
	}
}

// A new primary adapter resets the gates and rebuilds the series: LAN-01
// now pings the new gateway.
func TestRunLive_AdapterChangeRebuildsSeries(t *testing.T) {
	t.Parallel()
	f := newLiveFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Snapshot, 1)
	go func() {
		snap, err := RunLive(ctx, f.opts)
		if err != nil {
			t.Error(err)
		}
		done <- snap
	}()
	waitFor(t, "first generation", func() bool { return len(f.script.built()) == 1 })

	eth := sysapi.Adapter{Index: 3, GUID: "{E}", Name: "Ethernet", Type: sysapi.AdapterEthernet, Hardware: true, Up: true,
		IPv4: []netip.Prefix{netip.MustParsePrefix("10.0.0.5/24")}, DNSServers: []netip.Addr{netip.MustParseAddr("10.0.0.1")}}
	f.adapters.Set([]sysapi.Adapter{eth})
	f.routes.Set(sysapi.Route{InterfaceIndex: 3, Gateway: netip.MustParseAddr("10.0.0.1")}, nil)
	f.changes.C <- struct{}{}

	waitFor(t, "second generation", func() bool { return len(f.script.built()) == 2 })
	if g := f.script.built(); g[0] != "192.168.1.1" || g[1] != "10.0.0.1" {
		t.Errorf("gateways pinged: %v", g)
	}
	waitFor(t, "the adapter change line", func() bool {
		return strings.Contains(f.out.String(), "adapter change: Wi-Fi 192.168.1.23 gateway 192.168.1.1 -> Ethernet 10.0.0.5 gateway 10.0.0.1")
	})
	waitFor(t, "OK on the new adapter", func() bool {
		out := f.out.String()
		return strings.Contains(out[strings.Index(out, "adapter change"):], "OK [exit 0]")
	})
	cancel()
	snap := <-done
	// nothing of the old adapter is left: no series for its DNS server, no stale rows
	for _, sp := range snap.Specs {
		if sp.ID == "DNS-01:192.168.1.1" {
			t.Errorf("series %s of the old adapter survived", sp.ID)
		}
	}
	if snap.AdapterChanges != 1 || len(snap.AdapterEvents) != 1 {
		t.Errorf("changes %d, events %v", snap.AdapterChanges, snap.AdapterEvents)
	}
}

func TestRunLive_FailsWithoutAdapters(t *testing.T) {
	t.Parallel()
	f := newLiveFixture(t)
	f.adapters.SetErr(context.DeadlineExceeded)
	if _, err := RunLive(context.Background(), f.opts); err == nil {
		t.Error("expected an error when the adapters cannot be listed")
	}
}
