package engine

import (
	"log/slog"
	"net"
	"net/netip"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/config"
	"netdiag/internal/probe"
	"netdiag/internal/sysapi"
)

// Deps are the OS-facing parts the real probes are built from; tests swap
// them for fakes (SPEC §5, CLAUDE.md "inject everything that varies").
type Deps struct {
	Clock     clock.Clock
	ICMP      probe.ICMP
	Dialer    probe.Dialer
	Resolver  probe.Resolver   // system resolver (everything but DNS-01)
	DNSSystem probe.DNSQuerier // DNS-01: one adapter DNS server, past the cache
	DNSPublic probe.DNSQuerier // DNS-02: a public resolver over UDP 53
	ARP       sysapi.ARP       // LAN-02
	CPU       *sysapi.CPUMeter // SYS-01
	Namer     *probe.Namer     // TRC-01 reverse DNS; nil: created when trace.reverse_dns is set
	Logger    *slog.Logger     // optional

	// Build, if set, is asked first for the probe of a series; tests use
	// it to run scripted probes through the real wiring. false: fall back
	// to the real probe.
	Build func(sp Spec) (probe.Probe, bool)
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// BuildProbe returns the real probe of a series. false means the series
// has no probe in this build (Teams, Meet, media and source-port checks
// come with Phase 8; TRC-01 is the tracer, not a series probe).
func BuildProbe(cfg *config.Config, p sysapi.Primary, sp Spec, d Deps) (probe.Probe, bool) {
	to := cfg.Timeouts
	switch sp.Probe {
	case "SYS-01":
		return &probe.CPUProbe{Meter: d.CPU}, true
	case "LAN-01":
		return probe.ICMPProbe{ICMP: d.ICMP, Dest: p.Gateway, Timeout: ms(to.ICMP)}, true
	case "LAN-02":
		return probe.ARPProbe{ARP: d.ARP, Gateway: p.Gateway, IfIndex: p.Adapter.Index, Timeout: ms(to.ARP)}, true
	case "NET-01":
		return probe.ICMPProbe{ICMP: d.ICMP, Dest: netip.MustParseAddr(sp.Target), Timeout: ms(to.ICMP)}, true
	case "NET-04":
		return probe.TCPProbe{Dialer: d.Dialer, Resolver: d.Resolver, Addr: sp.Target, Timeout: ms(to.TCP)}, true
	case "RT-01":
		return probe.STUNProbe{Resolver: d.Resolver, Server: sp.Target, Timeout: ms(to.STUN), SkipUnresolved: true}, true
	case "NAT-01":
		return probe.NATProbe{Resolver: d.Resolver, Servers: cfg.Targets.STUN, Timeout: ms(to.STUN)}, true
	case "DNS-01":
		return probe.DNSProbe{Q: d.DNSSystem, Server: netip.MustParseAddr(sp.Target), Name: cfg.Targets.DNSTestName, Timeout: ms(to.DNS)}, true
	case "DNS-02":
		return probe.DNSProbe{Q: d.DNSPublic, Server: netip.MustParseAddr(sp.Target), Name: cfg.Targets.DNSTestName, Timeout: ms(to.DNS)}, true
	case "HTTP-01":
		for _, h := range cfg.Targets.HTTPCheck {
			if h.URL == sp.Target {
				return probe.HTTPProbe{URL: h.URL, ExpectStatus: h.ExpectStatus, ExpectBody: h.ExpectBody, Timeout: ms(to.HTTP)}, true
			}
		}
		return nil, false
	case "SVC-01":
		return probe.TLSProbe{Resolver: d.Resolver, Host: sp.Target, Timeout: ms(to.TLS)}, true
	case "MTU-01":
		return probe.MTUProbe{ICMP: d.ICMP, Resolver: d.Resolver, Host: sp.Target, Timeout: ms(to.ICMP)}, true
	case "CUST-ICMP":
		return probe.HostICMPProbe{ICMP: d.ICMP, Resolver: d.customResolver(), Host: sp.Target, Timeout: ms(to.ICMP)}, true
	case "CUST-TCP":
		return probe.TCPProbe{Dialer: d.Dialer, Resolver: d.customResolver(), Addr: sp.Target, Timeout: ms(to.TCP)}, true
	}
	return nil, false
}

// customResolver re-resolves custom target names once per 60 s (SPEC §4.2).
func (d Deps) customResolver() probe.Resolver {
	return &probe.CachedResolver{Inner: d.Resolver, Clock: d.Clock}
}

// Runnable returns the series that have a probe, with their probes.
func Runnable(cfg *config.Config, p sysapi.Primary, specs []Spec, d Deps) ([]Spec, []probe.Probe) {
	var outSpecs []Spec
	var outProbes []probe.Probe
	for _, sp := range specs {
		var pr probe.Probe
		ok := false
		if d.Build != nil {
			pr, ok = d.Build(sp)
		}
		if !ok {
			pr, ok = BuildProbe(cfg, p, sp, d)
		}
		if ok {
			outSpecs = append(outSpecs, sp)
			outProbes = append(outProbes, pr)
		}
	}
	return outSpecs, outProbes
}

// DefaultDialer is the plain dialer of the TCP probes.
func DefaultDialer() probe.Dialer { return &net.Dialer{} }
