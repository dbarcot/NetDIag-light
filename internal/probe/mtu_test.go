package probe

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

var mtuDest = netip.MustParseAddr("1.1.1.1")

// pathICMP is a fake path with a fixed MTU: DF echoes up to it are
// answered; bigger ones get the given status.
func pathICMP(mtu int, tooBig ICMPStatus) *FakeICMP {
	return &FakeICMP{Reply: func(req EchoRequest) (EchoReply, error) {
		if !req.DF {
			return EchoReply{}, errors.New("MTU-01 must set DF")
		}
		if req.Size+28 <= mtu {
			return EchoReply{Status: StatusSuccess, From: req.Dest, RTT: time.Millisecond}, nil
		}
		return EchoReply{Status: tooBig}, nil
	}}
}

func newMTUProbe(icmp ICMP) (*MTUProbe, *FakeSleep) {
	sl := &FakeSleep{}
	return &MTUProbe{ICMP: icmp, Resolver: &FakeResolver{}, Host: mtuDest.String(), Timeout: time.Second,
		LowBelow: 1400, Sleep: sl.Sleep}, sl
}

func runMTU(icmp ICMP) Sample {
	p, _ := newMTUProbe(icmp)
	return p.Run(context.Background())
}

func TestMTUProbe_BinarySearchAndBound(t *testing.T) {
	t.Parallel()
	for _, mtu := range []int{1500, 1492, 1480, 1420, 1400, 1399, 1280, 1000, 577, 576} {
		for _, tc := range []struct {
			status ICMPStatus
			bound  string
		}{{StatusPacketTooBig, BoundSignalled}, {StatusTimedOut, BoundSilent}} {
			icmp := pathICMP(mtu, tc.status)
			s := runMTU(icmp)
			if s.Result != OK || int(s.Value) != mtu {
				t.Errorf("path MTU %d (%v): %+v", mtu, tc.status, s)
				continue
			}
			want := tc.bound
			if mtu == MaxMTU {
				want = ""
			}
			if s.Bound != want {
				t.Errorf("path MTU %d (%v): bound %q, want %q", mtu, tc.status, s.Bound, want)
			}
			// ~log2(924) steps, the confirmation, and a retry for each lost one
			if n := len(icmp.Requests); n > 45 {
				t.Errorf("path MTU %d (%v): %d echoes", mtu, tc.status, n)
			}
		}
	}
}

func TestMTUProbe_FullSizeIsOneEcho(t *testing.T) {
	t.Parallel()
	icmp := pathICMP(1500, StatusPacketTooBig)
	p, sl := newMTUProbe(icmp)
	if s := p.Run(context.Background()); int(s.Value) != 1500 || len(icmp.Requests) != 1 || s.RerunAfter != 0 {
		t.Errorf("%+v after %d echoes", s, len(icmp.Requests))
	}
	if sz := icmp.Requests[0].Size; sz != 1472 {
		t.Errorf("payload %d bytes, want 1472 (1500 - 28)", sz)
	}
	if len(sl.Waits) != 0 {
		t.Errorf("waits %v: nothing to pace with a single echo", sl.Waits)
	}
}

// Echoes are at least 200 ms apart (ICMP rate limiting of routers).
func TestMTUProbe_PacesEveryEcho(t *testing.T) {
	t.Parallel()
	icmp := pathICMP(1400, StatusPacketTooBig)
	p, sl := newMTUProbe(icmp)
	if s := p.Run(context.Background()); int(s.Value) != 1400 {
		t.Fatalf("%+v", s)
	}
	if len(sl.Waits) != len(icmp.Requests)-1 {
		t.Errorf("%d waits for %d echoes, want one before every echo but the first", len(sl.Waits), len(icmp.Requests))
	}
	for _, w := range sl.Waits {
		if w < 200*time.Millisecond {
			t.Errorf("wait %v, want at least 200 ms", w)
		}
	}
}

// dropper loses chosen echoes (by 1-based index) as a rate-limiting router
// would, whatever their size.
func dropper(inner *FakeICMP, drop map[int]bool) *FakeICMP {
	var mu sync.Mutex
	n := 0
	return &FakeICMP{Reply: func(req EchoRequest) (EchoReply, error) {
		mu.Lock()
		n++
		lost := drop[n]
		mu.Unlock()
		if lost {
			return EchoReply{Status: StatusTimedOut}, nil
		}
		return inner.Reply(req)
	}}
}

// One reply lost anywhere in the run (a rate-limited router): the retry
// absorbs it and the true MTU comes out — never a lower one.
func TestMTUProbe_OneDroppedReplyNeverLowersTheResult(t *testing.T) {
	t.Parallel()
	for _, tooBig := range []ICMPStatus{StatusPacketTooBig, StatusTimedOut} {
		for i := 1; i <= 40; i++ {
			s := runMTU(dropper(pathICMP(1492, tooBig), map[int]bool{i: true}))
			if s.Result != OK || int(s.Value) != 1492 {
				t.Errorf("echo %d dropped (%v): %+v, want MTU 1492", i, tooBig, s)
			}
		}
	}
}

// Two lost replies in a row can fool one step. The run then either still
// finds the right MTU or fails (unconfirmed, or silent at 576): it never
// reports a low MTU.
func TestMTUProbe_TwoDroppedRepliesAreNeverAFalseLowMTU(t *testing.T) {
	t.Parallel()
	unconfirmed := 0
	for _, tooBig := range []ICMPStatus{StatusPacketTooBig, StatusTimedOut} {
		for i := 1; i <= 40; i++ {
			s := runMTU(dropper(pathICMP(1492, tooBig), map[int]bool{i: true, i + 1: true}))
			switch {
			case s.Result == OK && int(s.Value) == 1492:
			case s.Result == Fail && s.Detail == "MTU result not confirmed":
				unconfirmed++
			case s.Result == Fail && s.ErrClass == ClassTimeout: // the host looked silent at 576
			default:
				t.Errorf("echoes %d+%d dropped (%v): %+v, want MTU 1492 or unconfirmed", i, i+1, tooBig, s)
			}
		}
	}
	if unconfirmed == 0 {
		t.Error("no run was unconfirmed: the confirmation is not exercised")
	}
}

// A path that answers a size once and drops it the next time is not a
// stable boundary.
func TestMTUProbe_FlappingBoundaryIsNotConfirmed(t *testing.T) {
	t.Parallel()
	n := 0
	icmp := &FakeICMP{Reply: func(req EchoRequest) (EchoReply, error) {
		n++
		// sizes up to 1400 always fit; everything above fits every other time
		if req.Size+28 <= 1400 || n%2 == 0 {
			return EchoReply{Status: StatusSuccess, From: req.Dest}, nil
		}
		return EchoReply{Status: StatusPacketTooBig}, nil
	}}
	s := runMTU(icmp)
	if s.Result == OK && s.Value != 1500 {
		t.Errorf("a flapping path produced the MTU %v: %+v", s.Value, s)
	}
}

// A host that never answers is a failure, not an MTU of 576.
func TestMTUProbe_HostNotAnswering(t *testing.T) {
	t.Parallel()
	icmp := &FakeICMP{Reply: func(EchoRequest) (EchoReply, error) { return EchoReply{Status: StatusTimedOut}, nil }}
	s := runMTU(icmp)
	if s.Result != Fail || s.ErrClass != ClassTimeout {
		t.Errorf("got %+v", s)
	}
}

// A buffer error from the OS call is a bug, never an MTU result.
func TestMTUProbe_LocalErrorIsNotAnMTU(t *testing.T) {
	t.Parallel()
	icmp := &FakeICMP{Reply: func(EchoRequest) (EchoReply, error) { return EchoReply{}, ErrLocal }}
	s := runMTU(icmp)
	if s.Result != Fail || s.ErrClass != ClassLocal {
		t.Errorf("got %+v", s)
	}
	// ... also in the middle of the search
	calls := 0
	icmp = &FakeICMP{Reply: func(req EchoRequest) (EchoReply, error) {
		calls++
		if calls > 3 {
			return EchoReply{}, ErrLocal
		}
		return pathICMP(1400, StatusPacketTooBig).Reply(req)
	}}
	if s := runMTU(icmp); s.Result != Fail || s.ErrClass != ClassLocal {
		t.Errorf("mid-search: %+v", s)
	}
}

func TestMTUProbe_OtherStatusesFail(t *testing.T) {
	t.Parallel()
	icmp := &FakeICMP{Reply: func(EchoRequest) (EchoReply, error) { return EchoReply{Status: StatusDestHostUnreachable}, nil }}
	if s := runMTU(icmp); s.Result != Fail || s.ErrClass != ClassUnreachable {
		t.Errorf("got %+v", s)
	}
}

func TestMTUProbe_ResolvesHostnames(t *testing.T) {
	t.Parallel()
	res := &FakeResolver{}
	res.Set("trace.test", netip.MustParseAddr("9.9.9.9"))
	icmp := &FakeICMP{Reply: func(req EchoRequest) (EchoReply, error) {
		return EchoReply{Status: StatusSuccess, From: req.Dest}, nil
	}}
	p := &MTUProbe{ICMP: icmp, Resolver: res, Host: "trace.test", Timeout: time.Second, Sleep: (&FakeSleep{}).Sleep}
	s := p.Run(context.Background())
	if s.Result != OK || s.From != netip.MustParseAddr("9.9.9.9") {
		t.Errorf("got %+v", s)
	}
	p.Host = "missing.test"
	if s = p.Run(context.Background()); s.Result != Fail || s.ErrClass != ClassDNS {
		t.Errorf("unresolvable: %+v", s)
	}
}

// The first confirmed low result with a silent bound (a possible black
// hole) asks for one early re-run; the next such result does not ask
// again, a good result starts over.
func TestMTUProbe_OneEarlyRerunAfterAFirstLowResult(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	path := pathICMP(1380, StatusTimedOut)
	cur := path
	icmp := &FakeICMP{Reply: func(req EchoRequest) (EchoReply, error) {
		mu.Lock()
		c := cur
		mu.Unlock()
		return c.Reply(req)
	}}
	p, _ := newMTUProbe(icmp)
	set := func(f *FakeICMP) { mu.Lock(); cur = f; mu.Unlock() }
	run := func() Sample { return p.Run(context.Background()) }

	if s := run(); s.RerunAfter != time.Minute || s.Bound != BoundSilent {
		t.Errorf("first low silent run: %+v, want a re-run after 60 s", s)
	}
	if s := run(); s.RerunAfter != 0 || int(s.Value) != 1380 {
		t.Errorf("second low run: %+v, want no further early re-run", s)
	}
	set(pathICMP(1500, StatusPacketTooBig))
	if s := run(); s.RerunAfter != 0 || int(s.Value) != 1500 {
		t.Errorf("recovered: %+v", s)
	}
	set(path)
	if s := run(); s.RerunAfter != time.Minute {
		t.Errorf("low again after a good run: %+v, want a re-run again", s)
	}
	// a signalled bound is just a smaller MTU: nothing to re-check
	set(pathICMP(1380, StatusPacketTooBig))
	run() // resets the state with a different kind of result
	set(pathICMP(1300, StatusPacketTooBig))
	if s := run(); s.RerunAfter != 0 || s.Bound != BoundSignalled {
		t.Errorf("signalled low: %+v, want no re-run", s)
	}
	// an unconfirmed run is no result and asks for nothing
	set(dropper(pathICMP(1380, StatusTimedOut), map[int]bool{}))
	if s := run(); s.Result != OK {
		t.Fatalf("%+v", s)
	}
}
