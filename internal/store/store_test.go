package store

import (
	"sync"
	"testing"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/probe"
)

var t0 = time.Date(2026, 10, 25, 14, 0, 0, 0, time.UTC)

func ok(id string, at time.Time, rttMs int) probe.Sample {
	return probe.Sample{Series: id, Time: at, Result: probe.OK, RTT: time.Duration(rttMs) * time.Millisecond}
}

func TestRing_Wraparound(t *testing.T) {
	t.Parallel()
	r := newRing(3)
	for i := 1; i <= 5; i++ {
		r.push(probe.Sample{Value: float64(i)})
	}
	got := r.items()
	if len(got) != 3 || got[0].Value != 3 || got[1].Value != 4 || got[2].Value != 5 {
		t.Errorf("items after wraparound = %v, want values 3,4,5 oldest first", got)
	}
	r.clear()
	if len(r.items()) != 0 {
		t.Error("clear")
	}
	r.push(probe.Sample{Value: 9})
	if got := r.items(); len(got) != 1 || got[0].Value != 9 {
		t.Errorf("after clear: %v", got)
	}
}

func TestCapacity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		history, interval time.Duration
		want              int
	}{
		{15 * time.Minute, time.Second, 901},
		{15 * time.Minute, 5 * time.Second, 181},
		{15 * time.Minute, 200 * time.Millisecond, 4501},
		{15 * time.Minute, 0, 901},
		{1000 * time.Hour, time.Millisecond, maxCapacity},
	}
	for _, tt := range tests {
		if got := Capacity(tt.history, tt.interval); got != tt.want {
			t.Errorf("Capacity(%v, %v) = %d, want %d", tt.history, tt.interval, got, tt.want)
		}
	}
}

func TestStore_WindowExpiryWithFakeClock(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(t0)
	s := New(c, 10*time.Second, time.Minute)
	s.Register("NET-01:1.1.1.1", time.Second)
	for i := range 15 {
		s.Add(ok("NET-01:1.1.1.1", c.Now(), 10+i))
		c.Advance(time.Second)
	}
	// now = t0+15s; samples at t0+0..14 s. Window (now−10s, now] holds t0+6..14 s.
	w := s.Window("NET-01:1.1.1.1")
	if len(w) != 9 || !w[0].Time.Equal(t0.Add(6*time.Second)) {
		t.Fatalf("window has %d samples starting %v", len(w), w[0].Time)
	}
	if h := s.History("NET-01:1.1.1.1"); len(h) != 15 {
		t.Errorf("history has %d samples, want all 15", len(h))
	}
	st := s.Stats("NET-01:1.1.1.1")
	if st.Sent != 9 || st.Best != 16 || st.Worst != 24 || st.Last != 24 {
		t.Errorf("window stats %+v", st)
	}

	c.Advance(10 * time.Second) // everything leaves the window
	if n := len(s.Window("NET-01:1.1.1.1")); n != 0 {
		t.Errorf("window after 10 s idle has %d samples", n)
	}
	c.Advance(time.Hour) // and the history
	if n := len(s.History("NET-01:1.1.1.1")); n != 0 {
		t.Errorf("history after an hour has %d samples", n)
	}
}

func TestStore_SampleExactlyWindowOldHasLeft(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(t0)
	s := New(c, 10*time.Second, time.Minute)
	s.Add(ok("x", t0, 1))
	c.Advance(10 * time.Second)
	if n := len(s.Window("x")); n != 0 {
		t.Errorf("sample exactly 10 s old is still in the window (%d)", n)
	}
}

func TestStore_RingCapacityBoundsHistory(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(t0)
	s := New(c, time.Minute, 10*time.Second) // capacity 11 at 1 s
	s.Register("x", time.Second)
	for i := range 30 {
		s.Add(ok("x", c.Now(), i))
		c.Advance(100 * time.Millisecond) // faster than registered: ring bound wins
	}
	h := s.History("x")
	if len(h) != 11 || h[len(h)-1].RTTms() != 29 {
		t.Errorf("history %d samples, newest %v", len(h), h[len(h)-1].RTTms())
	}
}

func TestStore_ResetClearsAllSeries(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(t0)
	s := New(c, time.Minute, time.Minute)
	s.Add(ok("a", t0, 1))
	s.Add(ok("b", t0, 2))
	s.Reset()
	if len(s.Window("a"))+len(s.Window("b")) != 0 {
		t.Error("samples left after reset")
	}
	if ids := s.IDs(); len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Errorf("series must stay registered: %v", ids)
	}
	s.Add(ok("a", t0, 3))
	if st := s.Stats("a"); st.Sent != 1 || st.Last != 3 {
		t.Errorf("after reset %+v", st)
	}
}

func TestStore_UnknownSeries(t *testing.T) {
	t.Parallel()
	s := New(clock.NewFake(t0), time.Minute, time.Minute)
	if s.Window("nope") != nil || s.Stats("nope").Sent != 0 {
		t.Error("unknown series must be empty")
	}
}

func TestStore_ConcurrentAdd(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(t0)
	s := New(c, time.Minute, time.Minute)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				s.Add(ok(string(rune('a'+g)), t0, 1))
				_ = s.Stats(string(rune('a' + g)))
			}
		}()
	}
	wg.Wait()
	if len(s.IDs()) != 8 {
		t.Errorf("series %v", s.IDs())
	}
}

func TestStore_ResetSeriesAndRetain(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s := New(c, time.Minute, 15*time.Minute)
	for _, id := range []string{"A", "B", "C"} {
		s.Register(id, time.Second)
		s.Add(probe.Sample{Series: id, Time: c.Now(), Result: probe.OK})
	}
	s.ResetSeries([]string{"A", "nope"})
	if len(s.History("A")) != 0 || len(s.History("B")) != 1 || len(s.History("C")) != 1 {
		t.Errorf("only A is cleared: %d %d %d", len(s.History("A")), len(s.History("B")), len(s.History("C")))
	}
	s.Add(probe.Sample{Series: "A", Time: c.Now(), Result: probe.OK})
	if len(s.History("A")) != 1 {
		t.Error("a cleared series keeps recording")
	}
	s.Retain([]string{"A", "C"})
	if ids := s.IDs(); len(ids) != 2 || ids[0] != "A" || ids[1] != "C" {
		t.Errorf("ids %v, want A and C", ids)
	}
	if len(s.History("B")) != 0 {
		t.Error("B was removed")
	}
}
