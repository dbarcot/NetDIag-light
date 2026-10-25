package probe

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"netdiag/internal/clock"
)

var (
	traceDest = netip.MustParseAddr("1.1.1.1")
	r1        = netip.MustParseAddr("192.168.1.1")
	r2a       = netip.MustParseAddr("10.0.0.1")
	r2b       = netip.MustParseAddr("10.0.0.2")
	r3        = netip.MustParseAddr("172.16.0.1")
)

// path describes a fake route: reply for each TTL.
type path struct {
	mu     sync.Mutex
	reply  func(ttl int, n int) EchoReply // n: how many times this TTL was probed before
	counts map[int]int
	ttls   []int
}

func (p *path) icmp() *FakeICMP {
	return &FakeICMP{Reply: func(req EchoRequest) (EchoReply, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.counts == nil {
			p.counts = map[int]int{}
		}
		ttl := int(req.TTL)
		n := p.counts[ttl]
		p.counts[ttl]++
		p.ttls = append(p.ttls, ttl)
		return p.reply(ttl, n), nil
	}}
}

func expired(from netip.Addr) EchoReply {
	return EchoReply{Status: StatusTTLExpiredTransit, From: from, RTT: 5 * time.Millisecond}
}

func reached() EchoReply {
	return EchoReply{Status: StatusSuccess, From: traceDest, RTT: 20 * time.Millisecond}
}

// fourHops: r1, r2, r3, destination at TTL 4 (anything above reaches it).
func fourHops(ttl, n int) EchoReply {
	switch ttl {
	case 1:
		return expired(r1)
	case 2:
		return expired(r2a)
	case 3:
		return expired(r3)
	default:
		return reached()
	}
}

type collector struct {
	mu       sync.Mutex
	samples  []Sample
	register []string
}

func (c *collector) emit(s Sample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples = append(c.samples, s)
}

func (c *collector) reg(id string, _ time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.register = append(c.register, id)
}

func (c *collector) of(ttl int) []Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Sample
	for _, s := range c.samples {
		if s.Series == HopSeriesID(ttl) {
			out = append(out, s)
		}
	}
	return out
}

func newTracer(p *path, col *collector) *Tracer {
	res := &FakeResolver{}
	return &Tracer{
		ICMP: p.icmp(), Resolver: res, Host: traceDest.String(), MaxHops: 8, Interval: 200 * time.Millisecond,
		Timeout: time.Second, Clock: clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		Emit: col.emit, Register: col.reg,
	}
}

func steps(t *Tracer, n int) {
	for range n {
		t.Step(context.Background())
	}
}

// Until the destination answers the cycle runs up to MaxHops; once it
// answered at TTL 4 the cycle is 1…5 (length + 1) and hops beyond the end
// are not recorded.
func TestTracer_AssemblesHopsAndShrinksTheCycle(t *testing.T) {
	t.Parallel()
	p := &path{reply: fourHops}
	col := &collector{}
	tr := newTracer(p, col)
	steps(tr, 18)

	want := []int{1, 2, 3, 4, 5, 1, 2, 3, 4, 5, 1, 2, 3, 4, 5, 1, 2, 3}
	if len(p.ttls) != len(want) {
		t.Fatalf("TTLs %v", p.ttls)
	}
	for i := range want {
		if p.ttls[i] != want[i] {
			t.Fatalf("TTLs %v, want %v", p.ttls, want)
		}
	}
	hops := tr.Hops()
	if len(hops) != 4 {
		t.Fatalf("hops %+v, want 4", hops)
	}
	for i, h := range hops {
		if h.TTL != i+1 || h.Final != (h.TTL == 4) {
			t.Errorf("hop %+v", h)
		}
	}
	if hops[0].IPs[0] != r1 || hops[1].IPs[0] != r2a || hops[3].IPs[0] != traceDest {
		t.Errorf("hops %+v", hops)
	}
	if len(col.of(5)) != 0 || len(col.of(6)) != 0 {
		t.Errorf("hops beyond the destination recorded: %d, %d", len(col.of(5)), len(col.of(6)))
	}
	// the probe at TTL 5 answers from the destination too; TTL 4 is the end
	if len(col.register) != 4 {
		t.Errorf("registered %v, want hops 1-4 once each", col.register)
	}
	for _, s := range col.of(2) {
		if s.Result != OK || s.From != r2a || s.Probe != "TRC-01" || s.Target != "hop 2" || s.RTT <= 0 {
			t.Errorf("hop 2 sample %+v", s)
		}
	}
	if tr.Dest() != traceDest {
		t.Errorf("dest %v", tr.Dest())
	}
}

// Several routers answering at one TTL (load balancing) are all kept.
func TestTracer_ECMP(t *testing.T) {
	t.Parallel()
	p := &path{reply: func(ttl, n int) EchoReply {
		if ttl == 2 && n%2 == 1 {
			return expired(r2b)
		}
		return fourHops(ttl, n)
	}}
	tr := newTracer(p, &collector{})
	steps(tr, 20)
	h2 := tr.Hops()[1]
	if len(h2.IPs) != 2 || h2.IPs[0] != r2a || h2.IPs[1] != r2b {
		t.Errorf("hop 2 IPs %v, want both routers in order of sighting", h2.IPs)
	}
}

// A router that never answers TTL-expired shows as loss at its hop while
// the final hop is fine (mtr intermediate-hop loss).
func TestTracer_NonFinalHopLoss(t *testing.T) {
	t.Parallel()
	p := &path{reply: func(ttl, n int) EchoReply {
		if ttl == 2 {
			return EchoReply{Status: StatusTimedOut}
		}
		return fourHops(ttl, n)
	}}
	col := &collector{}
	tr := newTracer(p, col)
	steps(tr, 20)
	for _, s := range col.of(2) {
		if s.Result != Fail || s.ErrClass != ClassTimeout {
			t.Errorf("hop 2 sample %+v, want fail/timeout", s)
		}
	}
	if len(col.of(2)) != 4 {
		t.Errorf("hop 2 has %d samples, want 4 (one per cycle)", len(col.of(2)))
	}
	for _, s := range col.of(4) {
		if s.Result != OK {
			t.Errorf("final hop sample %+v", s)
		}
	}
	h := tr.Hops()
	if len(h[1].IPs) != 0 || !h[3].Final {
		t.Errorf("hops %+v: hop 2 has no address, hop 4 is final", h)
	}
}

// When the path gets longer the old end becomes an ordinary router and the
// cycle grows back to MaxHops until the destination is found again.
func TestTracer_PathChange(t *testing.T) {
	t.Parallel()
	longer := false
	var mu sync.Mutex
	p := &path{reply: func(ttl, n int) EchoReply {
		mu.Lock()
		defer mu.Unlock()
		if longer {
			switch ttl {
			case 4:
				return expired(netip.MustParseAddr("172.16.9.9"))
			case 5, 6:
				return reached()
			}
		}
		return fourHops(ttl, n)
	}}
	tr := newTracer(p, &collector{})
	steps(tr, 13)
	if !tr.Hops()[3].Final {
		t.Fatal("hop 4 should be final")
	}
	mu.Lock()
	longer = true
	mu.Unlock()
	steps(tr, 5) // hop 4 now answers TTL expired, the destination is found at TTL 5
	hops := tr.Hops()
	if len(hops) != 5 {
		t.Fatalf("hops %+v, want 5", hops)
	}
	if hops[3].Final || len(hops[3].IPs) != 2 {
		t.Errorf("hop 4 %+v: no longer final, and it has seen both addresses", hops[3])
	}
	if !hops[4].Final || hops[4].IPs[0] != traceDest {
		t.Errorf("hop 5 %+v, want the new final hop", hops[4])
	}
	// the cycle follows: 1…6 now
	p.ttls = nil
	steps(tr, 6)
	if slices.Max(p.ttls) != 6 {
		t.Errorf("cycle after the change: %v, want 1…6", p.ttls)
	}
}

func TestTracer_GateSkipsWaitsAndRuns(t *testing.T) {
	t.Parallel()
	p := &path{reply: fourHops}
	col := &collector{}
	tr := newTracer(p, col)
	steps(tr, 8) // learn the hops
	n := len(col.samples)

	run, reason := false, ""
	tr.Gate = func() (bool, string) { return run, reason }
	steps(tr, 3) // undecided: nothing happens
	if len(col.samples) != n || len(p.ttls) != 8 {
		t.Errorf("an undecided gate must not probe or record (samples %d, echoes %d)", len(col.samples)-n, len(p.ttls)-8)
	}
	reason = "gateway down"
	steps(tr, 5) // current TTL cycles over the known hops: skipped samples, no probes
	if len(p.ttls) != 8 {
		t.Error("a closed gate must not probe")
	}
	skipped := col.samples[n:]
	if len(skipped) == 0 {
		t.Fatal("no skipped samples")
	}
	for _, s := range skipped {
		if s.Result != Skipped || s.Detail != "skipped: gateway down" {
			t.Errorf("sample %+v", s)
		}
	}
	run = true
	steps(tr, 1)
	if len(p.ttls) != 9 {
		t.Error("an open gate probes again")
	}
}

func TestTracer_LocalErrorAndContext(t *testing.T) {
	t.Parallel()
	col := &collector{}
	tr := newTracer(&path{reply: fourHops}, col)
	tr.ICMP = &FakeICMP{Reply: func(EchoRequest) (EchoReply, error) { return EchoReply{}, errors.New("boom") }}
	steps(tr, 1)
	if s := col.of(1); len(s) != 1 || s[0].ErrClass != ClassLocal {
		t.Errorf("got %+v", col.samples)
	}
	// a cancelled context records nothing
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n := len(col.samples)
	tr.Step(ctx)
	if len(col.samples) != n {
		t.Error("a cancelled step recorded a sample")
	}
}

func TestTracer_IntervalCappedAt5PerSecond(t *testing.T) {
	t.Parallel()
	tr := &Tracer{Interval: 100 * time.Millisecond}
	if tr.interval() != 200*time.Millisecond {
		t.Errorf("interval %v, want the 200 ms floor (5 packets/s)", tr.interval())
	}
	tr.Interval = time.Second
	if tr.interval() != time.Second {
		t.Errorf("interval %v", tr.interval())
	}
}

// Run sends one packet per interval on the clock and stops with the context.
func TestTracer_RunPacesOnTheClock(t *testing.T) {
	t.Parallel()
	p := &path{reply: fourHops}
	col := &collector{}
	tr := newTracer(p, col)
	fake := tr.Clock.(*clock.Fake)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tr.Run(ctx); close(done) }()
	for i := 1; i <= 5; i++ {
		if !fake.WaitTimers(1, 2*time.Second) {
			t.Fatal("tracer did not wait on the clock")
		}
		fake.Advance(200 * time.Millisecond)
		deadline := time.Now().Add(2 * time.Second)
		for {
			p.mu.Lock()
			n := len(p.ttls)
			p.mu.Unlock()
			if n == i {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("after %d intervals %d probes", i, n)
			}
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestTracer_NewDestinationStartsANewTrace(t *testing.T) {
	t.Parallel()
	p := &path{reply: fourHops}
	col := &collector{}
	tr := newTracer(p, col)
	res := tr.Resolver.(*FakeResolver)
	tr.Host = "trace.test"
	res.Set("trace.test", traceDest)
	steps(tr, 8+5)
	if len(tr.Hops()) != 4 {
		t.Fatal("setup")
	}
	res.Set("trace.test", netip.MustParseAddr("9.9.9.9")) // host moved
	tr.cur = 0
	steps(tr, 1)
	if tr.Dest() != netip.MustParseAddr("9.9.9.9") || len(tr.Hops()) > 1 {
		t.Errorf("dest %v, hops %d: the old trace must be dropped", tr.Dest(), len(tr.Hops()))
	}
	// an unresolvable name keeps tracing the last address
	res.Set("trace.test")
	tr.cur = 0
	before := len(p.ttls)
	steps(tr, 1)
	if len(p.ttls) != before+1 || tr.Dest() != netip.MustParseAddr("9.9.9.9") {
		t.Errorf("a failed re-resolution must keep the last address")
	}
	// ... but with no address at all nothing is probed
	tr2 := newTracer(&path{reply: fourHops}, &collector{})
	tr2.Host = "never.test"
	steps(tr2, 2)
	if tr2.Dest().IsValid() {
		t.Error("unresolvable from the start: no destination")
	}
}

func TestNamer_AsyncCachedReverseDNS(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	calls := map[string]int{}
	n := &Namer{Lookup: func(_ context.Context, addr string) ([]string, error) {
		mu.Lock()
		calls[addr]++
		mu.Unlock()
		if addr == r1.String() {
			return []string{"router.lan."}, nil
		}
		return nil, errors.New("NXDOMAIN")
	}}
	ctx := context.Background()
	n.Request(ctx, r1)
	n.Request(ctx, r1)
	n.Request(ctx, r2a)
	n.Wait()
	n.Request(ctx, r2a) // a failed lookup is cached too
	n.Wait()
	if n.Name(r1) != "router.lan" || n.Name(r2a) != "" {
		t.Errorf("names %q %q", n.Name(r1), n.Name(r2a))
	}
	if calls[r1.String()] != 1 || calls[r2a.String()] != 1 {
		t.Errorf("lookups %v, want one per address", calls)
	}
}

func TestTracer_HopNames(t *testing.T) {
	t.Parallel()
	p := &path{reply: fourHops}
	tr := newTracer(p, &collector{})
	tr.Namer = &Namer{Lookup: func(_ context.Context, addr string) ([]string, error) {
		return []string{"host-" + addr + "."}, nil
	}}
	steps(tr, 8)
	tr.Namer.Wait()
	if h := tr.Hops()[0]; len(h.Names) != 1 || h.Names[0] != "host-192.168.1.1" {
		t.Errorf("hop 1 %+v", h)
	}
}

// realClock is the system clock for tests that must not use the fake one.
type realClock struct{ clock.Real }
