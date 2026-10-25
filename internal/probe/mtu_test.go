package probe

import (
	"context"
	"errors"
	"net/netip"
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

func runMTU(icmp ICMP) Sample {
	res := &FakeResolver{}
	return MTUProbe{ICMP: icmp, Resolver: res, Host: mtuDest.String(), Timeout: time.Second}.Run(context.Background())
}

func TestMTUProbe_BinarySearch(t *testing.T) {
	t.Parallel()
	for _, mtu := range []int{1500, 1492, 1480, 1420, 1400, 1399, 1280, 1000, 577, 576} {
		for _, status := range []ICMPStatus{StatusPacketTooBig, StatusTimedOut} {
			icmp := pathICMP(mtu, status)
			s := runMTU(icmp)
			if s.Result != OK || int(s.Value) != mtu {
				t.Errorf("path MTU %d (%v): %+v", mtu, status, s)
			}
			// ~log2(924) steps; with timeouts each failing step is tried twice
			if n := len(icmp.Requests); n > 30 {
				t.Errorf("path MTU %d (%v): %d echoes", mtu, status, n)
			}
		}
	}
}

func TestMTUProbe_FullSizeIsOneEcho(t *testing.T) {
	t.Parallel()
	icmp := pathICMP(1500, StatusPacketTooBig)
	if s := runMTU(icmp); int(s.Value) != 1500 || len(icmp.Requests) != 1 {
		t.Errorf("%+v after %d echoes", s, len(icmp.Requests))
	}
	if sz := icmp.Requests[0].Size; sz != 1472 {
		t.Errorf("payload %d bytes, want 1472 (1500 - 28)", sz)
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
	s := MTUProbe{ICMP: icmp, Resolver: res, Host: "trace.test", Timeout: time.Second}.Run(context.Background())
	if s.Result != OK || s.From != netip.MustParseAddr("9.9.9.9") {
		t.Errorf("got %+v", s)
	}
	s = MTUProbe{ICMP: icmp, Resolver: res, Host: "missing.test", Timeout: time.Second}.Run(context.Background())
	if s.Result != Fail || s.ErrClass != ClassDNS {
		t.Errorf("unresolvable: %+v", s)
	}
}
