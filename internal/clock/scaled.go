package clock

import (
	"sync"
	"time"
)

// Scaled is a real-time clock running factor times faster than the system
// clock. It lets tests and demos run real-time code paths (goroutines,
// timers) in a fraction of the wall time.
type Scaled struct {
	factor float64
	start  time.Time // system clock at creation (monotonic)
	base   time.Time // scaled time at creation
}

// NewScaled returns a clock that starts at the current time and runs
// factor times faster.
func NewScaled(factor float64) *Scaled {
	now := time.Now()
	return &Scaled{factor: factor, start: now, base: now}
}

func (s *Scaled) Now() time.Time {
	return s.base.Add(time.Duration(float64(time.Since(s.start)) * s.factor))
}

func (s *Scaled) real(d time.Duration) time.Duration { return time.Duration(float64(d) / s.factor) }

func (s *Scaled) NewTimer(d time.Duration) Timer {
	t := &scaledTimer{ch: make(chan time.Time, 1)}
	t.t = time.AfterFunc(s.real(d), func() { t.ch <- s.Now() })
	return t
}

func (s *Scaled) NewTicker(d time.Duration) Ticker {
	k := &scaledTicker{ch: make(chan time.Time, 1), done: make(chan struct{}), t: time.NewTicker(s.real(d))}
	go func() {
		for {
			select {
			case <-k.done:
				return
			case <-k.t.C:
				select {
				case k.ch <- s.Now():
				default: // receiver behind: drop, like time.Ticker
				}
			}
		}
	}()
	return k
}

type scaledTimer struct {
	t  *time.Timer
	ch chan time.Time
}

func (t *scaledTimer) C() <-chan time.Time { return t.ch }
func (t *scaledTimer) Stop() bool          { return t.t.Stop() }

type scaledTicker struct {
	t    *time.Ticker
	ch   chan time.Time
	done chan struct{}
	once sync.Once
}

func (k *scaledTicker) C() <-chan time.Time { return k.ch }

func (k *scaledTicker) Stop() {
	k.once.Do(func() {
		k.t.Stop()
		close(k.done)
	})
}
