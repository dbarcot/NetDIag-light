package probe

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// ICMPStatus is an IP_STATUS value from IcmpSendEcho2 (ipexport.h).
type ICMPStatus uint32

const (
	StatusSuccess             ICMPStatus = 0
	StatusBufTooSmall         ICMPStatus = 11001
	StatusDestNetUnreachable  ICMPStatus = 11002
	StatusDestHostUnreachable ICMPStatus = 11003
	StatusDestProtUnreachable ICMPStatus = 11004
	StatusDestPortUnreachable ICMPStatus = 11005
	StatusPacketTooBig        ICMPStatus = 11009
	StatusTimedOut            ICMPStatus = 11010
	StatusBadRoute            ICMPStatus = 11012
	StatusTTLExpiredTransit   ICMPStatus = 11013
	StatusTTLExpiredReassem   ICMPStatus = 11014
	StatusGeneralFailure      ICMPStatus = 11050
)

func (s ICMPStatus) String() string {
	switch s {
	case StatusSuccess:
		return "success"
	case StatusBufTooSmall:
		return "buffer too small"
	case StatusDestNetUnreachable:
		return "destination net unreachable"
	case StatusDestHostUnreachable:
		return "destination host unreachable"
	case StatusDestProtUnreachable:
		return "destination protocol unreachable"
	case StatusDestPortUnreachable:
		return "destination port unreachable"
	case StatusPacketTooBig:
		return "packet too big"
	case StatusTimedOut:
		return "timed out"
	case StatusBadRoute:
		return "bad route"
	case StatusTTLExpiredTransit:
		return "TTL expired in transit"
	case StatusTTLExpiredReassem:
		return "TTL expired during reassembly"
	case StatusGeneralFailure:
		return "general failure"
	default:
		return fmt.Sprintf("IP status %d", uint32(s))
	}
}

// Class maps a non-success status to a Sample error class.
func (s ICMPStatus) Class() string {
	switch s {
	case StatusTimedOut:
		return ClassTimeout
	case StatusDestNetUnreachable, StatusDestHostUnreachable, StatusDestProtUnreachable,
		StatusDestPortUnreachable, StatusBadRoute:
		return ClassUnreachable
	case StatusTTLExpiredTransit, StatusTTLExpiredReassem:
		return ClassTTLExpired
	case StatusPacketTooBig:
		return ClassPacketTooBig
	case StatusBufTooSmall:
		return ClassLocal
	default:
		return ClassOther
	}
}

// DefaultPayloadSize is the ICMP payload of ordinary pings (SPEC §5).
const DefaultPayloadSize = 32

// DefaultTTL is the TTL of ordinary pings (SPEC §5).
const DefaultTTL = 128

// EchoRequest describes one ICMP echo.
type EchoRequest struct {
	Dest    netip.Addr
	TTL     uint8 // 0 = DefaultTTL
	DF      bool  // don't fragment
	Size    int   // payload bytes; 0 = DefaultPayloadSize
	Timeout time.Duration
}

// EchoReply is what came back. RTT is measured with the monotonic clock
// around the OS call; APIRTT is the API's whole-millisecond value, used
// only as a sanity check (SPEC §5 RTT note).
type EchoReply struct {
	Status ICMPStatus
	From   netip.Addr
	RTT    time.Duration
	APIRTT time.Duration
}

// ErrLocal marks failures on this machine (e.g. a reply buffer the API
// rejects). They are bugs, never network results.
var ErrLocal = errors.New("local ICMP error")

// ICMP sends one echo request. A returned error wraps ErrLocal; network
// outcomes (timeouts, unreachable, TTL expired) are statuses, not errors.
type ICMP interface {
	Echo(ctx context.Context, req EchoRequest) (EchoReply, error)
}

// ReplyBufferSize is the reply buffer IcmpSendEcho2 gets for a payload
// size (SPEC §5 note): room for the reply struct, the payload, 8 bytes of
// ICMP header and an ICMP error message; at least 256 bytes.
func ReplyBufferSize(payload, replyStructSize int) int {
	return max(replyStructSize+payload+8+32, 256)
}

// ICMPProbe is an ordinary ping probe (LAN-01, NET-01, custom ping).
type ICMPProbe struct {
	ICMP    ICMP
	Dest    netip.Addr
	Timeout time.Duration
}

func (p ICMPProbe) Run(ctx context.Context) Sample {
	r, err := p.ICMP.Echo(ctx, EchoRequest{Dest: p.Dest, Timeout: p.Timeout})
	return EchoSample(p.Dest, r, err)
}

// EchoSample classifies an echo result for a ping to dest: only an echo
// reply from dest itself is ok.
func EchoSample(dest netip.Addr, r EchoReply, err error) Sample {
	switch {
	case err != nil:
		return Sample{Result: Fail, ErrClass: ClassLocal, Detail: err.Error()}
	case r.Status == StatusSuccess && r.From == dest:
		return Sample{Result: OK, RTT: r.RTT, From: r.From}
	case r.Status == StatusSuccess:
		return Sample{Result: Fail, ErrClass: ClassOther, From: r.From,
			Detail: fmt.Sprintf("echo reply from %s instead of %s", r.From, dest)}
	default:
		return Sample{Result: Fail, ErrClass: r.Status.Class(), From: r.From, Detail: r.Status.String()}
	}
}

// Loopback is the timer-overhead baseline target.
var Loopback = netip.MustParseAddr("127.0.0.1")

// TimerOverhead pings 127.0.0.1 n times and returns the minimum RTT: the
// cost of the Windows call and scheduling, shown as "timer overhead"
// (SPEC §5). It is never subtracted from RTTs.
func TimerOverhead(ctx context.Context, icmp ICMP, n int) (time.Duration, error) {
	best := time.Duration(-1)
	var lastErr error
	for range n {
		r, err := icmp.Echo(ctx, EchoRequest{Dest: Loopback, Timeout: time.Second})
		switch {
		case err != nil:
			lastErr = err
		case r.Status != StatusSuccess:
			lastErr = fmt.Errorf("loopback ping: %s", r.Status)
		case best < 0 || r.RTT < best:
			best = r.RTT
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
	}
	if best < 0 {
		return 0, fmt.Errorf("timer overhead: no loopback reply: %w", lastErr)
	}
	return best, nil
}
