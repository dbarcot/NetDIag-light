package sched

import (
	"context"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/probe"
)

var t0 = time.Date(2026, 10, 25, 14, 0, 0, 0, time.UTC)

// collector is a concurrency-safe sink.
type collector struct {
	mu      sync.Mutex
	samples []probe.Sample
}

func (c *collector) add(s probe.Sample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples = append(c.samples, s)
}

func (c *collector) all() []probe.Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]probe.Sample(nil), c.samples...)
}

func okProbe() probe.Probe {
	return probe.Func(func(context.Context) probe.Sample { return probe.Sample{Result: probe.OK, RTT: time.Millisecond} })
}

// harness runs the scheduler on a fake clock and steps time so that every
// series goroutine is parked on its timer before each advance.
type harness struct {
	t      *testing.T
	clock  *clock.Fake
	sink   *collector
	cancel context.CancelFunc
	done   chan struct{}
	timers int
}

func start(t *testing.T, series []Series, offset time.Duration) *harness {
	t.Helper()
	h := &harness{t: t, clock: clock.NewFake(t0), sink: &collector{}, done: make(chan struct{}), timers: len(series)}
	s := New(h.clock)
	s.Offset = func(time.Duration) time.Duration { return offset }
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		s.Run(ctx, series, h.sink.add)
		close(h.done)
	}()
	h.waitParked()
	return h
}

func (h *harness) waitParked() {
	h.t.Helper()
	if !h.clock.WaitTimers(h.timers, 2*time.Second) {
		h.t.Fatalf("series goroutines not parked on the clock (%d of %d timers)", h.clock.Timers(), h.timers)
	}
}

// step advances the fake clock in increments, letting the series react
// after each one.
func (h *harness) step(total, inc time.Duration) {
	h.t.Helper()
	for elapsed := time.Duration(0); elapsed < total; elapsed += inc {
		h.clock.Advance(inc)
		h.waitParked()
	}
}

// waitFor polls cond in real time (goroutine hand-off, not fake time).
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(200 * time.Microsecond)
	}
}

func (h *harness) stop() {
	h.t.Helper()
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		h.t.Fatal("Run did not return after cancel")
	}
}

func TestScheduler_RunsOncePerIntervalOnGrid(t *testing.T) {
	t.Parallel()
	h := start(t, []Series{
		{ID: "NET-01:1.1.1.1", Probe: "NET-01", Target: "1.1.1.1", Interval: time.Second, P: okProbe()},
		{ID: "DNS-02:8.8.8.8", Probe: "DNS-02", Target: "8.8.8.8", Interval: 5 * time.Second, P: okProbe()},
	}, 300*time.Millisecond)
	h.step(20*time.Second, 100*time.Millisecond)
	h.stop()

	var net, dns []probe.Sample
	for _, s := range h.sink.all() {
		switch s.Series {
		case "NET-01:1.1.1.1":
			net = append(net, s)
		case "DNS-02:8.8.8.8":
			dns = append(dns, s)
		}
	}
	// runs at 0.3, 1.3, …, 19.3 s → 20; DNS at 0.3, 5.3, 10.3, 15.3 → 4
	if len(net) != 20 || len(dns) != 4 {
		t.Fatalf("runs: NET-01 %d (want 20), DNS-02 %d (want 4)", len(net), len(dns))
	}
	for i, s := range net {
		want := t0.Add(300*time.Millisecond + time.Duration(i)*time.Second)
		if !s.Time.Equal(want) {
			t.Errorf("run %d at %v, want %v", i, s.Time.Sub(t0), want.Sub(t0))
		}
		if s.Probe != "NET-01" || s.Target != "1.1.1.1" || s.Result != probe.OK {
			t.Errorf("sample not stamped: %+v", s)
		}
	}
}

func TestScheduler_SlowProbeNeverOverlapsAndDropsMissedTicks(t *testing.T) {
	t.Parallel()
	var inFlight, maxInFlight, runs atomic.Int32
	release := make(chan struct{})
	slow := probe.Func(func(context.Context) probe.Sample {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		if runs.Add(1) == 2 {
			<-release // second run takes 3.5 s of fake time
		}
		inFlight.Add(-1)
		return probe.Sample{Result: probe.OK}
	})
	// offset 0: run 1 happens at 0 s during start
	h := start(t, []Series{{ID: "s", Interval: time.Second, P: slow}}, 0)

	h.clock.Advance(time.Second) // run 2 starts at 1 s and blocks
	waitFor(t, func() bool { return runs.Load() == 2 })
	for range 35 { // 3.5 s pass while run 2 is still in flight
		h.clock.Advance(100 * time.Millisecond)
	}
	if h.clock.Timers() != 0 {
		t.Fatal("a second run was scheduled while one is in flight")
	}
	close(release) // run 2 ends at 4.5 s; slots 2, 3, 4 s are dropped, next is 5 s
	h.waitParked()
	h.step(2*time.Second, 100*time.Millisecond) // until 6.5 s: runs at 5 s and 6 s
	h.stop()

	if maxInFlight.Load() != 1 {
		t.Errorf("max in flight = %d, want 1", maxInFlight.Load())
	}
	var times []time.Duration
	for _, s := range h.sink.all() {
		times = append(times, s.Time.Sub(t0))
	}
	want := []time.Duration{0, time.Second, 5 * time.Second, 6 * time.Second}
	if len(times) != len(want) {
		t.Fatalf("run times %v, want %v", times, want)
	}
	for i := range want {
		if times[i] != want[i] {
			t.Errorf("run times %v, want %v", times, want)
			break
		}
	}
}

// A series whose probe hangs must not delay any other series. Run with
// -race: the sink is called from both series goroutines.
func TestScheduler_SeriesAreIndependent(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var aStarted atomic.Bool
	blocking := probe.Func(func(context.Context) probe.Sample {
		aStarted.Store(true)
		<-release
		return probe.Sample{Result: probe.OK}
	})
	h := start(t, []Series{
		{ID: "A", Interval: time.Second, P: blocking},
		{ID: "B", Interval: time.Second, P: okProbe()},
	}, 500*time.Millisecond)

	h.clock.Advance(500 * time.Millisecond) // both fire at 0.5 s; A blocks
	waitFor(t, aStarted.Load)
	h.timers = 1 // only B parks from now on
	h.waitParked()
	h.step(5*time.Second, time.Second) // B at 1.5 … 5.5 s while A hangs

	var bTimes []time.Duration
	for _, s := range h.sink.all() {
		if s.Series == "A" {
			t.Fatalf("A emitted while its probe was still running: %+v", s)
		}
		bTimes = append(bTimes, s.Time.Sub(t0))
	}
	if len(bTimes) != 6 {
		t.Fatalf("B emitted %d samples during A's 5 s hang, want 6 (0.5 … 5.5 s): %v", len(bTimes), bTimes)
	}
	for i, d := range bTimes {
		if want := 500*time.Millisecond + time.Duration(i)*time.Second; d != want {
			t.Errorf("B sample %d at %v, want %v", i, d, want)
		}
	}

	close(release)
	waitFor(t, func() bool {
		for _, s := range h.sink.all() {
			if s.Series == "A" {
				return s.Time.Equal(t0.Add(500 * time.Millisecond)) // stamped with its start time
			}
		}
		return false
	})
	h.stop()
}

// lockedBuffer is an io.Writer safe for the logger in concurrent tests.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestScheduler_ProbePanicBecomesLocalErrorAndSeriesContinues(t *testing.T) {
	t.Parallel()
	var runs atomic.Int32
	p := probe.Func(func(context.Context) probe.Sample {
		if runs.Add(1) == 1 {
			panic("boom")
		}
		return probe.Sample{Result: probe.OK}
	})
	logs := &lockedBuffer{}
	h := &harness{t: t, clock: clock.NewFake(t0), sink: &collector{}, done: make(chan struct{}), timers: 1}
	s := New(h.clock)
	s.Offset = func(time.Duration) time.Duration { return 500 * time.Millisecond }
	s.Logger = slog.New(slog.NewTextHandler(logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { s.Run(ctx, []Series{{ID: "x", Interval: time.Second, P: p}}, h.sink.add); close(h.done) }()
	h.waitParked()
	h.step(3*time.Second, time.Second)
	h.stop()

	got := h.sink.all()
	if len(got) != 3 {
		t.Fatalf("got %d samples, want 3 (series must keep running): %+v", len(got), got)
	}
	if got[0].Result != probe.Fail || got[0].ErrClass != probe.ClassLocal || got[0].Detail != "panic: boom" {
		t.Errorf("panic sample %+v", got[0])
	}
	if got[1].Result != probe.OK || got[2].Result != probe.OK {
		t.Errorf("later samples %+v", got[1:])
	}
	if l := logs.String(); !strings.Contains(l, "probe panic") || !strings.Contains(l, "series=x") || !strings.Contains(l, "sched.(*Scheduler).runProbe") {
		t.Errorf("panic not logged with stack: %q", l)
	}
}

func TestScheduler_PanicWithoutLogger(t *testing.T) {
	t.Parallel()
	s := New(clock.NewFake(t0))
	sm := s.runProbe(context.Background(), Series{ID: "x", P: probe.Func(func(context.Context) probe.Sample { panic(42) })})
	if sm.Result != probe.Fail || sm.Detail != "panic: 42" {
		t.Errorf("got %+v", sm)
	}
}

func TestScheduler_GateSkipAndWait(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	decision := Decision{Action: Run}
	setGate := func(d Decision) { mu.Lock(); decision = d; mu.Unlock() }
	var probeRuns atomic.Int32
	p := probe.Func(func(context.Context) probe.Sample {
		probeRuns.Add(1)
		return probe.Sample{Result: probe.OK}
	})
	setGate(Decision{Action: Wait})
	h := start(t, []Series{{ID: "NET-01:8.8.8.8", Interval: time.Second, P: p,
		Gate: func() Decision { mu.Lock(); defer mu.Unlock(); return decision }}}, 500*time.Millisecond)

	h.step(3*time.Second, time.Second) // ticks at 0.5, 1.5, 2.5 s: nothing recorded
	setGate(Decision{Action: Skip, Reason: "gateway down"})
	h.step(2*time.Second, time.Second) // 3.5, 4.5 s: skipped
	setGate(Decision{Action: Run})
	h.step(2*time.Second, time.Second) // 5.5, 6.5 s: run
	h.stop()

	got := h.sink.all()
	if len(got) != 4 {
		t.Fatalf("got %d samples, want 2 skipped + 2 ok: %+v", len(got), got)
	}
	for _, s := range got[:2] {
		if s.Result != probe.Skipped || s.Detail != "skipped: gateway down" || s.Series != "NET-01:8.8.8.8" {
			t.Errorf("skipped sample %+v", s)
		}
	}
	if got[2].Result != probe.OK || probeRuns.Load() != 2 {
		t.Errorf("probe ran %d times; samples %+v", probeRuns.Load(), got[2:])
	}
}

func TestScheduler_CancelStopsAllGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	var series []Series
	for i := range 10 {
		series = append(series, Series{ID: string(rune('a' + i)), Interval: time.Second, P: okProbe()})
	}
	h := start(t, series, 0)
	h.step(3*time.Second, time.Second)
	h.stop()
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Errorf("goroutine leak: %d before, %d after", before, n)
	}
}

func TestScheduler_CancelDuringRunEmitsNothing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	c := clock.NewFake(t0)
	s := New(c)
	s.Offset = func(time.Duration) time.Duration { return 0 }
	sink := &collector{}
	p := probe.Func(func(ctx context.Context) probe.Sample {
		cancel()
		<-ctx.Done()
		return probe.Sample{Result: probe.Fail, ErrClass: probe.ClassTimeout}
	})
	done := make(chan struct{})
	go func() { s.Run(ctx, []Series{{ID: "x", Interval: time.Second, P: p}}, sink.add); close(done) }()
	<-done // offset 0: the first tick is due at start, the probe cancels
	if n := len(sink.all()); n != 0 {
		t.Errorf("cancelled run produced %d samples (a shutdown is not a failure)", n)
	}
}

func TestNextSlot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		prev, now, want time.Duration
	}{
		{0, 500 * time.Millisecond, time.Second},      // on time
		{0, time.Second, 2 * time.Second},             // finished exactly at the next slot: it is taken as missed
		{0, 3500 * time.Millisecond, 4 * time.Second}, // 3 slots missed
		{2 * time.Second, 2 * time.Second, 3 * time.Second},
	}
	for _, tt := range tests {
		got := nextSlot(t0.Add(tt.prev), time.Second, t0.Add(tt.now))
		if !got.Equal(t0.Add(tt.want)) {
			t.Errorf("nextSlot(%v, now %v) = %v, want %v", tt.prev, tt.now, got.Sub(t0), tt.want)
		}
	}
}

func TestRandomOffset(t *testing.T) {
	t.Parallel()
	for _, iv := range []time.Duration{200 * time.Millisecond, time.Second, time.Minute} {
		limit := min(iv, MaxStartOffset)
		for range 200 {
			if o := RandomOffset(iv); o < 0 || o >= limit {
				t.Fatalf("RandomOffset(%v) = %v, want [0, %v)", iv, o, limit)
			}
		}
	}
	if RandomOffset(0) != 0 {
		t.Error("zero interval")
	}
}
