package clock

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 25, 14, 0, 0, 0, time.UTC)

func fired(ch <-chan time.Time) (time.Time, bool) {
	select {
	case v := <-ch:
		return v, true
	default:
		return time.Time{}, false
	}
}

func TestFake_TimerFiresAtDeadline(t *testing.T) {
	t.Parallel()
	f := NewFake(t0)
	tm := f.NewTimer(2 * time.Second)
	f.Advance(1999 * time.Millisecond)
	if _, ok := fired(tm.C()); ok {
		t.Fatal("fired early")
	}
	f.Advance(time.Millisecond)
	if v, ok := fired(tm.C()); !ok || !v.Equal(t0.Add(2*time.Second)) {
		t.Fatalf("got %v, %v", v, ok)
	}
	if f.Timers() != 0 {
		t.Error("fired timer still active")
	}
}

func TestFake_StopPreventsFire(t *testing.T) {
	t.Parallel()
	f := NewFake(t0)
	tm := f.NewTimer(time.Second)
	if !tm.Stop() {
		t.Error("Stop on active timer = false")
	}
	f.Advance(2 * time.Second)
	if _, ok := fired(tm.C()); ok {
		t.Error("stopped timer fired")
	}
	if tm.Stop() {
		t.Error("second Stop = true")
	}
}

func TestFake_ZeroDurationFiresImmediately(t *testing.T) {
	t.Parallel()
	f := NewFake(t0)
	if _, ok := fired(f.NewTimer(0).C()); !ok {
		t.Error("zero timer did not fire")
	}
}

func TestFake_TickerAndOrder(t *testing.T) {
	t.Parallel()
	f := NewFake(t0)
	tk := f.NewTicker(time.Second)
	tm := f.NewTimer(1500 * time.Millisecond)
	f.Advance(time.Second)
	if v, ok := fired(tk.C()); !ok || !v.Equal(t0.Add(time.Second)) {
		t.Fatalf("tick 1: %v %v", v, ok)
	}
	f.Advance(time.Second) // timer at 1.5 s, tick at 2 s
	if v, ok := fired(tm.C()); !ok || !v.Equal(t0.Add(1500*time.Millisecond)) {
		t.Errorf("timer: %v %v", v, ok)
	}
	if v, ok := fired(tk.C()); !ok || !v.Equal(t0.Add(2*time.Second)) {
		t.Errorf("tick 2: %v %v", v, ok)
	}
	f.Advance(3 * time.Second) // receiver idle: ticks dropped, one buffered
	if _, ok := fired(tk.C()); !ok {
		t.Error("no buffered tick")
	}
	if _, ok := fired(tk.C()); ok {
		t.Error("ticks must not queue up")
	}
	tk.Stop()
	if f.Timers() != 0 {
		t.Error("ticker still active after Stop")
	}
	if !f.Now().Equal(t0.Add(5 * time.Second)) {
		t.Errorf("Now = %v", f.Now())
	}
}

func TestFake_WaitTimers(t *testing.T) {
	t.Parallel()
	f := NewFake(t0)
	if f.WaitTimers(1, 5*time.Millisecond) {
		t.Error("no timer yet")
	}
	go f.NewTimer(time.Second)
	if !f.WaitTimers(1, time.Second) {
		t.Error("timer not seen")
	}
}

func TestReal(t *testing.T) {
	t.Parallel()
	var c Clock = Real{}
	tm := c.NewTimer(time.Millisecond)
	<-tm.C()
	tk := c.NewTicker(time.Millisecond)
	<-tk.C()
	tk.Stop()
	if c.Now().IsZero() {
		t.Error("zero time")
	}
}
