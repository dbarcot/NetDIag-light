package sim

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/engine"
	"netdiag/internal/gate"
	"netdiag/internal/probe"
	"netdiag/internal/sched"
)

// Epoch is the fake-clock time of second 0 in deterministic runs.
var Epoch = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// Outcome is one checked expectation.
type Outcome struct {
	Expect   Expect
	Snapshot engine.Snapshot
	Errors   []string // empty when the expectation held
}

// orderSpecs sorts series lowest required layer first, so within one
// second the samples that open a layer are recorded before the probes
// above it ask their gate (the order real start offsets would give over a
// few seconds).
func orderSpecs(specs []engine.Spec) []engine.Spec {
	out := slices.Clone(specs)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Requires != out[j].Requires {
			return out[i].Requires < out[j].Requires
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func intervalSeconds(sp engine.Spec) int { return max(int(sp.Interval/time.Second), 1) }

// Run plays a scenario deterministically on a fake clock, second by
// second: every series due at second t runs (all series start at 0), then
// the gates tick, then the expectations for t are checked.
func Run(sc *Scenario) ([]Outcome, error) {
	cfg := sc.Config()
	c := clock.NewFake(Epoch)
	eng := engine.New(cfg, c)
	gen := NewGenerator(sc)

	sys, err := sc.SystemAt(0)
	if err != nil {
		return nil, err
	}
	eng.SetSystem(sys)
	specs := orderSpecs(engine.BuildSeries(cfg, eng.Primary()))
	eng.SetSeries(specs)

	var out []Outcome
	ev, ex := 0, 0
	due := map[string]int{} // next second each series runs; same rules as sched
	for t := 0; t <= sc.DurationS; t++ {
		if t > 0 {
			c.Advance(time.Second)
		}
		changed := false
		for ; ev < len(sc.Events) && sc.Events[ev].AtS == t; ev++ {
			changed = true
		}
		if changed {
			sys, err := sc.SystemAt(t)
			if err != nil {
				return nil, err
			}
			eng.SetSystem(sys)
			specs = orderSpecs(engine.BuildSeries(cfg, eng.Primary()))
			eng.SetSeries(specs)
			eng.SetCSVError(sc.CSVErrorAt(t))
		}
		for _, sp := range specs {
			if next, ok := due[sp.ID]; ok && t < next {
				continue
			}
			var s probe.Sample
			d := eng.Decision(sp.ID)
			if d.Action == sched.Wait {
				due[sp.ID] = t + 1 // ask again next second (sched.WaitRetry)
				continue
			}
			due[sp.ID] = t + intervalSeconds(sp)
			switch d.Action {
			case sched.Skip:
				s = probe.SkippedSample(d.Reason)
			default:
				s = gen.Sample(sp, t)
			}
			s.Time, s.Series, s.Probe, s.Target = c.Now(), sp.ID, sp.Probe, sp.Target
			eng.OnSample(s)
		}
		eng.Tick()
		for ; ex < len(sc.Expect) && sc.Expect[ex].AtS == t; ex++ {
			snap := eng.Snapshot()
			out = append(out, Outcome{Expect: sc.Expect[ex], Snapshot: snap, Errors: check(sc.Expect[ex], snap, eng)})
		}
	}
	return out, nil
}

// check compares a snapshot with an expectation.
func check(e Expect, s engine.Snapshot, eng *engine.Engine) []string {
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	r := s.Result
	if e.Verdict != "" && string(r.Verdict) != e.Verdict {
		fail("verdict %s (%s), want %s", r.Verdict, r.Detail, e.Verdict)
	}
	if e.ExitCode != nil && r.ExitCode != *e.ExitCode {
		fail("exit code %d, want %d", r.ExitCode, *e.ExitCode)
	}
	if e.DetailContains != "" && !strings.Contains(r.Detail, e.DetailContains) {
		fail("detail %q lacks %q", r.Detail, e.DetailContains)
	}
	for _, f := range e.FlagsInclude {
		if !slices.Contains(r.Flags, f) {
			fail("flags %v lack %s", r.Flags, f)
		}
	}
	for _, f := range e.FlagsExclude {
		if slices.Contains(r.Flags, f) {
			fail("flags %v contain %s", r.Flags, f)
		}
	}
	for _, p := range e.Skipped {
		n := 0
		for _, sp := range s.Specs {
			if sp.Probe != p {
				continue
			}
			n++
			h := eng.Store().History(sp.ID)
			if len(h) == 0 || h[len(h)-1].Result != probe.Skipped {
				last := "none"
				if len(h) > 0 {
					last = string(h[len(h)-1].Result)
				}
				fail("%s: newest sample %s, want skipped", sp.ID, last)
			}
		}
		if n == 0 {
			fail("no series of probe %s", p)
		}
	}
	for name, want := range e.Gates {
		l := gate.Layer(int(name[1] - '0'))
		if got := s.Gates[l].State.String(); got != want {
			fail("%s %s (%s), want %s", name, got, s.Gates[l].Reason, want)
		}
	}
	if e.QualitySource != "" && r.Quality.Source != e.QualitySource {
		fail("quality source %q, want %q", r.Quality.Source, e.QualitySource)
	}
	if e.QualityStatus != "" && r.Quality.Status != e.QualityStatus {
		fail("quality status %q (MOS %.2f), want %q", r.Quality.Status, r.Quality.MOS, e.QualityStatus)
	}
	if e.AdapterChanges != nil && s.AdapterChanges != *e.AdapterChanges {
		fail("adapter changes %d, want %d", s.AdapterChanges, *e.AdapterChanges)
	}
	if e.PrimaryAdapter != "" && s.Primary.Adapter.Name != e.PrimaryAdapter {
		fail("primary adapter %q, want %q", s.Primary.Adapter.Name, e.PrimaryAdapter)
	}
	return errs
}
