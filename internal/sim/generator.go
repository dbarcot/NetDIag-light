package sim

import (
	"hash/fnv"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"netdiag/internal/engine"
	"netdiag/internal/probe"
)

// Generator produces deterministic probe results for a scenario. It is
// safe for concurrent use (Live calls it from series goroutines).
type Generator struct {
	sc     *Scenario
	mu     sync.Mutex
	rngs   map[string]*rand.Rand
	counts map[string]int
}

// NewGenerator returns a generator for sc.
func NewGenerator(sc *Scenario) *Generator {
	return &Generator{sc: sc, rngs: map[string]*rand.Rand{}, counts: map[string]int{}}
}

// effective is a rule after merging every matching rule.
type effective struct {
	result   string
	rtt      []float64
	lossPct  float64
	arpState string
	value    *float64
	errClass string
	detail   string
	pattern  string
	patFrom  int
	bound    string
}

// probeDefaults are the values a probe reports when no rule sets them.
func probeDefaults(probeID string) effective {
	e := effective{result: "ok", rtt: []float64{12, 3}}
	switch probeID {
	case "LAN-01":
		e.rtt = []float64{1.5, 0.3}
	case "MTU-01":
		v := 1500.0
		e.value = &v
	case "SYS-01":
		v := 10.0
		e.value = &v
	}
	return e
}

func (r Rule) matches(sp engine.Spec, t int) bool {
	if r.Probe != "*" && r.Probe != sp.Probe && !slices.Contains(r.Probes, sp.Probe) {
		return false
	}
	if r.Target != "" && r.Target != sp.Target && !strings.HasSuffix(sp.ID, ":"+r.Target) {
		return false
	}
	if r.FromS != nil && t < *r.FromS {
		return false
	}
	if r.ToS != nil && t >= *r.ToS {
		return false
	}
	return true
}

func (e *effective) apply(r Rule) {
	if r.Result != "" {
		e.result = r.Result
		e.pattern = ""
	}
	if r.RTTms != nil {
		e.rtt = r.RTTms
	}
	if r.LossPct != nil {
		e.lossPct = *r.LossPct
	}
	if r.ARPState != "" {
		e.arpState = r.ARPState
	}
	if r.Value != nil {
		e.value = r.Value
	}
	if r.ErrClass != "" {
		e.errClass = r.ErrClass
	}
	if r.Detail != "" {
		e.detail = r.Detail
	}
	if r.Bound != "" {
		e.bound = r.Bound
	}
	if r.Pattern != "" {
		e.pattern = r.Pattern
		e.patFrom = 0
		if r.FromS != nil {
			e.patFrom = *r.FromS
		}
	}
}

func (g *Generator) rng(id string) *rand.Rand {
	if r, ok := g.rngs[id]; ok {
		return r
	}
	h := fnv.New64a()
	h.Write([]byte(g.sc.Name))
	a := h.Sum64()
	h.Write([]byte(id))
	r := rand.New(rand.NewPCG(a, h.Sum64()))
	g.rngs[id] = r
	return r
}

// Sample returns the result of series sp at second t of the scenario.
func (g *Generator) Sample(sp engine.Spec, t int) probe.Sample {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := probeDefaults(sp.Probe)
	e.apply(g.sc.Default) // the scenario default applies to every probe, before the rules
	for _, r := range g.sc.Rules {
		if r.matches(sp, t) {
			e.apply(r)
		}
	}
	k := g.counts[sp.ID]
	g.counts[sp.ID] = k + 1

	result := e.result
	if e.pattern != "" {
		i := (t - e.patFrom) % len(e.pattern)
		result = map[byte]string{'o': "ok", 'f': "fail", 's': "skipped", 'c': "captive"}[e.pattern[i]]
	}
	// loss_pct drops an evenly spread, deterministic fraction of samples
	if result == "ok" && e.lossPct > 0 && int(float64(k+1)*e.lossPct/100) > int(float64(k)*e.lossPct/100) {
		result = "fail"
	}

	s := probe.Sample{Detail: e.detail}
	if sp.Probe == "LAN-02" && e.arpState != "" && result == "ok" {
		switch e.arpState {
		case "stale", "probe", "delay":
			return probe.Sample{Result: probe.Fail, ErrClass: probe.ClassUnconfirmed, Detail: "ARP " + e.arpState}
		case "incomplete", "unreachable":
			return probe.Sample{Result: probe.Fail, ErrClass: probe.ClassUnreachable, Detail: "ARP " + e.arpState}
		case "busy":
			return probe.Sample{Result: probe.Fail, ErrClass: probe.ClassOther, Detail: "previous ARP still pending"}
		}
	}
	switch result {
	case "ok":
		s.Result = probe.OK
		ms := max(g.rng(sp.ID).NormFloat64()*e.rtt[1]+e.rtt[0], 0.05)
		s.RTT = time.Duration(ms * float64(time.Millisecond))
		if e.value != nil {
			s.Value = *e.value
		}
		if sp.Probe == "MTU-01" && s.Value < probe.MaxMTU {
			s.Bound = e.bound
			if s.Bound == "" {
				s.Bound = probe.BoundSignalled
			}
		}
	case "skipped":
		reason := e.detail
		if reason == "" {
			reason = "name not resolved"
		}
		return probe.SkippedSample(reason)
	case "captive":
		s.Result, s.ErrClass = probe.Fail, probe.ClassCaptive
		if s.Detail == "" {
			s.Detail = "redirect (captive portal)"
		}
	case "dns":
		s.Result, s.ErrClass = probe.Fail, probe.ClassDNS
		if s.Detail == "" {
			s.Detail = "name not resolved"
		}
	default: // fail
		s.Result, s.ErrClass = probe.Fail, e.errClass
		if s.ErrClass == "" {
			s.ErrClass = probe.ClassTimeout
		}
		if s.Detail == "" {
			s.Detail = "simulated " + s.ErrClass
		}
	}
	return s
}
