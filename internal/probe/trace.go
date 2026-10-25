package probe

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"netdiag/internal/clock"
)

// MinTraceInterval caps TRC-01 at 5 packets/s in total (CLAUDE.md probe
// rate rule): a shorter configured trace.interval_ms is raised to it.
const MinTraceInterval = 200 * time.Millisecond

// traceSeriesInterval sizes the store rings of hop series: a hop is
// refreshed once per cycle, a few seconds.
const traceSeriesInterval = 3 * time.Second

// HopSeriesID is the series ID of the hop at a TTL: each hop is its own
// series (SPEC §5 TRC-01).
func HopSeriesID(ttl int) string { return fmt.Sprintf("TRC-01:%02d", ttl) }

// Hop is what the tracer knows about one TTL.
type Hop struct {
	TTL int
	// IPs are the routers that answered at this TTL, in order of first
	// sighting; several mean load balancing (ECMP).
	IPs []netip.Addr
	// Names are the cached reverse DNS names of IPs ("" while unknown).
	Names []string
	// Final marks the hop where the destination itself answered.
	Final bool
}

// Namer resolves reverse DNS names asynchronously and caches them
// (including failures), at most one lookup per address.
type Namer struct {
	// Lookup resolves an address to names (net.Resolver.LookupAddr).
	Lookup func(ctx context.Context, addr string) ([]string, error)

	mu    sync.Mutex
	names map[netip.Addr]string
	wg    sync.WaitGroup
}

// NewNamer returns a Namer using the system resolver with a 2 s timeout.
func NewNamer() *Namer {
	return &Namer{Lookup: func(ctx context.Context, addr string) ([]string, error) {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return net.DefaultResolver.LookupAddr(ctx, addr)
	}}
}

// Request starts the lookup of ip unless it is known or running.
func (n *Namer) Request(ctx context.Context, ip netip.Addr) {
	n.mu.Lock()
	if n.names == nil {
		n.names = map[netip.Addr]string{}
	}
	if _, seen := n.names[ip]; seen {
		n.mu.Unlock()
		return
	}
	n.names[ip] = "" // pending, and the negative result if the lookup fails
	n.mu.Unlock()
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		names, err := n.Lookup(ctx, ip.String())
		if err != nil || len(names) == 0 {
			return
		}
		n.mu.Lock()
		n.names[ip] = strings.TrimSuffix(names[0], ".")
		n.mu.Unlock()
	}()
}

// Name returns the cached name of ip, "" if unknown.
func (n *Namer) Name(ip netip.Addr) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.names[ip]
}

// Wait blocks until the running lookups have finished (tests).
func (n *Namer) Wait() { n.wg.Wait() }

// Tracer is TRC-01, an mtr-style hop probe. It sends one TTL-limited echo
// every Interval (at least 200 ms), cycling TTL 1…N, where N is the TTL at
// which the destination answered plus one, or MaxHops while it is not
// known. Each TTL is its own series (HopSeriesID); a router that answers
// with "TTL expired" is a successful sample of that hop, a timeout is a
// failure (loss, which a renderer dims for hops before the final one).
type Tracer struct {
	ICMP     ICMP
	Resolver Resolver
	Host     string // trace target: IPv4 or name
	MaxHops  int
	Interval time.Duration // gap between packets; raised to MinTraceInterval
	Timeout  time.Duration
	Clock    clock.Clock
	// Gate says whether to probe now: run, or why not. Not running with an
	// empty reason means the gate is undecided (wait without a sample); a
	// reason records skipped samples for the known hops. Nil: always run.
	Gate func() (run bool, reason string)
	// Emit receives every sample; Register is called once per new hop
	// series before its first sample.
	Emit     func(Sample)
	Register func(id string, interval time.Duration)
	// Namer, if set, gets reverse DNS requests for new hop addresses.
	Namer *Namer

	mu      sync.Mutex
	dest    netip.Addr
	cur     int // next TTL to probe; 0 = start
	destTTL int // TTL at which the destination answered; 0 = unknown
	hops    map[int]*hopState
	newHops []int // hops found by record, registered by Step outside the lock
}

type hopState struct {
	ips   []netip.Addr
	final bool
}

func (t *Tracer) interval() time.Duration { return max(t.Interval, MinTraceInterval) }

// Hops returns the hops seen so far, ordered by TTL.
func (t *Tracer) Hops() []Hop {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Hop, 0, len(t.hops))
	for ttl, h := range t.hops {
		hop := Hop{TTL: ttl, IPs: slices.Clone(h.ips), Final: h.final}
		for _, ip := range h.ips {
			name := ""
			if t.Namer != nil {
				name = t.Namer.Name(ip)
			}
			hop.Names = append(hop.Names, name)
		}
		out = append(out, hop)
	}
	slices.SortFunc(out, func(a, b Hop) int { return a.TTL - b.TTL })
	return out
}

// Dest returns the address being traced (invalid before the first step).
func (t *Tracer) Dest() netip.Addr {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dest
}

// Run steps on the clock until ctx ends: one packet per interval on a
// fixed grid; slots missed while a probe ran are dropped, not caught up.
func (t *Tracer) Run(ctx context.Context) {
	next := t.Clock.Now().Add(t.interval())
	for {
		tm := t.Clock.NewTimer(next.Sub(t.Clock.Now()))
		select {
		case <-ctx.Done():
			tm.Stop()
			return
		case <-tm.C():
		}
		t.Step(ctx)
		if ctx.Err() != nil {
			return
		}
		next = nextSlot(next, t.interval(), t.Clock.Now())
	}
}

func nextSlot(prev time.Time, interval time.Duration, now time.Time) time.Time {
	next := prev.Add(interval)
	if next.After(now) {
		return next
	}
	return next.Add((now.Sub(next)/interval + 1) * interval)
}

// maxTTL is the highest TTL of the cycle.
func (t *Tracer) maxTTL() int {
	if t.destTTL > 0 {
		return min(t.destTTL+1, t.MaxHops)
	}
	return t.MaxHops
}

// advance moves to the next TTL of the cycle.
func (t *Tracer) advance() {
	t.cur++
	if t.cur > t.maxTTL() {
		t.cur = 1
	}
}

// Step sends one probe (or records one skipped sample) and advances the
// cycle. It is the unit tests drive; Run calls it once per interval.
func (t *Tracer) Step(ctx context.Context) {
	if t.cur == 0 {
		t.cur = 1
	}
	if t.Gate != nil {
		run, reason := t.Gate()
		switch {
		case run:
		case reason == "":
			return // undecided: wait without a sample
		default:
			t.mu.Lock()
			_, known := t.hops[t.cur]
			t.mu.Unlock()
			if known {
				s := SkippedSample(reason)
				t.stamp(&s, t.cur, t.Clock.Now())
				t.Emit(s)
			}
			t.advance()
			return
		}
	}
	if t.cur == 1 && !t.resolve(ctx) {
		return
	}
	ttl := t.cur
	at := t.Clock.Now()
	r, err := t.ICMP.Echo(ctx, EchoRequest{Dest: t.dest, TTL: uint8(ttl), Timeout: t.Timeout})
	if ctx.Err() != nil {
		return
	}
	t.advance()
	s, ok := t.record(ctx, ttl, r, err)
	// Register runs outside the tracer's lock: its callee takes the
	// engine's lock, and a snapshot holds that one while reading Hops.
	t.mu.Lock()
	newHops := t.newHops
	t.newHops = nil
	t.mu.Unlock()
	if t.Register != nil {
		for _, h := range newHops {
			t.Register(HopSeriesID(h), traceSeriesInterval)
		}
	}
	if ok {
		t.stamp(&s, ttl, at)
		t.Emit(s)
	}
}

// resolve (re)resolves the trace target at the start of a cycle; a change
// of address starts a new trace. It reports whether a destination is known.
func (t *Tracer) resolve(ctx context.Context) bool {
	ips, err := t.Resolver.LookupIPv4(ctx, t.Host)
	if err != nil || len(ips) == 0 {
		return t.dest.IsValid() // keep tracing the last known address
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dest != ips[0] {
		t.dest, t.destTTL, t.hops = ips[0], 0, map[int]*hopState{}
	}
	return true
}

func (t *Tracer) stamp(s *Sample, ttl int, at time.Time) {
	s.Time, s.Series, s.Probe, s.Target = at, HopSeriesID(ttl), "TRC-01", fmt.Sprintf("hop %d", ttl)
}

// record interprets one reply at a TTL and updates the hop state. ok is
// false for hops beyond the end of the path, which are not recorded.
func (t *Tracer) record(ctx context.Context, ttl int, r EchoReply, err error) (Sample, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	beyond := t.destTTL > 0 && ttl > t.destTTL
	if err != nil {
		return Sample{Result: Fail, ErrClass: ClassLocal, Detail: err.Error()}, !beyond
	}
	hop := func() *hopState {
		h, ok := t.hops[ttl]
		if !ok {
			h = &hopState{}
			if t.hops == nil {
				t.hops = map[int]*hopState{}
			}
			t.hops[ttl] = h
			t.newHops = append(t.newHops, ttl)
		}
		return h
	}
	seen := func(h *hopState, ip netip.Addr) {
		if !slices.Contains(h.ips, ip) {
			h.ips = append(h.ips, ip)
			if t.Namer != nil {
				t.Namer.Request(ctx, ip)
			}
		}
	}
	switch {
	case r.Status == StatusSuccess && r.From == t.dest:
		if beyond {
			return Sample{}, false
		}
		h := hop()
		seen(h, r.From)
		t.destTTL = ttl // the shortest TTL that reaches the destination
		for k, other := range t.hops {
			other.final = k == ttl
		}
		return Sample{Result: OK, RTT: r.RTT, From: r.From}, true
	case r.Status == StatusSuccess || r.Status == StatusTTLExpiredTransit || r.Status == StatusTTLExpiredReassem:
		if beyond {
			return Sample{}, false
		}
		if t.destTTL > 0 && ttl >= t.destTTL {
			// a router answers where the destination did: the path got longer
			t.destTTL = 0
			for _, other := range t.hops {
				other.final = false
			}
		}
		h := hop()
		seen(h, r.From)
		return Sample{Result: OK, RTT: r.RTT, From: r.From}, true
	default:
		if beyond {
			return Sample{}, false
		}
		hop()
		s := EchoSample(t.dest, r, nil)
		if r.Status == StatusTimedOut {
			s.Detail = "no reply"
		}
		return s, true
	}
}
