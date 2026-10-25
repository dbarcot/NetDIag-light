package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/config"
	"netdiag/internal/probe"
	"netdiag/internal/sched"
	"netdiag/internal/sysapi"
)

// RouteProbeTarget is the destination whose best route picks the primary
// adapter (SPEC §5 LINK-01).
var RouteProbeTarget = netip.MustParseAddr("1.1.1.1")

// LiveOptions configure RunLive.
type LiveOptions struct {
	Cfg    *config.Config
	Clock  clock.Clock
	Sys    sysapi.System // adapters, routes, Wi-Fi and change notifications
	Deps   Deps
	Out    io.Writer
	Logger *slog.Logger
}

// RunLive runs the real probes on the real scheduler: it polls the OS
// state (every min(rates.link, rates.wifi) seconds and on change notifications), builds the
// series for the primary adapter, runs the probes and the tracer, ticks
// the gates once per second and prints a timestamped line to Out whenever
// the verdict or the flags change (the plain format of SPEC §9). When the
// primary adapter changes, the gates are reset and the series are rebuilt
// for the new adapter. It returns the final snapshot when ctx ends.
func RunLive(ctx context.Context, o LiveOptions) (Snapshot, error) {
	eng := New(o.Cfg, o.Clock)
	if err := refreshSystem(ctx, eng, o.Sys); err != nil {
		return Snapshot{}, err
	}
	printPrimary(o.Out, eng.Primary())

	changed := make(chan struct{}, 1)
	var bg sync.WaitGroup
	bg.Add(2)
	go func() { defer bg.Done(); watchSystem(ctx, o, eng, changed) }()
	var last string
	printedEvents := 0
	report := func(prefix string) {
		snap := eng.Snapshot()
		for ; printedEvents < len(snap.AdapterEvents); printedEvents++ {
			ev := snap.AdapterEvents[printedEvents]
			fmt.Fprintf(o.Out, "%s  %s\n", ev.Time.Local().Format("15:04:05"), ev)
		}
		line := Describe(snap)
		if line != last || prefix != "" {
			fmt.Fprintf(o.Out, "%s  %s%s\n", snap.Time.Local().Format("15:04:05"), prefix, line)
			last = line
		}
	}
	go func() { // gate rounds and change reporting, once per second
		defer bg.Done()
		tk := o.Clock.NewTicker(time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C():
				eng.Tick()
				report("")
			}
		}
	}()

	for ctx.Err() == nil {
		runGeneration(ctx, o, eng, changed)
	}
	bg.Wait()
	eng.Tick()
	report("stopped: ")
	return eng.Snapshot(), nil
}

// runGeneration runs the series of the current primary adapter until ctx
// ends or the primary adapter changes.
func runGeneration(ctx context.Context, o LiveOptions, eng *Engine, changed <-chan struct{}) {
	primary := eng.Primary()
	gen := eng.Generation()
	specs, probes := Runnable(o.Cfg, primary, BuildSeries(o.Cfg, primary), o.Deps)
	eng.SetSeries(specs)
	series := make([]sched.Series, len(specs))
	for i, sp := range specs {
		series[i] = sched.Series{
			ID: sp.ID, Probe: sp.Probe, Target: sp.Target, Interval: sp.Interval, P: probes[i],
			Gate: func() sched.Decision { return eng.Decision(sp.ID) },
		}
	}
	sink := eng.SinkFor(gen)
	tracer := newTracer(o, eng, gen, sink)
	eng.SetTrace(tracer.Hops)

	genCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s := sched.New(o.Clock)
		s.Logger = o.Logger
		s.Run(genCtx, series, sink)
	}()
	go func() { defer wg.Done(); tracer.Run(genCtx) }()
	select {
	case <-ctx.Done():
	case <-changed:
	}
	cancel()
	wg.Wait()
}

// newTracer builds the TRC-01 tracer: hop series are added to the engine
// as they are found, and the engine's L3 gate decides when to probe.
func newTracer(o LiveOptions, eng *Engine, gen int, sink func(probe.Sample)) *probe.Tracer {
	cfg := o.Cfg
	t := &probe.Tracer{
		ICMP:     o.Deps.ICMP,
		Resolver: &probe.CachedResolver{Inner: o.Deps.Resolver, Clock: o.Clock},
		Host:     cfg.Targets.Trace,
		MaxHops:  cfg.Trace.MaxHops,
		Interval: ms(cfg.Trace.IntervalMs),
		Timeout:  ms(cfg.Timeouts.ICMP),
		Clock:    o.Clock,
		Gate: func() (bool, string) {
			d := eng.TraceDecision()
			return d.Action == sched.Run, d.Reason
		},
		Emit: sink,
		Register: func(id string, interval time.Duration) {
			ttl := 0
			_, _ = fmt.Sscanf(strings.TrimPrefix(id, "TRC-01:"), "%d", &ttl)
			s := Spec{Probe: "TRC-01", Target: fmt.Sprintf("hop %d", ttl)}
			s.ID, s.Interval = id, interval
			eng.AddSeriesFor(gen, s)
		},
	}
	switch {
	case o.Deps.Namer != nil:
		t.Namer = o.Deps.Namer
	case cfg.Trace.ReverseDNS:
		t.Namer = probe.NewNamer()
	}
	return t
}

// refreshSystem reads the OS state and applies it to the engine.
func refreshSystem(ctx context.Context, eng *Engine, sys sysapi.System) error {
	adapters, err := sys.Adapters.Adapters(ctx)
	if err != nil {
		return fmt.Errorf("list adapters: %w", err)
	}
	s := System{Adapters: adapters}
	s.Route, s.RouteErr = sys.Routes.BestRoute(ctx, RouteProbeTarget)
	if sys.WiFi != nil {
		if w, err := sys.WiFi.Status(ctx); err == nil {
			s.WiFi = w
		}
	}
	eng.SetSystem(s)
	return nil
}

// watchSystem refreshes the OS state every min(rates.link, rates.wifi) seconds and when the
// OS signals a change, and wakes the generation loop when the primary
// adapter changed.
func watchSystem(ctx context.Context, o LiveOptions, eng *Engine, changed chan<- struct{}) {
	var signal <-chan struct{}
	if o.Sys.Changes != nil {
		if ch, err := o.Sys.Changes.Watch(ctx); err == nil {
			signal = ch
		} else if o.Logger != nil {
			o.Logger.Warn("change notifications unavailable, polling", "err", err)
		}
	}
	tk := o.Clock.NewTicker(time.Duration(min(o.Cfg.Rates.Link, o.Cfg.Rates.WiFi)) * time.Second) // LINK-01 and WIFI-01
	defer tk.Stop()
	seen := eng.AdapterChanges()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C():
		case _, ok := <-signal:
			if !ok {
				signal = nil
				continue
			}
		}
		if err := refreshSystem(ctx, eng, o.Sys); err != nil {
			if o.Logger != nil && !errors.Is(err, context.Canceled) {
				o.Logger.Warn("refresh failed", "err", err)
			}
			continue
		}
		if n := eng.AdapterChanges(); n != seen {
			seen = n
			select {
			case changed <- struct{}{}:
			default:
			}
		}
	}
}

func printPrimary(out io.Writer, p sysapi.Primary) {
	switch {
	case !p.Found():
		fmt.Fprintln(out, "primary adapter: none")
	case p.OnLink:
		fmt.Fprintf(out, "primary adapter: %s (%s), on-link route\n", p.Adapter.Name, p.Reason)
	case p.Gateway.IsValid():
		fmt.Fprintf(out, "primary adapter: %s (%s), gateway %s\n", p.Adapter.Name, p.Reason, p.Gateway)
	default:
		fmt.Fprintf(out, "primary adapter: %s (%s), no default route\n", p.Adapter.Name, p.Reason)
	}
}

// Describe is the one-line state: verdict, detail, exit code and flags.
func Describe(s Snapshot) string {
	r := s.Result
	var b strings.Builder
	b.WriteString(string(r.Verdict))
	if r.Detail != "" {
		fmt.Fprintf(&b, " (%s)", r.Detail)
	}
	fmt.Fprintf(&b, " [exit %d]", r.ExitCode)
	if len(r.Flags) > 0 {
		b.WriteString("  flags: " + strings.Join(r.Flags, " "))
	}
	return b.String()
}
