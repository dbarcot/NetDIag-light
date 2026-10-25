// Package store keeps per-series ring buffers of samples (graph history)
// and computes rolling-window statistics over them.
package store

import (
	"sort"
	"sync"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/probe"
	"netdiag/internal/stats"
)

// maxCapacity bounds one ring regardless of interval (memory bound, SPEC §12).
const maxCapacity = 100_000

// Store holds samples per series. It is safe for concurrent use.
type Store struct {
	clock   clock.Clock
	window  time.Duration
	history time.Duration

	mu     sync.Mutex
	series map[string]*ring
}

// New returns a store with the rolling-window length (window.seconds) and
// graph history length (history.minutes).
func New(c clock.Clock, window, history time.Duration) *Store {
	return &Store{clock: c, window: window, history: history, series: map[string]*ring{}}
}

// Capacity is the ring size for a series sampled every interval: enough
// for the whole history plus one.
func Capacity(history, interval time.Duration) int {
	if interval <= 0 {
		interval = time.Second
	}
	return min(int(history/interval)+1, maxCapacity)
}

// Register creates the ring for a series. Adding to an unregistered series
// registers it with a 1 s interval.
func (s *Store) Register(id string, interval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.series[id]; !ok {
		s.series[id] = newRing(Capacity(s.history, interval))
	}
}

// Add appends a sample to its series (sample.Series).
func (s *Store) Add(sm probe.Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.series[sm.Series]
	if !ok {
		r = newRing(Capacity(s.history, time.Second))
		s.series[sm.Series] = r
	}
	r.push(sm)
}

// History returns the series' samples within the history length, oldest
// first.
func (s *Store) History(id string) []probe.Sample {
	return s.since(id, s.history)
}

// Window returns the series' samples within the rolling window, oldest first.
func (s *Store) Window(id string) []probe.Sample {
	return s.since(id, s.window)
}

// Stats summarizes the rolling window of a series.
func (s *Store) Stats(id string) stats.Summary {
	return stats.Summarize(s.Window(id))
}

// since returns samples with Time after now−d (a sample exactly d old has
// left the window).
func (s *Store) since(id string, d time.Duration) []probe.Sample {
	cutoff := s.clock.Now().Add(-d)
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.series[id]
	if !ok {
		return nil
	}
	all := r.items()
	i := sort.Search(len(all), func(i int) bool { return all[i].Time.After(cutoff) })
	return all[i:]
}

// IDs returns the registered series IDs, sorted.
func (s *Store) IDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.series))
	for id := range s.series {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Reset clears every series' samples (the `r` key, SPEC §9). Series stay
// registered.
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.series {
		r.clear()
	}
}

// ring is a fixed-capacity FIFO that overwrites its oldest element.
type ring struct {
	buf   []probe.Sample
	start int // index of the oldest element
	n     int
}

func newRing(capacity int) *ring { return &ring{buf: make([]probe.Sample, max(capacity, 1))} }

func (r *ring) push(s probe.Sample) {
	if r.n < len(r.buf) {
		r.buf[(r.start+r.n)%len(r.buf)] = s
		r.n++
		return
	}
	r.buf[r.start] = s
	r.start = (r.start + 1) % len(r.buf)
}

// items returns a copy, oldest first.
func (r *ring) items() []probe.Sample {
	out := make([]probe.Sample, r.n)
	for i := range r.n {
		out[i] = r.buf[(r.start+i)%len(r.buf)]
	}
	return out
}

func (r *ring) clear() {
	clear(r.buf)
	r.start, r.n = 0, 0
}
