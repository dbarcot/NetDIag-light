package engine

import (
	"net/netip"
	"slices"
	"strings"
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
	if !s.HasCPU || s.CPUAvg10 != 80 {
		t.Errorf("snapshot CPU %v %v, want the 10 s average 80 for the console header", s.HasCPU, s.CPUAvg10)
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

// feedSecond records one second of samples for the series of a catalog:
// ok unless lossy(id) says this one fails.
func feedSecond(e *Engine, c *clock.Fake, lossy func(id string) bool) {
	for _, sp := range e.Specs() {
		switch sp.Probe {
		case "LAN-01", "NET-01", "NET-04", "RT-01", "DNS-01", "SYS-01":
		default:
			continue
		}
		s := probe.Sample{Series: sp.ID, Probe: sp.Probe, Target: sp.Target, Time: c.Now(), Result: probe.OK, RTT: 10 * time.Millisecond, Value: 20}
		if lossy != nil && lossy(sp.ID) {
			s = probe.Sample{Series: sp.ID, Probe: sp.Probe, Target: sp.Target, Time: c.Now(), Result: probe.Fail, ErrClass: probe.ClassTimeout}
		}
		e.OnSample(s)
	}
	e.Tick()
	c.Advance(time.Second)
}

// Wi-Fi drops, the laptop moves to Ethernet: the failures measured on the
// old path must not read as LAN_LOSS/ISP_LOSS on the new one (SPEC §6).
func TestEngine_AdapterChangeStartsNewSeries(t *testing.T) {
	t.Parallel()
	e, c := newEngine(t)
	cfg := config.Default()
	e.SetSystem(system(wifiAdapter("192.168.1.23/24", "192.168.1.1"), "192.168.1.1"))
	e.SetSeries(BuildSeries(cfg, e.Primary()))
	oldGen := e.Generation()

	// 20 s of a bad Wi-Fi link: every other ping to the gateway and to the internet is lost
	n := 0
	for n = 0; n < 20; n++ {
		feedSecond(e, c, func(id string) bool {
			if strings.HasPrefix(id, "LAN-01") || strings.HasPrefix(id, "NET-01") {
				return n%2 == 0
			}
			return false
		})
	}
	if v := e.Snapshot().Result.Verdict; v != verdict.LANLoss {
		t.Fatalf("before the change: %s, want LAN_LOSS (test setup)", v)
	}
	if e.Store().Window("DNS-01:192.168.1.1") == nil {
		t.Fatal("setup: no DNS-01 series")
	}

	// moves to Ethernet with another gateway and DNS server
	eth := sysapi.Adapter{Index: 3, GUID: "{E}", Name: "Ethernet", Type: sysapi.AdapterEthernet, Hardware: true, Up: true,
		IPv4: []netip.Prefix{netip.MustParsePrefix("10.0.0.5/24")}, DNSServers: []netip.Addr{netip.MustParseAddr("10.0.0.1")}}
	e.SetSystem(system(eth, "10.0.0.1"))
	e.SetSeries(BuildSeries(cfg, e.Primary()))

	if got := len(e.Store().Window("LAN-01:gateway")); got != 0 {
		t.Errorf("LAN-01 window has %d samples of the old path", got)
	}
	if got := len(e.Store().History("NET-01:1.1.1.1")); got != 0 {
		t.Errorf("NET-01 history has %d samples of the old path", got)
	}
	if got := len(e.Store().Window("SYS-01:cpu")); got == 0 {
		t.Error("the CPU series belongs to the machine and must survive an adapter change")
	}
	ids := e.Store().IDs()
	if slices.Contains(ids, "DNS-01:192.168.1.1") {
		t.Errorf("the stale series DNS-01:192.168.1.1 is still in the store: %v", ids)
	}
	if !slices.Contains(ids, "DNS-01:10.0.0.1") {
		t.Errorf("no series for the new DNS server: %v", ids)
	}

	// a probe of the old adapter still in flight must not write into the new series
	e.SinkFor(oldGen)(probe.Sample{Series: "LAN-01:gateway", Time: c.Now(), Result: probe.Fail, ErrClass: probe.ClassTimeout})
	if got := len(e.Store().History("LAN-01:gateway")); got != 0 {
		t.Errorf("a stale-generation sample was recorded (%d)", got)
	}

	// clean samples on the new path: no loss verdict, however short the way back to OK
	for range 15 {
		feedSecond(e, c, nil)
		if v := e.Snapshot().Result.Verdict; v == verdict.LANLoss || v == verdict.ISPLoss {
			t.Fatalf("%s after the adapter change", v)
		}
	}
	snap := e.Snapshot()
	if snap.Result.Verdict != verdict.OK {
		t.Errorf("verdict %s, want OK on the new adapter", snap.Result.Verdict)
	}
	if len(snap.AdapterEvents) != 1 {
		t.Fatalf("events %+v, want one adapter change", snap.AdapterEvents)
	}
	ev := snap.AdapterEvents[0]
	if ev.From != (AdapterInfo{Name: "Wi-Fi", IPv4: "192.168.1.23", Gateway: "192.168.1.1"}) ||
		ev.To != (AdapterInfo{Name: "Ethernet", IPv4: "10.0.0.5", Gateway: "10.0.0.1"}) {
		t.Errorf("event %+v", ev)
	}
	if got := ev.String(); got != "adapter change: Wi-Fi 192.168.1.23 gateway 192.168.1.1 -> Ethernet 10.0.0.5 gateway 10.0.0.1" {
		t.Errorf("text %q", got)
	}
}

// A new generation of series adds hops only for itself.
func TestEngine_StaleGenerationCannotAddSeries(t *testing.T) {
	t.Parallel()
	e, _ := newEngine(t)
	e.SetSystem(system(wifiAdapter("192.168.1.23/24", "192.168.1.1"), "192.168.1.1"))
	e.SetSeries(BuildSeries(config.Default(), e.Primary()))
	old := e.Generation()
	eth := sysapi.Adapter{Index: 3, Name: "Ethernet", Type: sysapi.AdapterEthernet, Hardware: true, Up: true,
		IPv4: []netip.Prefix{netip.MustParsePrefix("10.0.0.5/24")}}
	e.SetSystem(system(eth, "10.0.0.1"))
	e.SetSeries(BuildSeries(config.Default(), e.Primary()))
	var s Spec
	s.ID, s.Probe = "TRC-01:03", "TRC-01"
	e.AddSeriesFor(old, s)
	if slices.Contains(e.Store().IDs(), "TRC-01:03") {
		t.Error("a hop of the old trace was added to the new generation")
	}
	e.AddSeriesFor(e.Generation(), s)
	if !slices.Contains(e.Store().IDs(), "TRC-01:03") {
		t.Error("a hop of the current trace was not added")
	}
	// the next generation drops it again
	e.SetSeries(BuildSeries(config.Default(), e.Primary()))
	if slices.Contains(e.Store().IDs(), "TRC-01:03") {
		t.Error("old trace hops must be removed by the next SetSeries")
	}
}

// Reset (the r key) clears stats, Sent counters and graph history, but
// keeps the gates and the adapter history (SPEC §9).
func TestEngine_ResetClearsCountersKeepsGates(t *testing.T) {
	t.Parallel()
	e, c := newEngine(t)
	e.SetSystem(system(wifiAdapter("192.168.1.23/24", "192.168.1.1"), "192.168.1.1"))
	e.SetSeries(BuildSeries(config.Default(), e.Primary()))
	for range 5 {
		feedSecond(e, c, nil)
	}
	before := e.Snapshot()
	if before.Gates[gate.L4].State != gate.Open || e.Store().Stats("NET-01:1.1.1.1").Sent == 0 {
		t.Fatalf("setup: L4 %s", before.Gates[gate.L4].State)
	}
	e.Reset()
	after := e.Snapshot()
	for _, id := range e.Store().IDs() {
		if n := len(e.Store().History(id)); n != 0 {
			t.Errorf("%s keeps %d samples after reset", id, n)
		}
	}
	if after.Gates != before.Gates || len(e.GateHistory()) == 0 {
		t.Errorf("gates changed by reset: %v -> %v", before.Gates, after.Gates)
	}
}

// Adapter events are numbered from 1 for consumers that must not print an
// event twice when old ones are dropped (Phase 9 cap).
func TestEngine_AdapterEventSeq(t *testing.T) {
	t.Parallel()
	e, _ := newEngine(t)
	wifi := system(wifiAdapter("192.168.1.23/24", "192.168.1.1"), "192.168.1.1")
	eth := system(sysapi.Adapter{Index: 3, Name: "Ethernet", Type: sysapi.AdapterEthernet, Hardware: true, Up: true,
		IPv4: []netip.Prefix{netip.MustParsePrefix("10.0.0.5/24")}}, "10.0.0.1")
	for _, s := range []System{wifi, eth, wifi, eth} {
		e.SetSystem(s)
	}
	snap := e.Snapshot()
	if len(snap.AdapterEvents) != 3 || snap.AdapterEventsDropped != 0 {
		t.Fatalf("events %d, dropped %d", len(snap.AdapterEvents), snap.AdapterEventsDropped)
	}
	for i, ev := range snap.AdapterEvents {
		if ev.Seq != i+1 {
			t.Errorf("event %d has Seq %d, want %d", i, ev.Seq, i+1)
		}
	}
}
