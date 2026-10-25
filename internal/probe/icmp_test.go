package probe

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"
)

var (
	dest = netip.MustParseAddr("1.1.1.1")
	hop  = netip.MustParseAddr("10.0.0.1")
)

func TestEchoSample(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		reply  EchoReply
		err    error
		result Result
		class  string
	}{
		{"reply from dest", EchoReply{Status: StatusSuccess, From: dest, RTT: 12 * time.Millisecond}, nil, OK, ""},
		{"timeout", EchoReply{Status: StatusTimedOut}, nil, Fail, ClassTimeout},
		{"host unreachable", EchoReply{Status: StatusDestHostUnreachable, From: hop}, nil, Fail, ClassUnreachable},
		{"net unreachable", EchoReply{Status: StatusDestNetUnreachable}, nil, Fail, ClassUnreachable},
		{"TTL expired", EchoReply{Status: StatusTTLExpiredTransit, From: hop}, nil, Fail, ClassTTLExpired},
		{"packet too big", EchoReply{Status: StatusPacketTooBig, From: hop}, nil, Fail, ClassPacketTooBig},
		{"success from other host", EchoReply{Status: StatusSuccess, From: hop}, nil, Fail, ClassOther},
		{"local error", EchoReply{}, fmt.Errorf("%w: boom", ErrLocal), Fail, ClassLocal},
		{"unknown status", EchoReply{Status: 11099}, nil, Fail, ClassOther},
	}
	for _, tt := range tests {
		s := EchoSample(dest, tt.reply, tt.err)
		if s.Result != tt.result || s.ErrClass != tt.class {
			t.Errorf("%s: result=%s class=%q, want %s %q", tt.name, s.Result, s.ErrClass, tt.result, tt.class)
		}
		if s.Result == OK && s.RTT != tt.reply.RTT {
			t.Errorf("%s: RTT %v", tt.name, s.RTT)
		}
		if s.Result == Fail && s.Detail == "" {
			t.Errorf("%s: failure without detail", tt.name)
		}
	}
}

func TestICMPProbe_SendsDefaults(t *testing.T) {
	t.Parallel()
	f := &FakeICMP{Reply: func(r EchoRequest) (EchoReply, error) {
		return EchoReply{Status: StatusSuccess, From: r.Dest, RTT: 3 * time.Millisecond}, nil
	}}
	s := ICMPProbe{ICMP: f, Dest: dest, Timeout: time.Second}.Run(context.Background())
	if s.Result != OK || s.RTTms() != 3 {
		t.Errorf("sample %+v", s)
	}
	r := f.Requests[0]
	if r.Dest != dest || r.Timeout != time.Second || r.DF || r.TTL != 0 || r.Size != 0 {
		t.Errorf("request %+v: ordinary ping must use defaults (TTL 128, 32 bytes, no DF)", r)
	}
}

func TestEchoTimeout(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 25, 14, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		req      time.Duration
		deadline time.Duration // from now; 0 = no deadline
		want     time.Duration
		ok       bool
	}{
		{"no deadline", time.Second, 0, time.Second, true},
		{"deadline later than timeout", time.Second, 5 * time.Second, time.Second, true},
		{"deadline shortens timeout", time.Second, 300 * time.Millisecond, 300 * time.Millisecond, true},
		{"exactly 1 ms is allowed", time.Second, time.Millisecond, time.Millisecond, true},
		{"below 1 ms: timed out without calling the API", time.Second, 999 * time.Microsecond, 999 * time.Microsecond, false},
		{"deadline passed", time.Second, -time.Second, -time.Second, false},
		{"zero request timeout", 0, 0, 0, false},
	}
	for _, tt := range tests {
		got, ok := EchoTimeout(tt.req, now.Add(tt.deadline), tt.deadline != 0, now)
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s: got %v, %v; want %v, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

func TestReplyBufferSize(t *testing.T) {
	t.Parallel()
	const reply = 40 // sizeof(ICMP_ECHO_REPLY) on amd64
	tests := map[int]int{
		0:    256,            // minimum
		32:   256,            // 40+32+8+32 = 112 → minimum
		200:  280,            // 40+200+8+32
		1472: 40 + 1472 + 40, // MTU-01 largest probe
	}
	for payload, want := range tests {
		if got := ReplyBufferSize(payload, reply); got != want {
			t.Errorf("ReplyBufferSize(%d) = %d, want %d", payload, got, want)
		}
	}
}

func TestICMPStatus_StringAndClass(t *testing.T) {
	t.Parallel()
	if StatusTimedOut.String() != "timed out" || StatusTimedOut.Class() != ClassTimeout {
		t.Error("timeout")
	}
	if StatusBufTooSmall.Class() != ClassLocal {
		t.Error("buffer too small must be a local error, never a network result")
	}
	if !strings.Contains(ICMPStatus(12345).String(), "12345") {
		t.Error("unknown status string")
	}
}

func TestTimerOverhead_ReturnsMinimum(t *testing.T) {
	t.Parallel()
	rtts := []time.Duration{90, 40, 70, 35, 60, 80, 45, 50, 55, 65}
	i := 0
	f := &FakeICMP{Reply: func(r EchoRequest) (EchoReply, error) {
		d := rtts[i] * time.Microsecond
		i++
		return EchoReply{Status: StatusSuccess, From: r.Dest, RTT: d}, nil
	}}
	got, err := TimerOverhead(context.Background(), f, 10)
	if err != nil || got != 35*time.Microsecond {
		t.Errorf("TimerOverhead = %v, %v; want 35µs", got, err)
	}
	if len(f.Requests) != 10 || f.Requests[0].Dest != Loopback {
		t.Errorf("requests %+v", f.Requests)
	}
}

func TestTimerOverhead_IgnoresFailuresButNeedsOneReply(t *testing.T) {
	t.Parallel()
	n := 0
	f := &FakeICMP{Reply: func(r EchoRequest) (EchoReply, error) {
		n++
		if n == 3 {
			return EchoReply{Status: StatusSuccess, From: r.Dest, RTT: 80 * time.Microsecond}, nil
		}
		return EchoReply{Status: StatusTimedOut}, nil
	}}
	if got, err := TimerOverhead(context.Background(), f, 5); err != nil || got != 80*time.Microsecond {
		t.Errorf("got %v, %v", got, err)
	}
	none := &FakeICMP{Reply: func(EchoRequest) (EchoReply, error) { return EchoReply{}, fmt.Errorf("%w: x", ErrLocal) }}
	if _, err := TimerOverhead(context.Background(), none, 3); !errors.Is(err, ErrLocal) {
		t.Errorf("want error wrapping the cause, got %v", err)
	}
}

func TestTimerOverhead_StopsOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	f := &FakeICMP{Reply: func(r EchoRequest) (EchoReply, error) {
		cancel()
		return EchoReply{Status: StatusSuccess, From: r.Dest, RTT: time.Microsecond}, nil
	}}
	if _, err := TimerOverhead(ctx, f, 10); !errors.Is(err, context.Canceled) || len(f.Requests) != 1 {
		t.Errorf("err %v after %d requests", err, len(f.Requests))
	}
}
