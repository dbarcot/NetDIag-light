package probe

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"time"
)

// Path MTU search bounds (SPEC §5 MTU-01).
const (
	MinMTU = 576
	MaxMTU = 1500
	// ipv4ICMPOverhead is the 20-byte IPv4 header plus the 8-byte ICMP
	// header: an echo with payload p is a packet of p+28 bytes.
	ipv4ICMPOverhead = 28
)

// How the upper bound of a path MTU result was established
// (Sample.Bound, SPEC §5 MTU-01).
const (
	// BoundSignalled: a router answered a bigger DF packet with "packet too
	// big" — path MTU discovery works, a smaller MTU is just a smaller MTU
	// (PPPoE, VPN).
	BoundSignalled = "signalled"
	// BoundSilent: bigger packets were only ever lost — a PMTUD black hole.
	BoundSilent = "silent"
)

// MTUPace is the shortest gap between two echoes of one MTU-01 run: many
// routers answer ICMP at a limited rate, and a rate-limited reply must not
// be read as "too big".
const MTUPace = 200 * time.Millisecond

// MTURerunAfter is when MTU-01 runs again after its first confirmed low,
// silently bounded result, instead of waiting the whole rates.mtu interval.
const MTURerunAfter = 60 * time.Second

// MTUProbe finds the path MTU to Host with DF echo requests and a binary
// search between 576 and 1500 (MTU-01). A reply means the size fits;
// IP_PACKET_TOO_BIG, or a timeout that repeats once, means it does not.
// Echoes are at least MTUPace apart. A result below 1500 is confirmed
// before it is reported: its size must answer again and the next size must
// fail twice, otherwise the run is a failure ("MTU result not confirmed"),
// never a low MTU. Sample.Value is the MTU; Sample.Bound tells how the
// upper bound was found. A local buffer error is a bug and fails the probe
// as local_error.
type MTUProbe struct {
	ICMP     ICMP
	Resolver Resolver
	Host     string
	Timeout  time.Duration // per echo
	// LowBelow is thresholds.mtu_min: the first confirmed silent result
	// below it asks for a re-run after MTURerunAfter. 0 disables that.
	LowBelow int
	// Sleep waits between echoes; nil uses real time. Tests and the
	// simulator inject their own.
	Sleep func(ctx context.Context, d time.Duration)

	mu       sync.Mutex
	rerunned bool // the low result already asked for its re-run
}

// run holds the state of one MTU-01 run.
type mtuRun struct {
	p      *MTUProbe
	ctx    context.Context
	dest   netip.Addr
	sent   bool
	signal map[int]bool // sizes answered with "packet too big"
}

type mtuStep int

const (
	mtuFits mtuStep = iota
	mtuSignalled
	mtuSilent
)

func (p *MTUProbe) Run(ctx context.Context) Sample {
	ips, err := p.Resolver.LookupIPv4(ctx, p.Host)
	if err == nil && len(ips) == 0 {
		err = noAddress(p.Host)
	}
	if err != nil {
		return resolveFailure(p.Host, err)
	}
	r := &mtuRun{p: p, ctx: ctx, dest: ips[0], signal: map[int]bool{}}
	fail := func(class, detail string) Sample {
		return Sample{Result: Fail, ErrClass: class, Detail: detail, From: r.dest}
	}

	st, bad := r.step(MaxMTU, true)
	if bad != nil {
		return *bad
	}
	if st == mtuFits {
		p.settle(false)
		return Sample{Result: OK, Value: MaxMTU, From: r.dest, Detail: "path MTU 1500"}
	}
	// is the host answering at all at the smallest size?
	if st, bad = r.step(MinMTU, true); bad != nil {
		return *bad
	}
	if st != mtuFits {
		return fail(ClassTimeout, fmt.Sprintf("no reply to a %d-byte packet with DF", MinMTU))
	}
	lo, hi := MinMTU, MaxMTU // lo fits, hi does not
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if st, bad = r.step(mid, true); bad != nil {
			return *bad
		}
		if st == mtuFits {
			lo = mid
		} else {
			hi = mid
		}
	}

	// confirm the boundary: lo answers again, lo+1 fails twice
	if st, bad = r.step(lo, true); bad != nil {
		return *bad
	}
	if st != mtuFits {
		return fail(ClassOther, "MTU result not confirmed")
	}
	for range 2 {
		if st, bad = r.step(hi, false); bad != nil {
			return *bad
		}
		if st == mtuFits {
			return fail(ClassOther, "MTU result not confirmed")
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(ClassTimeout, "interrupted")
	}

	bound := BoundSilent
	for size := range r.signal {
		if size > lo {
			bound = BoundSignalled
		}
	}
	s := Sample{Result: OK, Value: float64(lo), From: r.dest, Bound: bound,
		Detail: fmt.Sprintf("path MTU %d (%s bound)", lo, bound)}
	if p.settle(bound == BoundSilent && p.LowBelow > 0 && lo < p.LowBelow) {
		s.RerunAfter = MTURerunAfter
	}
	return s
}

// settle remembers whether the last confirmed result was low and silent,
// and reports whether this one is the first of such a series (it asks for
// the one early re-run).
func (p *MTUProbe) settle(low bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	first := low && !p.rerunned
	p.rerunned = low
	return first
}

func (r *mtuRun) pace() {
	if r.sent {
		sleep := r.p.Sleep
		if sleep == nil {
			sleep = realSleep
		}
		sleep(r.ctx, MTUPace)
	}
	r.sent = true
}

func realSleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// step sends DF echoes of a packet of mtu bytes and says whether it fits,
// was refused with "packet too big" or was silently lost. With retry, a
// timeout is tried once more before it counts as lost. A non-nil sample is
// a failure that ends the run (local error, unreachable, ...).
func (r *mtuRun) step(mtu int, retry bool) (mtuStep, *Sample) {
	req := EchoRequest{Dest: r.dest, DF: true, Size: mtu - ipv4ICMPOverhead, Timeout: r.p.Timeout}
	attempts := 1
	if retry {
		attempts = 2
	}
	for attempt := range attempts {
		r.pace()
		reply, err := r.p.ICMP.Echo(r.ctx, req)
		if err != nil {
			if r.ctx.Err() != nil {
				return mtuSilent, &Sample{Result: Fail, ErrClass: ClassTimeout, Detail: "interrupted", From: r.dest}
			}
			return mtuSilent, &Sample{Result: Fail, ErrClass: ClassLocal, Detail: err.Error(), From: r.dest}
		}
		switch reply.Status {
		case StatusSuccess:
			if reply.From == r.dest {
				return mtuFits, nil
			}
			return mtuSilent, &Sample{Result: Fail, ErrClass: ClassOther, From: reply.From,
				Detail: fmt.Sprintf("echo reply from %s instead of %s", reply.From, r.dest)}
		case StatusPacketTooBig:
			r.signal[mtu] = true
			return mtuSignalled, nil
		case StatusTimedOut:
			if attempt+1 < attempts && r.ctx.Err() == nil {
				continue // a lost packet looks like a black hole once; ask again
			}
			return mtuSilent, nil
		default:
			s := EchoSample(r.dest, reply, nil)
			return mtuSilent, &s
		}
	}
	return mtuSilent, nil
}

// FakeSleep records the waits instead of sleeping (tests).
type FakeSleep struct {
	mu    sync.Mutex
	Waits []time.Duration
}

func (f *FakeSleep) Sleep(_ context.Context, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Waits = append(f.Waits, d)
}
