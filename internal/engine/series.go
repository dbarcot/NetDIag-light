// Package engine ties the store, the gates and the verdict together: it
// builds the series catalog from the config and the primary adapter,
// answers the scheduler's gate questions, records samples and produces
// snapshots for the console, the web UI and the report.
package engine

import (
	"net"
	"net/netip"
	"strconv"
	"time"

	"netdiag/internal/config"
	"netdiag/internal/gate"
	"netdiag/internal/sysapi"
)

// Spec is one series: what it measures and how the gates treat it.
type Spec struct {
	gate.Series
	Probe  string // probe ID, e.g. "NET-01"
	Target string // display target: an IP, host:port, URL or name
	Name   string // custom target name (CUST-*)
}

func sec(n int) time.Duration { return time.Duration(n) * time.Second }

// isHostname reports whether a target (host or host:port) needs DNS.
func isHostname(target string) bool {
	h := target
	if host, _, err := net.SplitHostPort(target); err == nil {
		h = host
	}
	return config.IsHostname(h)
}

// requiresFor is the layer a probe to target needs: base, or L5 when the
// target is a hostname.
func requiresFor(base gate.Layer, target string) gate.Layer {
	if isHostname(target) {
		return gate.L5
	}
	return base
}

// onLink reports whether ip is inside one of the adapter's subnets.
func onLink(ip netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Masked().Contains(ip) {
			return true
		}
	}
	return false
}

// BuildSeries returns the series catalog (SPEC §5) for a config and the
// primary adapter. Probes that do not exist yet in this build are still
// listed so gates, verdicts and simulation cover them.
func BuildSeries(cfg *config.Config, p sysapi.Primary) []Spec {
	r, t := cfg.Rates, cfg.Targets
	var out []Spec
	add := func(id, probeID, target string, interval time.Duration, requires, defines gate.Layer) *Spec {
		out = append(out, Spec{
			Series: gate.Series{ID: id, Interval: interval, Requires: requires, Defines: defines},
			Probe:  probeID, Target: target,
		})
		return &out[len(out)-1]
	}

	add("SYS-01:cpu", "SYS-01", "cpu", sec(r.CPU), gate.None, gate.None)

	gw := ""
	if p.Gateway.IsValid() {
		gw = p.Gateway.String()
	}
	add("LAN-01:gateway", "LAN-01", gw, sec(r.LANPing), gate.L2, gate.L3).NeedsGateway = true
	add("LAN-02:gateway", "LAN-02", gw, sec(r.LANARP), gate.L2, gate.L3).NeedsGateway = true

	for _, ip := range t.InternetICMP {
		add("NET-01:"+ip, "NET-01", ip, sec(r.InternetICMP), gate.L3, gate.L4).L3Evidence = true
	}
	net04 := add("NET-04:"+t.InternetTCP, "NET-04", t.InternetTCP, sec(r.InternetTCP), gate.L3, gate.L4)
	net04.L3Evidence, net04.L3Recovery = true, true
	// RT-01 defines L4, so it cannot wait for DNS: an unresolved STUN name
	// is recorded as skipped by the probe (SPEC §6).
	for _, s := range t.STUN {
		add("RT-01:"+s, "RT-01", s, sec(r.STUN), gate.L3, gate.L4).L3Evidence = true
	}

	for _, ip := range p.Adapter.DNSServers {
		req := gate.L4
		if onLink(ip, p.Adapter.IPv4) {
			req = gate.L3
		}
		add("DNS-01:"+ip.String(), "DNS-01", ip.String(), sec(r.DNSSystem), req, gate.L5)
	}
	for _, ip := range t.PublicDNS {
		add("DNS-02:"+ip, "DNS-02", ip, sec(r.DNSPublic), gate.L4, gate.None)
	}
	add("NAT-01:stun", "NAT-01", "stun", sec(r.NAT), gate.L4, gate.None)

	// HTTP-01 needs only L3: a captive portal blocks almost everything
	// except DNS and HTTP to itself, so L4/L5 may well be closed. The probe
	// resolves its host itself; a DNS failure is a plain failure (SPEC §6).
	for _, h := range t.HTTPCheck {
		add("HTTP-01:"+h.URL, "HTTP-01", h.URL, sec(r.HTTP), gate.L3, gate.None)
	}
	for _, s := range t.Services {
		add("SVC-01:"+s, "SVC-01", s, sec(r.Services), gate.L5, gate.None)
	}
	add("MTU-01:gateway", "MTU-01", gw, sec(r.MTU), gate.L3, gate.None).NeedsGateway = true
	add("MTU-01:"+t.Trace, "MTU-01", t.Trace, sec(r.MTU), requiresFor(gate.L4, t.Trace), gate.None)

	if relays := cfg.Teams.Relays; len(relays) > 0 {
		for _, relay := range relays {
			req := requiresFor(gate.L4, relay)
			for _, port := range cfg.Teams.UDPPorts {
				hp := net.JoinHostPort(relay, strconv.Itoa(port))
				add("TEAMS-01:"+hp, "TEAMS-01", hp, sec(r.TeamsUDP), req, gate.None)
			}
			hp := net.JoinHostPort(relay, "443")
			add("TEAMS-02:"+hp, "TEAMS-02", hp, sec(r.TeamsTCP), req, gate.None)
			add("SRC-01:"+relay, "SRC-01", relay, sec(r.SrcPorts), req, gate.None)
		}
		add(MediaTeams, "MEDIA-01", relays[0], sec(r.Media), requiresFor(gate.L4, relays[0]), gate.None)
	}
	for _, s := range cfg.Meet.STUN {
		add("MEET-01:"+s, "MEET-01", s, sec(r.Meet), requiresFor(gate.L4, s), gate.None)
	}
	if len(cfg.Meet.STUN) > 0 {
		add(MediaMeet, "MEDIA-01", cfg.Meet.STUN[0], sec(r.Media), requiresFor(gate.L4, cfg.Meet.STUN[0]), gate.None)
	}

	for _, c := range t.Ping {
		s := add("CUST:"+c.Name, "CUST-ICMP", c.Host, sec(c.Rate), customRequires(c.DependsOn, c.Host), gate.None)
		s.Name = c.Name
	}
	for _, c := range t.TCP {
		s := add("CUST:"+c.Name, "CUST-TCP", c.Host, sec(c.Rate), customRequires(c.DependsOn, c.Host), gate.None)
		s.Name = c.Name
	}
	return out
}

// ProbeIDs lists every probe ID in the catalog.
var ProbeIDs = []string{
	"SYS-01", "LAN-01", "LAN-02", "NET-01", "NET-04", "RT-01", "DNS-01", "DNS-02", "NAT-01",
	"HTTP-01", "SVC-01", "MTU-01", "TEAMS-01", "TEAMS-02", "SRC-01", "MEET-01", "MEDIA-01",
	"CUST-ICMP", "CUST-TCP",
}

// Call-quality series IDs.
const (
	MediaTeams = "MEDIA-01:teams"
	MediaMeet  = "MEDIA-01:meet"
)

// QualitySources lists the call-quality sources in preference order (SPEC
// §5 MEDIA-01 note): Teams media, Meet media, NET-01 to the first
// internet target (1.1.1.1 by default).
func QualitySources(cfg *config.Config) []string {
	return []string{MediaTeams, MediaMeet, "NET-01:" + cfg.Targets.InternetICMP[0]}
}

func customRequires(dependsOn, host string) gate.Layer {
	base := gate.L4
	if dependsOn == config.DependsLAN {
		base = gate.L3
	}
	return requiresFor(base, host)
}
