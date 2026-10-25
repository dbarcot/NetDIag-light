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
}

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
		e.gates.Reset()
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
	gs := make([]gate.Series, 0, len(specs))
	for _, s := range specs {
		e.byID[s.ID] = s
		gs = append(gs, s.Series)
		e.store.Register(s.ID, s.Interval)
	}
	e.gates.SetSeries(gs)
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
	}
	if e.wifi != nil {
		w := *e.wifi
		snap.WiFi = &w
	}
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
