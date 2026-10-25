package engine

import (
	"net/netip"
	"slices"
	"testing"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/config"
	"netdiag/internal/gate"
	"netdiag/internal/probe"
	"netdiag/internal/sched"
	"netdiag/internal/sysapi"
	"netdiag/internal/verdict"
)

var t0 = time.Date(2026, 10, 25, 14, 0, 0, 0, time.UTC)

func wifiAdapter(ip string, dns ...string) sysapi.Adapter {
	a := sysapi.Adapter{Index: 7, GUID: "{W}", Name: "Wi-Fi", Type: sysapi.AdapterWiFi, Hardware: true, Up: true,
		IPv4: []netip.Prefix{netip.MustParsePrefix(ip)}}
	for _, d := range dns {
		a.DNSServers = append(a.DNSServers, netip.MustParseAddr(d))
	}
	return a
}

func system(a sysapi.Adapter, gw string) System {
	return System{Adapters: []sysapi.Adapter{a}, Route: sysapi.Route{InterfaceIndex: a.Index, Gateway: netip.MustParseAddr(gw)}}
}

func specByID(t *testing.T, specs []Spec, id string) Spec {
	t.Helper()
	for _, s := range specs {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no series %s", id)
	return Spec{}
}

func TestBuildSeries_Catalog(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(`{"targets": {
		"ping": [{"name": "nas", "host": "192.168.1.10"}, {"name": "intranet", "host": "intranet.corp.local"}],
		"tcp": [{"name": "web", "host": "9.9.9.9:443"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	p := sysapi.SelectPrimary([]sysapi.Adapter{wifiAdapter("192.168.1.23/24", "192.168.1.1", "10.9.9.9")},
		sysapi.Route{InterfaceIndex: 7, Gateway: netip.MustParseAddr("192.168.1.1")}, nil)
	specs := BuildSeries(cfg, p)

	tests := []struct {
		id         string
		requires   gate.Layer
		defines    gate.Layer
		l3Evidence bool
		interval   time.Duration
	}{
		{"SYS-01:cpu", gate.None, gate.None, false, time.Second},
		{"LAN-01:gateway", gate.L2, gate.L3, false, time.Second},
		{"LAN-02:gateway", gate.L2, gate.L3, false, 5 * time.Second},
		{"NET-01:1.1.1.1", gate.L3, gate.L4, true, time.Second},
		{"NET-01:8.8.8.8", gate.L3, gate.L4, true, time.Second},
		{"NET-04:1.1.1.1:443", gate.L3, gate.L4, true, time.Second},
		{"RT-01:stun.cloudflare.com:3478", gate.L3, gate.L4, true, 5 * time.Second},
		{"DNS-01:192.168.1.1", gate.L3, gate.L5, false, 5 * time.Second}, // on-link resolver
		{"DNS-01:10.9.9.9", gate.L4, gate.L5, false, 5 * time.Second},    // remote resolver
		{"DNS-02:8.8.8.8", gate.L4, gate.None, false, 5 * time.Second},
		{"HTTP-01:http://www.msftconnecttest.com/connecttest.txt", gate.L3, gate.None, false, 15 * time.Second}, // captive portal: L3 only
		{"SVC-01:meet.google.com", gate.L5, gate.None, false, 30 * time.Second},
		{"MTU-01:gateway", gate.L3, gate.None, false, 600 * time.Second},
		{"MTU-01:1.1.1.1", gate.L4, gate.None, false, 600 * time.Second},
		{"TEAMS-01:worldaz.tr.teams.microsoft.com:3478", gate.L5, gate.None, false, 10 * time.Second},
		{"TEAMS-02:worldaz.tr.teams.microsoft.com:443", gate.L5, gate.None, false, 10 * time.Second},
		{"SRC-01:worldaz.tr.teams.microsoft.com", gate.L5, gate.None, false, 60 * time.Second},
		{"MEET-01:stun.l.google.com:19302", gate.L5, gate.None, false, 10 * time.Second},
		{MediaTeams, gate.L5, gate.None, false, time.Second},
		{MediaMeet, gate.L5, gate.None, false, time.Second},
		{"CUST:nas", gate.L3, gate.None, false, time.Second},      // private IP → lan
		{"CUST:intranet", gate.L5, gate.None, false, time.Second}, // hostname → needs DNS
		{"CUST:web", gate.L4, gate.None, false, time.Second},      // public IP → internet
	}
	for _, tt := range tests {
		s := specByID(t, specs, tt.id)
		if s.Requires != tt.requires || s.Defines != tt.defines || s.L3Evidence != tt.l3Evidence || s.Interval != tt.interval {
			t.Errorf("%s: requires %s defines %s l3ev %v interval %v; want %s %s %v %v",
				tt.id, s.Requires, s.Defines, s.L3Evidence, s.Interval, tt.requires, tt.defines, tt.l3Evidence, tt.interval)
		}
	}
	if s := specByID(t, specs, "LAN-01:gateway"); !s.NeedsGateway || s.Target != "192.168.1.1" {
		t.Errorf("LAN-01 %+v", s)
	}
	if s := specByID(t, specs, "CUST:nas"); s.Probe != "CUST-ICMP" || s.Name != "nas" {
		t.Errorf("custom %+v", s)
	}
	ids := map[string]bool{}
	for _, s := range specs {
		if ids[s.ID] {
			t.Errorf("duplicate series %s", s.ID)
		}
		ids[s.ID] = true
	}
}

func TestBuildSeries_TeamsDisabledAndPorts(t *testing.T) {
	t.Parallel()
	cfg, _ := config.Parse([]byte(`{"teams": {"relays": []}}`))
	for _, s := range BuildSeries(cfg, sysapi.Primary{}) {
		if s.Probe == "TEAMS-01" || s.Probe == "TEAMS-02" || s.Probe == "SRC-01" || s.ID == MediaTeams {
			t.Errorf("Teams series %s with no relays", s.ID)
		}
	}
	cfg, _ = config.Parse([]byte(`{"teams": {"udp_ports": [3478, 3479, 3480, 3481]}}`))
	n := 0
	for _, s := range BuildSeries(cfg, sysapi.Primary{}) {
		if s.Probe == "TEAMS-01" {
			n++
		}
	}
	if n != 4 {
		t.Errorf("%d TEAMS-01 series, want one per port", n)
	}
}

func newEngine(t *testing.T) (*Engine, *clock.Fake) {
	t.Helper()
	c := clock.NewFake(t0)
	e := New(config.Default(), c)
	return e, c
}

func TestEngine_StartsChecking(t *testing.T) {
	t.Parallel()
	e, _ := newEngine(t)
	e.SetSystem(system(wifiAdapter("192.168.1.23/24", "192.168.1.1"), "192.168.1.1"))
	e.SetSeries(BuildSeries(config.Default(), e.Primary()))
	if v := e.Snapshot().Result.Verdict; v != verdict.Checking {
		t.Errorf("verdict %s, want CHECKING before any sample", v)
	}
	if d := e.Decision("NET-01:1.1.1.1"); d.Action != sched.Wait {
		t.Errorf("NET-01 decision %+v, want wait", d)
	}
}

func TestEngine_AdapterChangeResetsUpperGates(t *testing.T) {
	t.Parallel()
	e, c := newEngine(t)
	e.SetSystem(system(wifiAdapter("192.168.1.23/24", "192.168.1.1"), "192.168.1.1"))
	e.SetSeries(BuildSeries(config.Default(), e.Primary()))
	e.OnSample(probe.Sample{Series: "LAN-01:gateway", Time: c.Now(), Result: probe.OK})
	e.Tick()
	if st := e.Snapshot().Gates[gate.L3].State; st != gate.Open {
		t.Fatalf("L3 %s", st)
	}
	e.SetSystem(system(wifiAdapter("192.168.1.23/24", "192.168.1.1"), "192.168.1.1")) // same: no change
	if n := e.Snapshot().AdapterChanges; n != 0 {
		t.Fatalf("adapter changes %d", n)
	}
	eth := sysapi.Adapter{Index: 3, Name: "Ethernet", Type: sysapi.AdapterEthernet, Hardware: true, Up: true,
		IPv4: []netip.Prefix{netip.MustParsePrefix("10.0.0.5/24")}, DNSServers: []netip.Addr{netip.MustParseAddr("10.0.0.1")}}
	e.SetSystem(system(eth, "10.0.0.1"))
	s := e.Snapshot()
	if s.AdapterChanges != 1 || s.Gates[gate.L3].State != gate.Unknown || s.Primary.Adapter.Name != "Ethernet" {
		t.Errorf("after change: changes %d, L3 %s, primary %q", s.AdapterChanges, s.Gates[gate.L3].State, s.Primary.Adapter.Name)
	}
}

func TestEngine_WiFiOfPrimaryAndCPUAverage(t *testing.T) {
	t.Parallel()
	e, c := newEngine(t)
	sig := 30
	sys := system(wifiAdapter("192.168.1.23/24", "192.168.1.1"), "192.168.1.1")
	sys.WiFi = []sysapi.WiFiStatus{{InterfaceGUID: "{OTHER}"}, {InterfaceGUID: "{w}", SignalPct: &sig}}
	e.SetSystem(sys)
	e.SetSeries(BuildSeries(config.Default(), e.Primary()))
	for _, v := range []float64{70, 80, 90} {
		e.OnSample(probe.Sample{Series: "SYS-01:cpu", Time: c.Now(), Result: probe.OK, Value: v})
		c.Advance(time.Second)
	}
	s := e.Snapshot()
	if s.WiFi == nil || *s.WiFi.SignalPct != 30 {
		t.Errorf("Wi-Fi of the primary adapter (GUID match is case-insensitive): %+v", s.WiFi)
	}
	if !slices.Contains(s.Result.Flags, verdict.FlagWiFiWeak) || !slices.Contains(s.Result.Flags, verdict.FlagCPUHigh) {
		t.Errorf("flags %v (CPU avg 80 ≥ 75)", s.Result.Flags)
	}
	c.Advance(10 * time.Second)
	if slices.Contains(e.Snapshot().Result.Flags, verdict.FlagCPUHigh) {
		t.Error("CPU_HIGH must use only the last 10 s")
	}
}

// NET-04 samples taken while L3 is not open are marked Recovery: they
// reopen L3 but are not L4 evidence and not part of the loss stats.
func TestEngine_MarksRecoverySamples(t *testing.T) {
	t.Parallel()
	e, c := newEngine(t)
	e.SetSystem(system(wifiAdapter("192.168.1.23/24", "192.168.1.1"), "192.168.1.1"))
	e.SetSeries(BuildSeries(config.Default(), e.Primary()))
	const net04 = "NET-04:1.1.1.1:443"
	if d := e.Decision(net04); d.Action != sched.Run {
		t.Fatalf("NET-04 decision %+v, want run (L3 recovery)", d)
	}
	e.OnSample(probe.Sample{Series: net04, Time: c.Now(), Result: probe.OK, RTT: 20 * time.Millisecond})
	e.Tick()
	h := e.Store().History(net04)
	if len(h) != 1 || !h[0].Recovery {
		t.Fatalf("history %+v, want one recovery sample", h)
	}
	s := e.Snapshot()
	if s.Gates[gate.L3].State != gate.Open || s.Gates[gate.L4].State != gate.Unknown {
		t.Errorf("L3 %s L4 %s, want open/unknown", s.Gates[gate.L3].State, s.Gates[gate.L4].State)
	}
	if st := e.Store().Stats(net04); st.Sent != 0 {
		t.Errorf("recovery sample counted: %+v", st)
	}
	c.Advance(time.Second)
	e.OnSample(probe.Sample{Series: net04, Time: c.Now(), Result: probe.OK, RTT: 20 * time.Millisecond})
	if h := e.Store().History(net04); h[len(h)-1].Recovery {
		t.Error("sample with L3 open marked recovery")
	}
}
