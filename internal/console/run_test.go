package console

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/config"
	"netdiag/internal/engine"
	"netdiag/internal/store"
	"netdiag/internal/sysapi"
	"netdiag/internal/verdict"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// stubSource returns scripted results; snapshot n (from 0) uses result(n).
type stubSource struct {
	c      clock.Clock
	st     *store.Store
	result func(n int) verdict.Result
	panics bool

	mu     sync.Mutex
	calls  int
	resets int
}

func newStub(c clock.Clock) *stubSource {
	return &stubSource{c: c, st: store.New(c, time.Minute, time.Minute), result: func(int) verdict.Result {
		return verdict.Result{Verdict: verdict.OK, Quality: verdict.CallQuality{Status: verdict.QualityMeasuring}}
	}}
}

func (s *stubSource) Snapshot() engine.Snapshot {
	now := s.c.Now() // before the call is counted: the test advances the clock after it
	s.mu.Lock()
	n := s.calls
	s.calls++
	s.mu.Unlock()
	if s.panics {
		panic("render boom")
	}
	return engine.Snapshot{Time: now, Result: s.result(n)}
}

func (s *stubSource) Store() *store.Store { return s.st }

func (s *stubSource) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resets++
}

func (s *stubSource) count() (calls, resets int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.resets
}

// syncBuffer is a bytes.Buffer safe for the render goroutine and the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func noEnv(string) (string, bool) { return "", false }

func options(c clock.Clock, con sysapi.Console, out *syncBuffer) Options {
	return Options{
		Console: con, Stdout: out, Clock: c, LookupEnv: noEnv, Config: config.Default(),
		Meta: Meta{Version: "1.2.3", ConfigSource: "test"}, Location: time.UTC,
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// blockWork runs until its context ends.
func blockWork(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Plain mode prints only verdict/flag changes plus a summary every 30 s,
// and a final summary.
func TestRun_PlainPrintsChangesAndPeriodicSummary(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(t0)
	src := newStub(c)
	src.result = func(n int) verdict.Result {
		r := verdict.Result{Verdict: verdict.Checking, ExitCode: 1}
		if n >= 2 {
			r = verdict.Result{Verdict: verdict.OK}
		}
		if n >= 40 {
			r.Flags = []string{verdict.FlagCPUHigh}
		}
		r.Quality.Status = verdict.QualityMeasuring
		return r
	}
	con := &sysapi.FakeConsole{NoIn: true, NoOut: true} // redirected
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, options(c, con, out), src, blockWork) }()

	if !c.WaitTimers(1, 5*time.Second) {
		t.Fatal("render loop did not start its ticker")
	}
	for i := 1; i <= 65; i++ {
		c.Advance(time.Second)
		waitUntil(t, "a snapshot per tick", func() bool { n, _ := src.count(); return n >= i })
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var top []string
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if !strings.HasPrefix(l, "    ") {
			top = append(top, l)
		}
	}
	want := []string{
		"12:00:01  CHECKING [exit 1]",
		"12:00:03  OK [exit 0]",
		"12:00:30  summary: OK [exit 0]",
		"12:00:41  OK [exit 0]  flags: CPU_HIGH",
		"12:01:00  summary: OK [exit 0]  flags: CPU_HIGH",
		"12:01:05  final: OK [exit 0]  flags: CPU_HIGH",
	}
	if strings.Join(top, "\n") != strings.Join(want, "\n") {
		t.Errorf("plain output:\n%s\nwant:\n%s", strings.Join(top, "\n"), strings.Join(want, "\n"))
	}
	text := out.String()
	if strings.Contains(text, "\x1b") || strings.ContainsAny(text, "▁▂▃▄▅▆▇█") {
		t.Errorf("plain output has escapes or sparklines:\n%s", text)
	}
	if !strings.Contains(text, "    Call quality: Measuring…\n") || !strings.Contains(text, "    Name ") {
		t.Errorf("summary block lacks call quality or table:\n%s", text)
	}
	if con.Output() != "" {
		t.Error("redirected run wrote to the console")
	}
}

// startTTY runs the console view on a fake console with key input.
func startTTY(t *testing.T, src *stubSource, con *sysapi.FakeConsole, work func(context.Context) error) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, options(src.c, con, &syncBuffer{}), src, work) }()
	return cancel, done
}

const (
	origIn  = 0x1f7 // processed, line, echo, quick edit, extended…
	origOut = 0x3
)

func ttyConsole() *sysapi.FakeConsole {
	return &sysapi.FakeConsole{In: origIn, Out: origOut, Cols: 120, Keys: make(chan sysapi.Key, 4)}
}

func TestRun_KeysResetAndQuit(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(t0)
	src := newStub(c)
	con := ttyConsole()
	workCtx := make(chan context.Context, 1)
	_, done := startTTY(t, src, con, func(ctx context.Context) error {
		workCtx <- ctx
		<-ctx.Done()
		return nil
	})
	waitUntil(t, "first frame", func() bool { n, _ := src.count(); return n >= 1 })
	con.Keys <- sysapi.Key{Rune: 'r'}
	waitUntil(t, "reset", func() bool { _, r := src.count(); return r == 1 })
	waitUntil(t, "redraw after reset", func() bool { n, _ := src.count(); return n >= 2 })
	con.Keys <- sysapi.Key{Rune: 'Q'}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("q did not stop the run")
	}
	if ctx := <-workCtx; ctx.Err() == nil {
		t.Error("q did not cancel the work context")
	}
	out := con.Output()
	if !strings.HasPrefix(out, vtHideCursor+vtClear+vtHome) || strings.Count(out, vtClear) != 1 {
		t.Errorf("frames must start at home without clearing the screen each time: %q", out[:min(len(out), 40)])
	}
	if !strings.Contains(out, "q quit  r reset counters") {
		t.Error("no key hint")
	}
	if i := strings.LastIndex(out, vtShowCursor); i < 0 || !strings.Contains(out[i:], "final: OK") {
		t.Error("cursor not shown before the final summary")
	}
}

// The original console modes come back on every exit path, and processed
// input (Ctrl+C) is never switched off.
func TestRun_RestoresConsoleModes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		work    func(ctx context.Context) error
		trigger func(cancel context.CancelFunc, con *sysapi.FakeConsole)
		panics  bool
		wantErr string
	}{
		{name: "q", work: blockWork, trigger: func(_ context.CancelFunc, con *sysapi.FakeConsole) { con.Keys <- sysapi.Key{Rune: 'q'} }},
		{name: "ctrl+c", work: blockWork, trigger: func(cancel context.CancelFunc, _ *sysapi.FakeConsole) { cancel() }},
		{name: "fatal error", work: func(context.Context) error { return errors.New("list adapters: boom") }, wantErr: "boom"},
		{name: "panic in render loop", work: blockWork, panics: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := clock.NewFake(t0)
			src := newStub(c)
			src.panics = tt.panics
			con := ttyConsole()
			var workCtx context.Context
			var mu sync.Mutex
			work := func(ctx context.Context) error {
				mu.Lock()
				workCtx = ctx
				mu.Unlock()
				return tt.work(ctx)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan any, 1)
			go func() {
				defer func() { done <- recover() }()
				if err := Run(ctx, options(c, con, &syncBuffer{}), src, work); err != nil {
					done <- err
				}
			}()
			if tt.trigger != nil {
				waitUntil(t, "first frame", func() bool { n, _ := src.count(); return n >= 1 })
				tt.trigger(cancel, con)
			}
			var got any
			select {
			case got = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return")
			}
			switch {
			case tt.panics:
				if got != "render boom" {
					t.Errorf("recovered %v, want the panic re-raised", got)
				}
				mu.Lock()
				if workCtx != nil && workCtx.Err() == nil {
					t.Error("work not cancelled after the panic")
				}
				mu.Unlock()
			case tt.wantErr != "":
				if err, _ := got.(error); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("got %v, want error %q", got, tt.wantErr)
				}
				if strings.Contains(con.Output(), "final:") {
					t.Error("final summary after a fatal error")
				}
			default:
				if got != nil {
					t.Errorf("got %v", got)
				}
			}
			if in, _ := con.InMode(); in != origIn {
				t.Errorf("stdin mode %#x, want %#x restored", in, origIn)
			}
			if out, _ := con.OutMode(); out != origOut {
				t.Errorf("stdout mode %#x, want %#x restored", out, origOut)
			}
			changes := con.ModeChanges()
			if len(changes) != 4 {
				t.Errorf("mode changes %+v, want set + restore for both", changes)
			}
			for _, ch := range changes {
				if ch.Stream == "in" && ch.Mode&sysapi.ModeProcessedInput == 0 {
					t.Errorf("ENABLE_PROCESSED_INPUT cleared: %#x", ch.Mode)
				}
			}
			if !strings.Contains(con.Output(), vtShowCursor) {
				t.Error("cursor not shown again")
			}
		})
	}
}

// VT unavailable on a real console: plain output through the console
// writer; redirected stdin only disables keys.
func TestRun_PlainWhenVTFails(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(t0)
	src := newStub(c)
	con := &sysapi.FakeConsole{In: origIn, Out: origOut, FailSetOut: true, Cols: 120, Keys: make(chan sysapi.Key, 1)}
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, options(c, con, out), src, blockWork) }()
	if !c.WaitTimers(1, 5*time.Second) {
		t.Fatal("no ticker")
	}
	c.Advance(time.Second)
	waitUntil(t, "a tick", func() bool { n, _ := src.count(); return n >= 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	text := con.Output()
	if strings.Contains(text, "\x1b") || !strings.Contains(text, "12:00:01  OK [exit 0]") {
		t.Errorf("console output %q", text)
	}
	if out.String() != "" {
		t.Error("a console stdout must not get the redirected writer")
	}
}

func TestRun_NoColor(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(t0)
	src := newStub(c)
	con := ttyConsole()
	o := options(c, con, &syncBuffer{})
	o.LookupEnv = func(k string) (string, bool) { return "", k == "NO_COLOR" } // set, even empty
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, o, src, blockWork) }()
	waitUntil(t, "first frame", func() bool { n, _ := src.count(); return n >= 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	out := con.Output()
	if strings.Contains(out, sgrReset) || !strings.Contains(out, vtHome) {
		t.Errorf("NO_COLOR: want VT positioning without colour: %q", out[:min(len(out), 80)])
	}
}

// A frame taller than the window is cut so that cursor-home redraws never
// scroll the console.
func TestRun_FrameFitsWindowHeight(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(t0)
	src := newStub(c)
	con := ttyConsole()
	con.Rows = 6
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, options(c, con, &syncBuffer{}), src, blockWork) }()
	waitUntil(t, "first frame", func() bool { n, _ := src.count(); return n >= 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	out := con.Output()
	frame := out[strings.Index(out, vtHome):strings.Index(out, vtEraseBelow)]
	if n := strings.Count(frame, "\r\n") + 1; n != 6 {
		t.Errorf("frame has %d rows, want the window height 6:\n%s", n, frame)
	}
	if !strings.Contains(frame, "more lines: enlarge the window") {
		t.Errorf("no hint about hidden rows:\n%s", frame)
	}
}

func TestFitHeight(t *testing.T) {
	t.Parallel()
	ls := []line{plain("a"), plain("b"), plain("c")}
	if got := fitHeight(ls, 3, 80); len(got) != 3 {
		t.Errorf("fits: %d lines", len(got))
	}
	got := fitHeight(ls, 2, 80)
	if len(got) != 2 || got[1].String(false) != "… 2 more lines: enlarge the window" {
		t.Errorf("cut: %q", got)
	}
	if len(ls) != 3 || ls[1].String(false) != "b" {
		t.Error("input modified")
	}
}
