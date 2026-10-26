package engine

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/config"
	"netdiag/internal/gate"
	"netdiag/internal/probe"
	"netdiag/internal/sched"
	"netdiag/internal/stats"
	"netdiag/internal/store"
	"netdiag/internal/sysapi"
	"netdiag/internal/verdict"
)

// recentLen is how many newest non-skipped samples per series the verdict
// sees for "consecutive failures" checks.
const recentLen = 5

// cpuAvgWindow is the CPU_HIGH averaging window (SPEC §8).
const cpuAvgWindow = 10 * time.Second

// System is what the OS reports about the network (from sysapi or a
// simulation scenario).
type System struct {
	Adapters []sysapi.Adapter
	Route    sysapi.Route
	RouteErr error
	WiFi     []sysapi.WiFiStatus
}

// Snapshot is the evaluated state at one moment.
type Snapshot struct {
	Time           time.Time
	Result         verdict.Result
	Gates          [gate.L5 + 1]gate.Status
	Primary        sysapi.Primary
	WiFi           *sysapi.WiFiStatus
	AdapterChanges int
	Specs          []Spec
	// Hops are the TRC-01 hops seen so far (empty before the first trace).
	Hops []probe.Hop
	// AdapterEvents are the adapter changes of the run, oldest first.
	AdapterEvents []AdapterChange
	// AdapterEventsDropped counts old events no longer in AdapterEvents
	// (0 until the Phase 9 cap; consumers track AdapterChange.Seq).
	AdapterEventsDropped int
	// CPUAvg10 is the average CPU % over the last 10 s (valid with HasCPU).
	CPUAvg10 float64
	HasCPU   bool
}

// Engine is safe for concurrent use: the scheduler's series goroutines
// call Decision and OnSample, the UI calls Snapshot.
type Engine struct {
	mu    sync.Mutex
	cfg   *config.Config
	clock clock.Clock
	store *store.Store
	gates *gate.Gates

	specs []Spec
	byID  map[string]Spec

	primary        sysapi.Primary
	wifi           *sysapi.WiFiStatus
	havePrimary    bool
	adapterChanges int
	csvErr         bool
	hops           func() []probe.Hop
	generation     int // bumped by every adapter change
	events         []AdapterChange
	eventSeq       int // Seq of the newest adapter event
}

// AdapterInfo describes the primary adapter for the change event.
type AdapterInfo struct {
	Name    string
	IPv4    string // first address, "" if none
	Gateway string // IP, "on-link" or "" (no default route)
}

// AdapterChange is the recorded "adapter_change" event (SPEC §6): shown in
// the plain output now, in the CSV events and as a graph marker later.
type AdapterChange struct {
	// Seq numbers the events of a run from 1, never reused, so a consumer
	// can tell which events it has seen even when old ones are dropped.
	Seq      int
	Time     time.Time
	From, To AdapterInfo
}

func (a AdapterChange) String() string {
	return fmt.Sprintf("adapter change: %s -> %s", a.From, a.To)
}

func (i AdapterInfo) String() string {
	name := i.Name
	if name == "" {
		name = "none"
	}
	ip := i.IPv4
	if ip == "" {
		ip = "no address"
	}
	gw := i.Gateway
	if gw == "" {
		gw = "no gateway"
	}
	return fmt.Sprintf("%s %s gateway %s", name, ip, gw)
}

func adapterInfo(p sysapi.Primary) AdapterInfo {
	if !p.Found() {
		return AdapterInfo{}
	}
	i := AdapterInfo{Name: p.Adapter.Name}
	if len(p.Adapter.IPv4) > 0 {
		i.IPv4 = p.Adapter.IPv4[0].Addr().String()
	}
	switch {
	case p.OnLink:
		i.Gateway = "on-link"
	case p.Gateway.IsValid():
		i.Gateway = p.Gateway.String()
	}
	return i
}

// TraceID is the gate-only series of TRC-01: the tracer asks it whether to
// probe (L3 open). The hop series themselves are added as they are found.
const TraceID = "TRC-01"

// New returns an engine for cfg on clock c.
func New(cfg *config.Config, c clock.Clock) *Engine {
	return &Engine{
		cfg:   cfg,
		clock: c,
		store: store.New(c, sec(cfg.Window.Seconds), time.Duration(cfg.History.Minutes)*time.Minute),
		gates: gate.New(cfg.Gate.FailCount, cfg.Gate.OKCount, c),
		byID:  map[string]Spec{},
	}
}

// Store gives read access to the samples (graphs, report).
func (e *Engine) Store() *store.Store { return e.store }

// SetSystem applies the OS state: it picks the primary adapter, sets L1–L3
// inputs and, when the primary adapter, its address or gateway changed,
// resets L3–L5 (SPEC §6 adapter change).
func (e *Engine) SetSystem(s System) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := sysapi.SelectPrimary(s.Adapters, s.Route, s.RouteErr)
	if e.havePrimary && primaryKey(e.primary) != primaryKey(p) {
		e.adapterChanges++
		e.generation++
		e.gates.Reset()
		// New series start: nothing measured on the old path may count
		// towards the loss and quality of the new one. Only the CPU load
		// is a property of the machine.
		var ids []string
		for _, id := range e.store.IDs() {
			if sp, ok := e.byID[id]; !ok || sp.Probe != "SYS-01" {
				ids = append(ids, id)
			}
		}
		e.store.ResetSeries(ids)
		e.eventSeq++
		e.events = append(e.events, AdapterChange{Seq: e.eventSeq, Time: e.clock.Now(), From: adapterInfo(e.primary), To: adapterInfo(p)})
	}
	e.primary, e.havePrimary = p, true
	e.wifi = nil
	if p.Found() && p.Adapter.Type == sysapi.AdapterWiFi {
		for i := range s.WiFi {
			if strings.EqualFold(s.WiFi[i].InterfaceGUID, p.Adapter.GUID) {
				w := s.WiFi[i]
				e.wifi = &w
			}
		}
	}
	mode := gate.RouteGateway
	switch {
	case p.Reason != sysapi.PrimaryByRoute:
		mode = gate.RouteNone
	case p.OnLink:
		mode = gate.RouteOnLink
	}
	e.gates.SetLink(gate.Link{
		Up:      p.LinkUp(),
		Address: p.Address(),
		Route:   mode,
		HasDNS:  len(p.Adapter.DNSServers) > 0,
	})
}

func primaryKey(p sysapi.Primary) string {
	return fmt.Sprintf("%s|%d|%s|%v|%v", p.Reason, p.Adapter.Index, p.Adapter.Name, p.Adapter.IPv4, p.Gateway)
}

// Primary returns the current primary adapter.
func (e *Engine) Primary() sysapi.Primary {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.primary
}

// SetSeries replaces the series catalog.
func (e *Engine) SetSeries(specs []Spec) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.specs = append([]Spec(nil), specs...)
	e.byID = make(map[string]Spec, len(specs))
	gs := make([]gate.Series, 0, len(specs)+1)
	ids := make([]string, 0, len(specs))
	for _, s := range specs {
		e.byID[s.ID] = s
		gs = append(gs, s.Series)
		e.store.Register(s.ID, s.Interval)
		ids = append(ids, s.ID)
	}
	e.store.Retain(ids) // series of a previous generation are gone
	// TRC-01 runs whenever L3 is open (SPEC §6)
	gs = append(gs, gate.Series{ID: TraceID, Interval: time.Second, Requires: gate.L3})
	e.gates.SetSeries(gs)
}

// Generation counts the adapter changes; a run of series belongs to one
// generation (SinkFor, AddSeriesFor).
func (e *Engine) Generation() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.generation
}

// SinkFor returns the sample sink of one generation: samples that arrive
// after an adapter change from probes still running for the old adapter
// (a ping in flight) are dropped instead of polluting the new series.
func (e *Engine) SinkFor(gen int) func(probe.Sample) {
	return func(s probe.Sample) {
		e.mu.Lock()
		stale := gen != e.generation
		e.mu.Unlock()
		if !stale {
			e.OnSample(s)
		}
	}
}

// AddSeries adds one series after SetSeries (TRC-01 hops appear while the
// trace runs). It does nothing if the series exists.
func (e *Engine) AddSeries(s Spec) { e.addSeries(-1, s) }

// AddSeriesFor is AddSeries for a generation; a stale one is ignored.
func (e *Engine) AddSeriesFor(gen int, s Spec) { e.addSeries(gen, s) }

func (e *Engine) addSeries(gen int, s Spec) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if gen >= 0 && gen != e.generation {
		return
	}
	if _, ok := e.byID[s.ID]; ok {
		return
	}
	e.specs = append(e.specs, s)
	e.byID[s.ID] = s
	e.store.Register(s.ID, s.Interval)
}

// SetTrace sets where snapshots get the trace hops from.
func (e *Engine) SetTrace(hops func() []probe.Hop) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hops = hops
}

// TraceDecision is the gate of TRC-01.
func (e *Engine) TraceDecision() sched.Decision { return e.Decision(TraceID) }

// AdapterEvents returns the recorded adapter changes, oldest first.
func (e *Engine) AdapterEvents() []AdapterChange {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]AdapterChange(nil), e.events...)
}

// AdapterChanges counts the primary adapter changes so far.
func (e *Engine) AdapterChanges() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.adapterChanges
}

// Specs returns the series catalog.
func (e *Engine) Specs() []Spec {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Spec(nil), e.specs...)
}

// Decision is the scheduler gate for a series.
func (e *Engine) Decision(id string) sched.Decision {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gates.Decision(id)
}

// OnSample records a sample (the scheduler sink). A sample of an L3
// recovery series that arrives while L3 is not open is marked Recovery.
func (e *Engine) OnSample(s probe.Sample) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s.Result != probe.Skipped && e.gates.Recovering(s.Series) {
		s.Recovery = true
	}
	e.store.Add(s)
	e.gates.Observe(s)
}

// Tick runs the gate rounds that are due; call it about once per second.
func (e *Engine) Tick() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gates.Tick()
}

// Reset clears the rolling-window stats, Sent counters and graph history
// of every series (the `r` key, SPEC §9). Gates, verdict history and
// adapter events are kept.
func (e *Engine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.store.Reset()
}

// SetCSVError raises or clears CSV_WRITE_ERROR.
func (e *Engine) SetCSVError(on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.csvErr = on
}

// GateHistory returns every gate transition so far.
func (e *Engine) GateHistory() []gate.Transition {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gates.History()
}

// Snapshot evaluates the current state.
func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()
	in := verdict.Input{
		Address:        e.primary.Address(),
		WiFi:           e.wifi,
		TeamsEnabled:   len(e.cfg.Teams.Relays) > 0,
		CSVError:       e.csvErr,
		QualitySources: QualitySources(e.cfg),
		Thresholds:     e.cfg.Thresholds,
		MinSamples:     e.cfg.Window.MinSamples,
		FailCount:      e.cfg.Gate.FailCount,
	}
	for _, l := range gate.Layers {
		in.Gates[l] = e.gates.Status(l)
	}
	var cpuSum float64
	var cpuN int
	for _, sp := range e.specs {
		window := e.store.Window(sp.ID)
		in.Series = append(in.Series, verdict.SeriesData{
			ID: sp.ID, Probe: sp.Probe, Target: sp.Target, Name: sp.Name,
			Window:        stats.Summarize(window),
			Recent:        recent(e.store.History(sp.ID), recentLen),
			TrailingFails: verdict.TrailingFails(window),
		})
		if sp.Probe == "SYS-01" {
			for _, s := range window {
				if s.Result == probe.OK && now.Sub(s.Time) < cpuAvgWindow {
					cpuSum += s.Value
					cpuN++
				}
			}
		}
	}
	if cpuN > 0 {
		in.CPUAvg10, in.HasCPU = cpuSum/float64(cpuN), true
	}
	snap := Snapshot{
		Time:           now,
		Result:         verdict.Evaluate(in),
		Gates:          in.Gates,
		Primary:        e.primary,
		AdapterChanges: e.adapterChanges,
		Specs:          slices.Clone(e.specs),
		CPUAvg10:       in.CPUAvg10,
		HasCPU:         in.HasCPU,
	}
	if e.wifi != nil {
		w := *e.wifi
		snap.WiFi = &w
	}
	if e.hops != nil {
		snap.Hops = e.hops()
	}
	snap.AdapterEvents = slices.Clone(e.events)
	return snap
}

// recent returns the newest n non-skipped, non-recovery samples, oldest
// first.
func recent(history []probe.Sample, n int) []probe.Sample {
	var out []probe.Sample
	for i := len(history) - 1; i >= 0 && len(out) < n; i-- {
		if history[i].Result != probe.Skipped && !history[i].Recovery {
			out = append(out, history[i])
		}
	}
	slices.Reverse(out)
	return out
}
