package clock

import (
	"sync"
	"time"
)

// Fake is a manual clock: time moves only with Advance. Timers and tickers
// fire in deadline order during Advance.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

// NewFake returns a fake clock set to start.
func NewFake(start time.Time) *Fake { return &Fake{now: start} }

type fakeTimer struct {
	f        *Fake
	ch       chan time.Time
	deadline time.Time
	period   time.Duration // > 0 for tickers
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) NewTimer(d time.Duration) Timer { return f.add(d, 0) }

func (f *Fake) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: non-positive ticker period")
	}
	return fakeTicker{f.add(d, d)}
}

type fakeTicker struct{ t *fakeTimer }

func (k fakeTicker) C() <-chan time.Time { return k.t.ch }
func (k fakeTicker) Stop()               { k.t.Stop() }

func (f *Fake) add(d, period time.Duration) *fakeTimer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{f: f, ch: make(chan time.Time, 1), deadline: f.now.Add(d), period: period}
	if d <= 0 && period == 0 {
		t.ch <- f.now
		return t
	}
	f.timers = append(f.timers, t)
	return t
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	return t.f.remove(t)
}

// remove deletes t from the active list; f.mu must be held.
func (f *Fake) remove(t *fakeTimer) bool {
	for i, x := range f.timers {
		if x == t {
			f.timers = append(f.timers[:i], f.timers[i+1:]...)
			return true
		}
	}
	return false
}

// Advance moves time forward by d, firing every timer whose deadline is
// reached, earliest first, with Now equal to that deadline while firing.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	target := f.now.Add(d)
	for {
		var next *fakeTimer
		for _, t := range f.timers {
			if !t.deadline.After(target) && (next == nil || t.deadline.Before(next.deadline)) {
				next = t
			}
		}
		if next == nil {
			break
		}
		f.now = next.deadline
		select {
		case next.ch <- f.now:
		default: // receiver behind: drop, like time.Ticker
		}
		if next.period > 0 {
			next.deadline = next.deadline.Add(next.period)
		} else {
			f.remove(next)
		}
	}
	f.now = target
}

// Timers returns the number of active (not yet fired or stopped) timers
// and tickers.
func (f *Fake) Timers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

// WaitTimers waits (in real time, polling) until at least n timers are
// active, so a test knows the goroutines under test are parked on the
// clock before it advances. It reports false after timeout.
func (f *Fake) WaitTimers(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if f.Timers() >= n {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Microsecond)
	}
}
