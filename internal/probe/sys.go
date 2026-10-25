package probe

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"

	"netdiag/internal/sysapi"
)

// ARPProbe is LAN-02: the gateway's MAC through ResolveIpNetEntry2. Only a
// Reachable or Permanent neighbor entry is ok; a cached Probe/Delay/Stale
// entry is class unconfirmed and counts as a failure for the gates
// (SPEC §5 LAN-02).
type ARPProbe struct {
	ARP     sysapi.ARP
	Gateway netip.Addr
	IfIndex uint32
	Timeout time.Duration
}

func (p ARPProbe) Run(ctx context.Context) Sample {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	elapsed := stopwatch()
	r, err := p.ARP.Resolve(ctx, p.Gateway, p.IfIndex)
	rtt := elapsed()
	switch {
	case errors.Is(err, sysapi.ErrARPBusy):
		return Sample{Result: Fail, ErrClass: ClassOther, Detail: err.Error()}
	case errors.Is(err, sysapi.ErrUnresolved):
		return Sample{Result: Fail, ErrClass: ClassUnreachable, Detail: "ARP " + r.State.String()}
	case errors.Is(err, context.DeadlineExceeded):
		return Sample{Result: Fail, ErrClass: ClassTimeout, Detail: "timeout"}
	case err != nil:
		return Sample{Result: Fail, ErrClass: ClassLocal, Detail: err.Error()}
	}
	detail := r.MAC.String() + " (" + r.State.String() + ")"
	if r.Outcome() != sysapi.ARPResolved {
		return Sample{Result: Fail, ErrClass: ClassUnconfirmed, Detail: "ARP " + r.State.String() + ", MAC " + r.MAC.String()}
	}
	return Sample{Result: OK, RTT: rtt, Detail: detail}
}

// CPUProbe is SYS-01: total CPU usage in percent (Sample.Value) from the
// delta of GetSystemTimes. The first run has no delta yet and is skipped.
type CPUProbe struct {
	mu    sync.Mutex
	Meter *sysapi.CPUMeter
}

func (p *CPUProbe) Run(context.Context) Sample {
	p.mu.Lock()
	defer p.mu.Unlock()
	pct, ok, err := p.Meter.Sample()
	switch {
	case err != nil:
		return Sample{Result: Fail, ErrClass: ClassLocal, Detail: err.Error()}
	case !ok:
		return SkippedSample("measuring")
	}
	return Sample{Result: OK, Value: pct}
}
