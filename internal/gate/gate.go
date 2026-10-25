package gate

import (
	"fmt"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/probe"
	"netdiag/internal/sched"
	"netdiag/internal/sysapi"
)

// Layer is a dependency layer; None means "no layer".
type Layer int

const (
	None Layer = iota
	L1         // link: primary adapter up
	L2         // address: non-APIPA IPv4
	L3         // gateway
	L4         // internet
	L5         // DNS
)

// Layers lists L1–L5 in order.
var Layers = []Layer{L1, L2, L3, L4, L5}

func (l Layer) String() string {
	if l < L1 || l > L5 {
		return "none"
	}
	return fmt.Sprintf("L%d", int(l))
}

// State is a layer's state.
type State int

const (
	Unknown State = iota // not decided yet (start-up, after a reset)
	Open
	Closed
)

func (s State) String() string {
	switch s {
	case Open:
		return "open"
	case Closed:
		return "closed"
	default:
		return "unknown"
	}
}

// Reasons a layer is closed; they are also the skip reasons of probes
// above it ("skipped: gateway down").
const (
	ReasonNoLink       = "no link"
	ReasonAPIPA        = "APIPA address only"
	ReasonNoAddress    = "no IPv4 address"
	ReasonNoRoute      = "no default route"
	ReasonGatewayDown  = "gateway down"
	ReasonInternetDown = "internet down"
	ReasonDNSDown      = "DNS down"
	ReasonNoDNSServers = "no DNS servers"
	ReasonOnLink       = "on-link route"
	// SkipOnLink is the skip reason of LAN-01/LAN-02 with an on-link route.
	SkipOnLink = "no gateway (on-link route)"
)

// Status is a layer's state with its reason and the time it was entered.
type Status struct {
	State  State
	Reason string
	Since  time.Time
}

// Transition is one change of a layer's state (gate history).
type Transition struct {
	Time     time.Time
	Layer    Layer
	From, To State
	Reason   string
}

// Series tells the gates what a series needs and proves.
type Series struct {
	ID       string
	Interval time.Duration
	// Requires is the highest layer the series needs open (None: always runs).
	Requires Layer
	// Defines is the layer whose state this series decides (None if any).
	Defines Layer
	// L3Evidence marks L4-defining series (NET-01/NET-04/RT-01): their
	// success also proves the gateway alive (SPEC §6 L3 evidence).
	L3Evidence bool
	// NeedsGateway marks LAN-01/LAN-02: skipped without a gateway.
	NeedsGateway bool
	// L3Recovery marks NET-04: it keeps running while L3 is not open with
	// a gateway route, so a gateway that ignores ping and whose ARP entry
	// is only stale can still reopen L3 through the internet (SPEC §6).
	L3Recovery bool
}

// RouteMode says how L3 is decided.
type RouteMode int

const (
	RouteGateway RouteMode = iota // default route via a gateway: evidence decides
	RouteOnLink                   // on-link default route: L3 follows L2
	RouteNone                     // no default route: L3 closed
)

// Link is the L1/L2 input from the primary adapter (SPEC §5 LINK-01).
type Link struct {
	Up      bool
	Address sysapi.AddressState
	Route   RouteMode
	HasDNS  bool // the adapter has at least one DNS server
}

// Gates holds the layer states. It is not safe for concurrent use; the
// engine serializes access.
type Gates struct {
	failCount, okCount int
	clock              clock.Clock

	status    [L5 + 1]Status
	okStreak  [L5 + 1]int
	failStrk  [L5 + 1]int
	series    map[string]Series
	period    [L5 + 1]time.Duration // round period: shortest defining interval
	lastRound [L5 + 1]time.Time
	evidence  [L5 + 1]map[string]probe.Sample
	link      Link
	linkSet   bool
	history   []Transition
}

// New returns gates with the hysteresis counts (gate.fail_count,
// gate.ok_count), all layers unknown.
func New(failCount, okCount int, c clock.Clock) *Gates {
	g := &Gates{failCount: failCount, okCount: okCount, clock: c, series: map[string]Series{}}
	now := c.Now()
	for _, l := range Layers {
		g.status[l] = Status{State: Unknown, Since: now}
		g.evidence[l] = map[string]probe.Sample{}
	}
	return g
}

// SetSeries replaces the series list. A layer runs one hysteresis round
// per period, the shortest interval of its defining series (SPEC §6).
func (g *Gates) SetSeries(list []Series) {
	g.series = make(map[string]Series, len(list))
	g.period = [L5 + 1]time.Duration{}
	for _, s := range list {
		g.series[s.ID] = s
		if s.Defines != None && (g.period[s.Defines] == 0 || s.Interval < g.period[s.Defines]) {
			g.period[s.Defines] = s.Interval
		}
	}
	for _, l := range Layers {
		for id := range g.evidence[l] {
			if _, ok := g.series[id]; !ok {
				delete(g.evidence[l], id)
			}
		}
	}
}

// Status returns a layer's status.
func (g *Gates) Status(l Layer) Status { return g.status[l] }

// History returns every transition so far.
func (g *Gates) History() []Transition { return append([]Transition(nil), g.history...) }

// SetLink applies the primary adapter's state: L1 and L2 follow it
// directly (adapter state is authoritative, no hysteresis), L3 follows the
// route mode.
func (g *Gates) SetLink(l Link) {
	g.link, g.linkSet = l, true
	switch {
	case !l.Up:
		g.set(L1, Closed, ReasonNoLink)
	default:
		g.set(L1, Open, "")
	}
	switch {
	case g.status[L1].State != Open:
	case l.Address == sysapi.AddressAPIPA:
		g.set(L2, Closed, ReasonAPIPA)
	case l.Address == sysapi.AddressNone:
		g.set(L2, Closed, ReasonNoAddress)
	default:
		g.set(L2, Open, "")
	}
	if g.status[L2].State == Open {
		switch l.Route {
		case RouteOnLink:
			g.set(L3, Open, ReasonOnLink)
		case RouteNone:
			g.set(L3, Closed, ReasonNoRoute)
		default:
			if r := g.status[L3].Reason; r == ReasonOnLink || r == ReasonNoRoute {
				g.set(L3, Unknown, "") // back to evidence
			}
		}
	}
	g.normalize()
}

// Reset is the adapter-change reset (SPEC §6): L3–L5 return to unknown
// and are re-evaluated from fresh evidence.
func (g *Gates) Reset() {
	for _, l := range []Layer{L3, L4, L5} {
		g.clearLayer(l)
		g.set(l, Unknown, "")
	}
	if g.linkSet {
		g.SetLink(g.link)
	}
}

func (g *Gates) clearLayer(l Layer) {
	g.evidence[l] = map[string]probe.Sample{}
	g.okStreak[l], g.failStrk[l] = 0, 0
	g.lastRound[l] = time.Time{}
}

// Observe records a sample as evidence; rounds run in Tick. Skipped
// samples withdraw the series' evidence.
func (g *Gates) Observe(s probe.Sample) {
	sp, ok := g.series[s.Series]
	if !ok {
		return
	}
	if s.Result == probe.Skipped {
		delete(g.evidence[sp.Defines], s.Series)
		if sp.L3Evidence {
			delete(g.evidence[L3], s.Series)
		}
		return
	}
	if s.Recovery {
		if sp.L3Evidence {
			g.evidence[L3][s.Series] = s
		}
		return
	}
	if sp.Defines != None {
		g.evidence[sp.Defines][s.Series] = s
	}
	if sp.L3Evidence {
		g.evidence[L3][s.Series] = s
	}
}

// Tick runs the hysteresis rounds that are due, lowest layer first, so a
// layer that opens lets the next one be evaluated in the same tick. Call
// it about once per second.
func (g *Gates) Tick() {
	now := g.clock.Now()
	for _, l := range []Layer{L3, L4, L5} {
		p := g.period[l]
		if p == 0 || (!g.lastRound[l].IsZero() && now.Sub(g.lastRound[l]) < p-p/10) {
			continue
		}
		if g.round(l) {
			g.lastRound[l] = now
		}
	}
}

// fresh reports whether evidence still describes the present: the
// series' latest result counts for 1.5 intervals.
func (g *Gates) fresh(s probe.Sample) bool {
	iv := g.series[s.Series].Interval
	return g.clock.Now().Sub(s.Time) <= iv+iv/2
}

// round evaluates one hysteresis round of layer l: success if any fresh
// evidence is ok; failure if none is ok and at least one of the layer's
// own defining series failed (L4 evidence alone never fails L3). It
// reports whether a round took place.
func (g *Gates) round(l Layer) bool {
	if g.status[l-1].State != Open {
		return false // its defining probes are skipped or waiting
	}
	if l == L3 && g.link.Route != RouteGateway {
		return false
	}
	success, failure := false, false
	for id, s := range g.evidence[l] {
		if !g.fresh(s) {
			continue
		}
		if s.Result == probe.OK {
			success = true
		} else if g.series[id].Defines == l {
			failure = true
		}
	}
	switch {
	case success:
		g.okStreak[l]++
		g.failStrk[l] = 0
	case failure:
		g.failStrk[l]++
		g.okStreak[l] = 0
	default:
		return false
	}
	st := g.status[l].State
	switch {
	case success && (st == Unknown || (st == Closed && g.okStreak[l] >= g.okCount)):
		g.set(l, Open, "")
	case failure && st != Closed && g.failStrk[l] >= g.failCount:
		g.set(l, Closed, closedReason(l))
	}
	g.normalize()
	return true
}

func closedReason(l Layer) string {
	switch l {
	case L3:
		return ReasonGatewayDown
	case L4:
		return ReasonInternetDown
	case L5:
		return ReasonDNSDown
	default:
		return ""
	}
}

// set changes a layer's state. A layer that is not open makes every layer
// above it unknown: they are re-decided from fresh evidence once it opens
// ("recovery reopens dependents").
func (g *Gates) set(l Layer, st State, reason string) {
	cur := g.status[l]
	if cur.State == st && cur.Reason == reason {
		return
	}
	now := g.clock.Now()
	if cur.State != st {
		g.history = append(g.history, Transition{Time: now, Layer: l, From: cur.State, To: st, Reason: reason})
		g.okStreak[l], g.failStrk[l] = 0, 0
	}
	g.status[l] = Status{State: st, Reason: reason, Since: now}
	if st != Open {
		for above := l + 1; above <= L5; above++ {
			if g.status[above].State != Unknown {
				g.history = append(g.history, Transition{Time: now, Layer: above, From: g.status[above].State, To: Unknown})
				g.status[above] = Status{State: Unknown, Since: now}
			}
			g.clearLayer(above)
		}
	}
}

// normalize applies rules that follow from other layers: without DNS
// servers L5 is closed as soon as L4 is open.
func (g *Gates) normalize() {
	if g.linkSet && !g.link.HasDNS && g.status[L4].State == Open && g.status[L5].State != Closed {
		g.set(L5, Closed, ReasonNoDNSServers)
	}
}

// Recovering reports whether a sample of series id taken now is an L3
// recovery run: an L3Recovery series while L1/L2 are open, the route goes
// through a gateway and L3 is not open (closed, or unknown at start-up and
// after a reset). Such samples are L3 evidence only (SPEC §6).
func (g *Gates) Recovering(id string) bool {
	sp, ok := g.series[id]
	return ok && sp.L3Recovery && g.status[L1].State == Open && g.status[L2].State == Open &&
		g.link.Route == RouteGateway && g.status[L3].State != Open
}

// Decision tells the scheduler what a series may do now: skip with the
// reason of the lowest closed layer it requires, wait while one of them is
// unknown, otherwise run. An L3 recovery series runs while L3 is not open.
func (g *Gates) Decision(id string) sched.Decision {
	sp, ok := g.series[id]
	if !ok {
		return sched.Decision{Action: sched.Run}
	}
	if g.Recovering(id) {
		return sched.Decision{Action: sched.Run}
	}
	for l := L1; l <= sp.Requires; l++ {
		if st := g.status[l]; st.State == Closed {
			return sched.Decision{Action: sched.Skip, Reason: skipReason(st)}
		}
	}
	if sp.NeedsGateway && g.status[L2].State == Open {
		switch g.link.Route {
		case RouteOnLink:
			return sched.Decision{Action: sched.Skip, Reason: SkipOnLink}
		case RouteNone:
			return sched.Decision{Action: sched.Skip, Reason: ReasonNoRoute}
		}
	}
	for l := L1; l <= sp.Requires; l++ {
		if g.status[l].State == Unknown {
			return sched.Decision{Action: sched.Wait}
		}
	}
	return sched.Decision{Action: sched.Run}
}

func skipReason(st Status) string {
	if st.Reason == ReasonNoDNSServers {
		return ReasonDNSDown
	}
	return st.Reason
}
