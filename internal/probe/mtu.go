package probe

import (
	"context"
	"fmt"
	"net/netip"
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

// MTUProbe finds the path MTU to Host with DF echo requests and a binary
// search between 576 and 1500 (MTU-01). Sample.Value is the MTU. A reply
// means the size fits; IP_PACKET_TOO_BIG, or a timeout that repeats once,
// means it does not. A local buffer error is a bug and fails the probe as
// local_error: it is never read as an MTU result.
type MTUProbe struct {
	ICMP     ICMP
	Resolver Resolver
	Host     string
	Timeout  time.Duration // per step
}

func (p MTUProbe) Run(ctx context.Context) Sample {
	ips, err := p.Resolver.LookupIPv4(ctx, p.Host)
	if err == nil && len(ips) == 0 {
		err = noAddress(p.Host)
	}
	if err != nil {
		return resolveFailure(p.Host, err)
	}
	dest := ips[0]
	cancelled := func() Sample {
		return Sample{Result: Fail, ErrClass: ClassTimeout, From: dest, Detail: "interrupted"}
	}

	fits, fail := p.step(ctx, dest, MaxMTU)
	if fail != nil {
		return *fail
	}
	if fits {
		return mtuSample(MaxMTU, dest)
	}
	// is the host answering at all at the smallest size?
	if fits, fail = p.step(ctx, dest, MinMTU); fail != nil {
		return *fail
	}
	if !fits {
		return Sample{Result: Fail, ErrClass: ClassTimeout, From: dest,
			Detail: fmt.Sprintf("no reply to a %d-byte packet with DF", MinMTU)}
	}
	lo, hi := MinMTU, MaxMTU // lo fits, hi does not
	for hi-lo > 1 {
		if ctx.Err() != nil {
			return cancelled()
		}
		mid := (lo + hi) / 2
		if fits, fail = p.step(ctx, dest, mid); fail != nil {
			return *fail
		}
		if fits {
			lo = mid
		} else {
			hi = mid
		}
	}
	return mtuSample(lo, dest)
}

func mtuSample(mtu int, dest netip.Addr) Sample {
	return Sample{Result: OK, Value: float64(mtu), From: dest, Detail: fmt.Sprintf("path MTU %d", mtu)}
}

// step sends one DF echo of a packet of mtu bytes and reports whether it
// fits. A non-nil sample is a failure that ends the search (local error,
// unreachable, ...).
func (p MTUProbe) step(ctx context.Context, dest netip.Addr, mtu int) (bool, *Sample) {
	req := EchoRequest{Dest: dest, DF: true, Size: mtu - ipv4ICMPOverhead, Timeout: p.Timeout}
	for attempt := range 2 {
		r, err := p.ICMP.Echo(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				return false, nil // interrupted: the caller notices ctx
			}
			return false, &Sample{Result: Fail, ErrClass: ClassLocal, Detail: err.Error(), From: dest}
		}
		switch r.Status {
		case StatusSuccess:
			if r.From == dest {
				return true, nil
			}
			return false, &Sample{Result: Fail, ErrClass: ClassOther, From: r.From,
				Detail: fmt.Sprintf("echo reply from %s instead of %s", r.From, dest)}
		case StatusPacketTooBig:
			return false, nil
		case StatusTimedOut:
			// a router that drops oversized DF packets silently looks like a
			// timeout; one retry tells it from a lost packet
			if attempt == 0 && ctx.Err() == nil {
				continue
			}
			return false, nil
		default:
			s := EchoSample(dest, r, nil)
			return false, &s
		}
	}
	return false, nil
}
