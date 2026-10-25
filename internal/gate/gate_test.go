package gate

import (
	"testing"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/probe"
	"netdiag/internal/sched"
	"netdiag/internal/sysapi"
)

var t0 = time.Date(2026, 10, 25, 14, 0, 0, 0, time.UTC)

const (
	lan01  = "LAN-01:gateway"
	lan02  = "LAN-02:gateway"
	net01a = "NET-01:1.1.1.1"
	net01b = "NET-01:8.8.8.8"
	net04  = "NET-04:1.1.1.1:443"
	dnsOn  = "DNS-01:192.168.1.1" // on-link resolver
	dnsRem = "DNS-01:10.9.9.9"    // remote resolver
	dns02  = "DNS-02:1.1.1.1"
	http01 = "HTTP-01:msft"
	sys01  = "SYS-01:cpu"
)

var testSeries = []Series{
	{ID: lan01, Interval: time.Second, Requires: L2, Defines: L3, NeedsGateway: true},
	{ID: lan02, Interval: 5 * time.Second, Requires: L2, Defines: L3, NeedsGateway: true},
	{ID: net01a, Interval: time.Second, Requires: L3, Defines: L4, L3Evidence: true},
	{ID: net01b, Interval: time.Second, Requires: L3, Defines: L4, L3Evidence: true},
	{ID: net04, Interval: time.Second, Requires: L3, Defines: L4, L3Evidence: true},
	{ID: dnsOn, Interval: 5 * time.Second, Requires: L3, Defines: L5},
	{ID: dnsRem, Interval: 5 * time.Second, Requires: L4, Defines: L5},
	{ID: dns02, Interval: 5 * time.Second, Requires: L4},
	{ID: http01, Interval: 15 * time.Second, Requires: L5},
	{ID: sys01, Interval: time.Second},
}

var goodLink = Link{Up: true, Address: sysapi.AddressOK, Route: RouteGateway, HasDNS: true}

type fixture struct {
	t     *testing.T
	clock *clock.Fake
	g     *Gates
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{t: t, clock: clock.NewFake(t0)}
	f.g = New(3, 3, f.clock)
	f.g.SetSeries(testSeries)
	return f
}

type feed struct {
	id string
	r  probe.Result
}

// gwDown is one second of a dead gateway: ping and all internet probes fail.
var gwDown = []feed{fail(lan01), fail(net01a), fail(net01b), fail(net04)}

func ok(id string) feed   { return feed{id, probe.OK} }
func fail(id string) feed { return feed{id, probe.Fail} }

// now records samples at the current time and runs the gate tick.
func (f *fixture) now(feeds ...feed) {
	for _, fd := range feeds {
		f.g.Observe(probe.Sample{Series: fd.id, Time: f.clock.Now(), Result: fd.r})
	}
	f.g.Tick()
}

// second advances one second, then records and ticks.
func (f *fixture) second(feeds ...feed) {
	f.clock.Advance(time.Second)
	f.now(feeds...)
}

func (f *fixture) want(l Layer, st State) {
	f.t.Helper()
	if got := f.g.Status(l).State; got != st {
		f.t.Fatalf("%s = %s, want %s (reason %q)", l, got, st, f.g.Status(l).Reason)
	}
}

func (f *fixture) wantDecision(id string, a sched.Action, reason string) {
	f.t.Helper()
	d := f.g.Decision(id)
	if d.Action != a || d.Reason != reason {
		f.t.Fatalf("Decision(%s) = %+v, want action %d reason %q", id, d, a, reason)
	}
}

// openAll brings L1–L5 open, as the first ticks after start-up would.
func (f *fixture) openAll() {
	f.g.SetLink(goodLink)
	f.now(ok(lan01))
	f.now(ok(net01a))
	f.now(ok(dnsOn))
	for _, l := range Layers {
		f.want(l, Open)
	}
}

func TestGates_StartUnknownThenOpenOnFirstSuccess(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, l := range Layers {
		f.want(l, Unknown)
	}
	f.wantDecision(lan01, sched.Wait, "") // L1 not decided: probes wait, no samples
	f.wantDecision(sys01, sched.Run, "")  // SYS-01 has no gate

	f.g.SetLink(goodLink)
	f.want(L1, Open)
	f.want(L2, Open)
	f.want(L3, Unknown)
	f.wantDecision(lan01, sched.Run, "") // L3-defining probes run at L2
	f.wantDecision(net01a, sched.Wait, "")

	f.now(ok(lan01))
	f.want(L3, Open)
	f.wantDecision(net01a, sched.Run, "")
	f.now(ok(net01a))
	f.want(L4, Open)
	f.now(ok(dnsOn))
	f.want(L5, Open)
	f.wantDecision(http01, sched.Run, "")
}

func TestGates_OneTickCascadesUpward(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.g.SetLink(goodLink)
	f.now(ok(lan01), ok(net01a), ok(dnsOn)) // samples of one second, one tick
	for _, l := range Layers {
		f.want(l, Open)
	}
}

func TestGates_HysteresisClosesAfterThreeFailures(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.openAll()
	f.second(gwDown...)
	f.second(gwDown...)
	f.want(L3, Open) // 2 failures: still open
	f.second(gwDown...)
	f.want(L3, Closed)
	if r := f.g.Status(L3).Reason; r != ReasonGatewayDown {
		t.Errorf("reason %q", r)
	}
	f.want(L4, Unknown) // dependents are re-decided after recovery
	f.want(L5, Unknown)
	f.wantDecision(net01a, sched.Skip, ReasonGatewayDown)
	f.wantDecision(dns02, sched.Skip, ReasonGatewayDown) // lowest closed layer's reason
	f.wantDecision(lan01, sched.Run, "")                 // defining probe keeps running
}

func TestGates_OneRoundPerPeriod(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.openAll()
	// three failing samples within one second are one round, not three
	f.clock.Advance(time.Second)
	for range 3 {
		f.now(fail(lan01))
	}
	f.want(L3, Open)
}

func TestGates_UnknownClosesOnlyAfterFailCount(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.g.SetLink(goodLink)
	f.now(fail(lan01))
	f.second(fail(lan01))
	f.want(L3, Unknown)
	f.second(fail(lan01))
	f.want(L3, Closed)
}

func TestGates_FlappingNeverCloses(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.openAll()
	for range 10 {
		f.second(fail(lan01))
		f.second(fail(lan01))
		f.second(ok(lan01))
		f.want(L3, Open)
	}
}

func TestGates_RecoveryNeedsOKCountAndReopensDependents(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.openAll()
	for range 3 {
		f.second(gwDown...)
	}
	f.want(L3, Closed)
	f.second(ok(lan01))
	f.second(ok(lan01))
	f.want(L3, Closed) // 2 successes
	f.second(ok(lan01))
	f.want(L3, Open)
	f.want(L4, Unknown)
	f.wantDecision(net01a, sched.Run, "")
	f.now(ok(net01a))
	f.want(L4, Open)
	f.now(ok(dnsOn))
	f.want(L5, Open)
}

func TestGates_ICMPBlockedNetworkOpensL4ViaTCP(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.g.SetLink(goodLink)
	f.now(ok(lan01))
	for range 6 {
		f.second(ok(lan01), fail(net01a), fail(net01b), ok(net04))
		f.want(L4, Open)
	}
}

func TestGates_AllInternetProbesFailingClosesL4(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.openAll()
	for range 3 {
		f.second(ok(lan01), fail(net01a), fail(net01b), fail(net04))
	}
	f.want(L3, Open)
	f.want(L4, Closed)
	f.wantDecision(dns02, sched.Skip, ReasonInternetDown)
}

func TestGates_OnLinkResolverDoesNotDependOnL4(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.openAll()
	for range 3 {
		f.second(ok(lan01), fail(net01a), fail(net01b), fail(net04))
	}
	f.want(L4, Closed)
	f.wantDecision(dnsOn, sched.Run, "")                   // on-link: needs only L3
	f.wantDecision(dnsRem, sched.Skip, ReasonInternetDown) // remote: needs L4
}

func TestGates_L5RoundsEveryDNSInterval(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.openAll()
	for i := 1; i <= 15; i++ {
		feeds := []feed{ok(lan01), ok(net01a)}
		if i%5 == 0 {
			feeds = append(feeds, fail(dnsOn))
		}
		f.second(feeds...)
		if i < 15 {
			f.want(L5, Open) // a failing DNS sample is one round per 5 s, not one per second
		}
	}
	f.want(L5, Closed)
}

func TestGates_StaleARPAloneDoesNotKeepL3Open(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.openAll()
	f.now(fail(lan02)) // stale/unconfirmed ARP is recorded as a failure
	for range 3 {
		f.second(fail(lan01), fail(net01a), fail(net01b), fail(net04))
	}
	f.want(L3, Closed)
}

func TestGates_InternetTrafficKeepsL3OpenWhenGatewayIgnoresPing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.openAll()
	f.now(fail(lan02)) // stale ARP
	for range 6 {
		f.second(fail(lan01), ok(net01a))
		f.want(L3, Open)
	}
}

func TestGates_ReachableARPKeepsL3Open(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.openAll()
	f.now(ok(lan02))
	for range 4 {
		f.second(fail(lan01))
		f.want(L3, Open)
	}
}

func TestGates_OldEvidenceExpires(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.openAll()
	f.now(ok(lan02)) // LAN-02 runs every 5 s: its result counts for 7.5 s
	f.clock.Advance(7 * time.Second)
	f.now(gwDown...)
	f.second(gwDown...) // 8 s: the ARP result has expired, failures count from here
	f.second(gwDown...)
	f.want(L3, Open) // the 7 s round still had fresh ARP evidence
	f.second(gwDown...)
	f.want(L3, Closed)
}

func TestGates_SkippedSampleWithdrawsEvidence(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.openAll()
	f.now(ok(lan02), feed{lan02, probe.Skipped})
	for range 3 {
		f.second(gwDown...)
	}
	f.want(L3, Closed)
}

func TestGates_OnLinkRoute(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.g.SetLink(Link{Up: true, Address: sysapi.AddressOK, Route: RouteOnLink, HasDNS: true})
	f.want(L3, Open)
	f.wantDecision(lan01, sched.Skip, SkipOnLink)
	f.wantDecision(lan02, sched.Skip, SkipOnLink)
	f.wantDecision(net01a, sched.Run, "")
	for range 3 { // LAN-01 rounds do not apply on-link
		f.second(fail(lan01))
	}
	f.want(L3, Open)
	f.g.SetLink(goodLink) // back to a gateway route: evidence decides again
	f.want(L3, Unknown)
}

func TestGates_NoDefaultRoute(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.g.SetLink(Link{Up: true, Address: sysapi.AddressOK, Route: RouteNone, HasDNS: true})
	f.want(L3, Closed)
	f.wantDecision(lan01, sched.Skip, ReasonNoRoute)
	f.wantDecision(net01a, sched.Skip, ReasonNoRoute)
}

func TestGates_LinkAndAddress(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.openAll()
	f.g.SetLink(Link{Up: false})
	f.want(L1, Closed)
	for _, l := range []Layer{L2, L3, L4, L5} {
		f.want(l, Unknown)
	}
	f.wantDecision(lan01, sched.Skip, ReasonNoLink)
	f.wantDecision(http01, sched.Skip, ReasonNoLink)

	f.g.SetLink(Link{Up: true, Address: sysapi.AddressAPIPA, Route: RouteNone})
	f.want(L1, Open)
	f.want(L2, Closed)
	f.wantDecision(lan01, sched.Skip, ReasonAPIPA)

	f.g.SetLink(Link{Up: true, Address: sysapi.AddressNone, Route: RouteNone})
	if r := f.g.Status(L2).Reason; r != ReasonNoAddress {
		t.Errorf("L2 reason %q", r)
	}
}

func TestGates_NoDNSServersClosesL5(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.g.SetLink(Link{Up: true, Address: sysapi.AddressOK, Route: RouteGateway, HasDNS: false})
	f.now(ok(lan01), ok(net01a))
	f.want(L4, Open)
	f.want(L5, Closed)
	if r := f.g.Status(L5).Reason; r != ReasonNoDNSServers {
		t.Errorf("reason %q", r)
	}
	f.wantDecision(http01, sched.Skip, ReasonDNSDown)
}

func TestGates_ResetOnAdapterChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.openAll()
	f.g.Reset()
	f.want(L1, Open)
	f.want(L2, Open)
	f.want(L3, Unknown)
	f.want(L4, Unknown)
	f.want(L5, Unknown)
	f.wantDecision(net01a, sched.Wait, "")
	f.now(ok(lan01))
	f.want(L3, Open)

	resets := 0
	for _, tr := range f.g.History() {
		if tr.Layer == L4 && tr.From == Open && tr.To == Unknown {
			resets++
		}
	}
	if resets != 1 {
		t.Errorf("history %+v", f.g.History())
	}
}

func TestGates_UnknownSeriesRuns(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.wantDecision("nope", sched.Run, "")
	f.now(fail("nope")) // ignored
}

func TestLayerAndStateStrings(t *testing.T) {
	t.Parallel()
	if L3.String() != "L3" || None.String() != "none" || Closed.String() != "closed" || Unknown.String() != "unknown" {
		t.Error("strings")
	}
}
