// Package sched runs probes on their intervals: random start offsets, at
// most one run in flight per series, and gate-aware pausing.
package sched

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/probe"
)

// Action is what a gate allows for one tick.
type Action int

const (
	Run  Action = iota // run the probe
	Skip               // record a skipped sample with Reason
	Wait               // gate not decided yet: record nothing (SPEC §6 start-up)
)

// Decision is a gate's answer for one tick.
type Decision struct {
	Action Action
	Reason string // for Skip, e.g. "gateway down"
}

// Series is one probe toward one endpoint.
type Series struct {
	ID       string // e.g. "NET-01:1.1.1.1"
	Probe    string // probe ID, e.g. "NET-01"
	Target   string
	Interval time.Duration
	P        probe.Probe
	// Gate is asked before every tick; nil means always Run.
	Gate func() Decision
}

// WaitRetry is how soon a series asks its gate again after Wait; once it
// runs, its grid restarts from that run.
const WaitRetry = time.Second

// MaxStartOffset caps the random start offset (SPEC §5: probes get a small
// random offset so they don't fire in bursts).
const MaxStartOffset = time.Second

// Scheduler runs series on a clock.
type Scheduler struct {
	clock clock.Clock
	// Offset returns the start offset of a series; defaults to uniform in
	// [0, min(interval, MaxStartOffset)).
	Offset func(interval time.Duration) time.Duration
	// Logger, if set, receives probe panics with their stack.
	Logger *slog.Logger
}

// New returns a scheduler on c with random start offsets.
func New(c clock.Clock) *Scheduler {
	return &Scheduler{clock: c, Offset: RandomOffset}
}

// RandomOffset is the default start offset.
func RandomOffset(interval time.Duration) time.Duration {
	span := min(interval, MaxStartOffset)
	if span <= 0 {
		return 0
	}
	return rand.N(span)
}

// Run starts every series and blocks until ctx ends and all series
// goroutines have returned. sink receives every sample and is called
// concurrently from the series goroutines.
func (s *Scheduler) Run(ctx context.Context, series []Series, sink func(probe.Sample)) {
	var wg sync.WaitGroup
	start := s.clock.Now()
	for _, sr := range series {
		if sr.Interval <= 0 {
			panic("sched: series " + sr.ID + " has no interval")
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.loop(ctx, sr, start.Add(s.Offset(sr.Interval)), sink)
		}()
	}
	wg.Wait()
}

// loop runs one series on the grid first, first+interval, … The probe runs
// in this goroutine, so runs never overlap; ticks missed while a probe was
// running are dropped, never caught up.
func (s *Scheduler) loop(ctx context.Context, sr Series, next time.Time, sink func(probe.Sample)) {
	for {
		t := s.clock.NewTimer(next.Sub(s.clock.Now()))
		var now time.Time // when the timer fired: the tick's own time
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case now = <-t.C():
		}
		d := Decision{Action: Run}
		if sr.Gate != nil {
			d = sr.Gate()
		}
		if d.Action == Wait {
			// The gate is not decided yet: ask again soon instead of a whole
			// interval later (a 10-minute MTU-01 must still run at start);
			// the grid restarts from the run that follows.
			next = now.Add(min(sr.Interval, WaitRetry))
			continue
		}
		var sm probe.Sample
		if d.Action == Skip {
			sm = probe.SkippedSample(d.Reason)
		} else {
			sm = s.runProbe(ctx, sr)
			if ctx.Err() != nil {
				return // cancelled mid-run: not a measurement
			}
		}
		sm.Time, sm.Series, sm.Probe, sm.Target = now, sr.ID, sr.Probe, sr.Target
		sink(sm)
		next = nextSlot(next, sr.Interval, s.clock.Now())
		if sm.RerunAfter > 0 {
			// an early re-run (MTU-01); the grid restarts from it
			if early := s.clock.Now().Add(sm.RerunAfter); early.Before(next) {
				next = early
			}
		}
	}
}

// runProbe runs the probe, turning a panic into a local_error failure so
// one broken probe never stops its series or the program.
func (s *Scheduler) runProbe(ctx context.Context, sr Series) (sm probe.Sample) {
	defer func() {
		if v := recover(); v != nil {
			if s.Logger != nil {
				s.Logger.Error("probe panic", "series", sr.ID, "panic", v, "stack", string(debug.Stack()))
			}
			sm = probe.Sample{Result: probe.Fail, ErrClass: probe.ClassLocal, Detail: fmt.Sprintf("panic: %v", v)}
		}
	}()
	return sr.P.Run(ctx)
}

// nextSlot returns the first grid slot after now, starting from prev.
func nextSlot(prev time.Time, interval time.Duration, now time.Time) time.Time {
	next := prev.Add(interval)
	if next.After(now) {
		return next
	}
	missed := now.Sub(next)/interval + 1
	return next.Add(missed * interval)
}
