package sim

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/engine"
	"netdiag/internal/probe"
	"netdiag/internal/sched"
)

// Live plays a scenario in (clock) real time through the real scheduler,
// with random start offsets, and prints a timestamped line to out
// whenever the verdict or the flags change (the plain console format of
// SPEC §9). It returns the final snapshot when the scenario ends or ctx
// is cancelled.
//
// The series catalog is built once at start; an adapter change resets the
// gates but does not rebuild series (series restart is Phase 9).
func Live(ctx context.Context, sc *Scenario, c clock.Clock, out io.Writer) (engine.Snapshot, error) {
	cfg := sc.Config()
	eng := engine.New(cfg, c)
	gen := NewGenerator(sc)
	sys, err := sc.SystemAt(0)
	if err != nil {
		return engine.Snapshot{}, err
	}
	eng.SetSystem(sys)
	specs := engine.BuildSeries(cfg, eng.Primary())
	eng.SetSeries(specs)

	start := c.Now()
	second := func() int { return int(c.Now().Sub(start) / time.Second) }
	series := make([]sched.Series, 0, len(specs))
	for _, sp := range specs {
		series = append(series, sched.Series{
			ID: sp.ID, Probe: sp.Probe, Target: sp.Target, Interval: sp.Interval,
			P:    probe.Func(func(context.Context) probe.Sample { return gen.Sample(sp, second()) }),
			Gate: func() sched.Decision { return eng.Decision(sp.ID) },
		})
	}

	fmt.Fprintf(out, "simulation %q (%d s): %s\n", sc.Name, sc.DurationS, sc.Description)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // events, then the end of the scenario
		defer wg.Done()
		defer cancel()
		for _, e := range sc.Events {
			if !sleepUntil(runCtx, c, start.Add(time.Duration(e.AtS)*time.Second)) {
				return
			}
			if s, err := sc.SystemAt(e.AtS); err == nil {
				eng.SetSystem(s)
			}
			eng.SetCSVError(sc.CSVErrorAt(e.AtS))
		}
		sleepUntil(runCtx, c, start.Add(time.Duration(sc.DurationS)*time.Second))
	}()
	var last string
	report := func(prefix string) {
		snap := eng.Snapshot()
		line := engine.Describe(snap)
		if line != last || prefix != "" {
			fmt.Fprintf(out, "%s  %s%s\n", snap.Time.Local().Format("15:04:05"), prefix, line)
			last = line
		}
	}
	go func() { // gate rounds and change reporting, once per second
		defer wg.Done()
		tk := c.NewTicker(time.Second)
		defer tk.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-tk.C():
				eng.Tick()
				report("")
			}
		}
	}()
	sched.New(c).Run(runCtx, series, eng.OnSample)
	wg.Wait()
	eng.Tick()
	report("end of simulation: ")
	return eng.Snapshot(), nil
}

// sleepUntil waits on the clock until t; false if ctx ended first.
func sleepUntil(ctx context.Context, c clock.Clock, t time.Time) bool {
	d := t.Sub(c.Now())
	if d <= 0 {
		return ctx.Err() == nil
	}
	tm := c.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-tm.C():
		return true
	}
}
