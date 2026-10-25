package probe

import (
	"context"
	"net/netip"
	"time"
)

// Result is the outcome of one probe run.
type Result string

const (
	OK      Result = "ok"
	Fail    Result = "fail"
	Skipped Result = "skipped" // gate closed; never counted as loss
)

// Error classes recorded in Sample.ErrClass for failures.
const (
	ClassTimeout      = "timeout"
	ClassUnreachable  = "unreachable"
	ClassTTLExpired   = "ttl_expired"
	ClassPacketTooBig = "packet_too_big"
	ClassLocal        = "local_error" // a bug or OS problem on this machine, not the network
	ClassOther        = "error"
	ClassCaptive      = "captive"     // HTTP-01: redirect or wrong status/body (captive portal)
	ClassDNS          = "dns"         // the probe's target name did not resolve
	ClassUnconfirmed  = "unconfirmed" // LAN-02: only a stale/probe/delay ARP entry
)

// Sample is one immutable measurement. Probes fill the outcome fields; the
// scheduler stamps Time, Series, Probe and Target.
type Sample struct {
	Time     time.Time // start of the run (scheduler clock)
	Series   string    // series ID, e.g. "NET-01:1.1.1.1"
	Probe    string    // probe ID, e.g. "NET-01"
	Target   string
	Result   Result
	RTT      time.Duration // ok only
	Value    float64       // probe-specific value (CPU % for SYS-01)
	From     netip.Addr    // responder, e.g. a trace hop
	ErrClass string        // failures only
	Detail   string        // error detail or "skipped: <reason>"
	// Recovery marks a NET-04 run taken while L3 is not open (SPEC §6 L3
	// recovery): it is L3 evidence only, never L4 evidence, and it is left
	// out of loss and RTT statistics like a skipped sample.
	Recovery bool
}

// RTTms returns the RTT in milliseconds.
func (s Sample) RTTms() float64 { return float64(s.RTT) / float64(time.Millisecond) }

// Probe runs one measurement. Run must return within its own timeout and
// honour ctx cancellation where the OS call allows it.
type Probe interface {
	Run(ctx context.Context) Sample
}

// Func adapts a function to Probe.
type Func func(ctx context.Context) Sample

func (f Func) Run(ctx context.Context) Sample { return f(ctx) }

// SkippedSample returns a skipped sample with the reason in Detail.
func SkippedSample(reason string) Sample {
	return Sample{Result: Skipped, Detail: "skipped: " + reason}
}
