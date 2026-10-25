package config

import (
	"net"
	"net/netip"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"unicode"

	"netdiag/internal/sysapi"
)

const (
	maxRateSeconds = 3600
	minTimeoutMs   = 100
	maxTimeoutMs   = 10000
	maxNameLen     = 40
)

// Validate checks every value and returns a *ValidationError naming each
// offending key path, or nil.
func (c *Config) Validate() error {
	var v errList
	if c.Version != SchemaVersion {
		v.add("version", "unsupported schema version %d (this build supports %d)", c.Version, SchemaVersion)
	}
	intRange(&v, "ui.port", c.UI.Port, 1, 65535)
	// Checks relative to another key run only when that key is valid, so
	// one mistake is reported once.
	if intRange(&v, "window.seconds", c.Window.Seconds, 10, 3600) {
		intRange(&v, "window.min_samples", c.Window.MinSamples, 1, c.Window.Seconds)
	}
	intRange(&v, "history.minutes", c.History.Minutes, 1, 120)
	intRange(&v, "gate.fail_count", c.Gate.FailCount, 1, 100)
	intRange(&v, "gate.ok_count", c.Gate.OKCount, 1, 100)

	c.validateTargets(&v)

	if len(c.Teams.Relays) > 0 && len(c.Teams.UDPPorts) == 0 {
		v.add("teams.udp_ports", "must not be empty while teams.relays is set")
	}
	for i, h := range c.Teams.Relays {
		host(&v, indexPath("teams.relays", i), h)
	}
	for i, p := range c.Teams.UDPPorts {
		intRange(&v, indexPath("teams.udp_ports", i), p, 1, 65535)
	}
	for i, s := range c.Meet.STUN {
		hostPort(&v, indexPath("meet.stun", i), s)
	}

	forEachInt(c.Rates, func(key string, val int) {
		intRange(&v, "rates."+key, val, 1, maxRateSeconds)
	})
	forEachInt(c.Timeouts, func(key string, val int) {
		intRange(&v, "timeouts."+key, val, minTimeoutMs, maxTimeoutMs)
	})

	t := c.Thresholds
	floatRange(&v, "thresholds.loss_pct", t.LossPct, 0, 100)
	floatPositive(&v, "thresholds.rtt_ms", t.RTTMs)
	floatPositive(&v, "thresholds.jitter_ms", t.JitterMs)
	if floatRange(&v, "thresholds.mos_min", t.MOSMin, 1, 4.5) {
		floatRange(&v, "thresholds.mos_good", t.MOSGood, t.MOSMin, 4.5)
	}
	intRange(&v, "thresholds.mtu_min", t.MTUMin, 576, 1500)
	intRange(&v, "thresholds.wifi_signal_pct", t.WiFiSignalPct, 0, 100)
	if intRange(&v, "thresholds.cpu_moderate_pct", t.CPUModeratePct, 1, 98) &&
		intRange(&v, "thresholds.cpu_high_pct", t.CPUHighPct, t.CPUModeratePct+1, 99) {
		intRange(&v, "thresholds.cpu_heavy_pct", t.CPUHeavyPct, t.CPUHighPct+1, 100)
	}

	intRange(&v, "trace.max_hops", c.Trace.MaxHops, 1, 64)
	intRange(&v, "trace.interval_ms", c.Trace.IntervalMs, 200, 10000) // 5 packets/s cap (CLAUDE.md)

	s := c.CSV
	if s.Separator != ";" && s.Separator != "," {
		v.add("csv.separator", `must be ";" or ","`)
	}
	switch {
	case s.Decimal != "," && s.Decimal != ".":
		v.add("csv.decimal", `must be "," or "."`)
	case s.Decimal == s.Separator:
		v.add("csv.decimal", "must differ from csv.separator (%q)", s.Separator)
	}
	intRange(&v, "csv.summary_seconds", s.SummarySeconds, 1, 86400)
	if s.Rotate != "daily" && s.Rotate != "none" {
		v.add("csv.rotate", `must be "daily" or "none"`)
	}
	intRange(&v, "csv.max_mb", s.MaxMB, 1, 100000)
	return v.err()
}

func (c *Config) validateTargets(v *errList) {
	t := c.Targets
	names := map[string]string{} // name → path of first use
	checkName := func(path, name string) {
		switch {
		case name == "":
			v.add(path, "required")
		case len(name) > maxNameLen:
			v.add(path, "must be at most %d characters", maxNameLen)
		case strings.IndexFunc(name, unicode.IsControl) >= 0:
			v.add(path, "must not contain control characters")
		case names[name] != "":
			v.add(path, "duplicate name %q (also %s)", name, names[name])
		default:
			names[name] = path
		}
	}
	for i, p := range t.Ping {
		base := indexPath("targets.ping", i)
		checkName(base+".name", p.Name)
		host(v, base+".host", p.Host)
		intRange(v, base+".rate", p.Rate, 1, maxRateSeconds)
		dependsOn(v, base+".depends_on", p.DependsOn)
	}
	for i, p := range t.TCP {
		base := indexPath("targets.tcp", i)
		checkName(base+".name", p.Name)
		hostPort(v, base+".host", p.Host)
		intRange(v, base+".rate", p.Rate, 1, maxRateSeconds)
		dependsOn(v, base+".depends_on", p.DependsOn)
	}
	if intRange(v, "limits.max_custom_targets", c.Limits.MaxCustomTargets, 0, 100) {
		if n := len(t.Ping) + len(t.TCP); n > c.Limits.MaxCustomTargets {
			v.add("targets", "%d custom targets (ping + tcp) exceed limits.max_custom_targets (%d)", n, c.Limits.MaxCustomTargets)
		}
	}

	// Core targets that open L4 must not need DNS (SPEC §6), so they are IPv4 literals.
	nonEmpty(v, "targets.internet_icmp", len(t.InternetICMP))
	for i, s := range t.InternetICMP {
		ipv4(v, indexPath("targets.internet_icmp", i), s)
	}
	ipv4Port(v, "targets.internet_tcp", t.InternetTCP)
	nonEmpty(v, "targets.public_dns", len(t.PublicDNS))
	for i, s := range t.PublicDNS {
		ipv4(v, indexPath("targets.public_dns", i), s)
	}
	hostname(v, "targets.dns_test_name", t.DNSTestName)
	nonEmpty(v, "targets.stun", len(t.STUN))
	for i, s := range t.STUN {
		hostPort(v, indexPath("targets.stun", i), s)
	}
	for i, h := range t.HTTPCheck {
		base := indexPath("targets.http_check", i)
		httpURL(v, base+".url", h.URL)
		intRange(v, base+".expect_status", h.ExpectStatus, 100, 599)
	}
	for i, s := range t.Services {
		hostname(v, indexPath("targets.services", i), s)
	}
	host(v, "targets.trace", t.Trace)
}

// normalize fills values derived from other keys after validation.
func (c *Config) normalize() {
	for i := range c.Targets.Ping {
		p := &c.Targets.Ping[i]
		if p.DependsOn == "" {
			p.DependsOn = InferDependsOn(p.Host)
		}
	}
	for i := range c.Targets.TCP {
		p := &c.Targets.TCP[i]
		if p.DependsOn == "" {
			h, _, _ := net.SplitHostPort(p.Host)
			p.DependsOn = InferDependsOn(h)
		}
	}
}

// InferDependsOn returns the gate a custom target depends on when
// depends_on is omitted (SPEC §4.2): private IP → lan, public IP or
// hostname → internet.
func InferDependsOn(host string) string {
	if a, err := netip.ParseAddr(host); err == nil && sysapi.IsLocalIPv4(a) {
		return DependsLAN
	}
	return DependsInternet
}

// IsHostname reports whether host is a hostname rather than an IP literal,
// i.e. whether reaching it also needs DNS.
func IsHostname(host string) bool {
	_, err := netip.ParseAddr(host)
	return err != nil
}

// forEachInt calls fn with the JSON key and value of every int field of a
// flat struct such as Rates or Timeouts.
func forEachInt(s any, fn func(key string, val int)) {
	rv := reflect.ValueOf(s)
	for i := range rv.NumField() {
		fn(jsonName(rv.Type().Field(i)), int(rv.Field(i).Int()))
	}
}

// intRange and floatRange report whether val is valid.
func intRange(v *errList, path string, val, lo, hi int) bool {
	if val < lo || val > hi {
		v.add(path, "must be between %d and %d, got %d", lo, hi, val)
		return false
	}
	return true
}

func floatRange(v *errList, path string, val, lo, hi float64) bool {
	if val < lo || val > hi {
		v.add(path, "must be between %s and %s, got %s", fmtFloat(lo), fmtFloat(hi), fmtFloat(val))
		return false
	}
	return true
}

func floatPositive(v *errList, path string, val float64) {
	if val <= 0 {
		v.add(path, "must be greater than 0, got %s", fmtFloat(val))
	}
}

func fmtFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func nonEmpty(v *errList, path string, n int) {
	if n == 0 {
		v.add(path, "must not be empty")
	}
}

func dependsOn(v *errList, path, val string) {
	if val != "" && val != DependsLAN && val != DependsInternet {
		v.add(path, `must be "lan" or "internet", got %q`, val)
	}
}

func ipv4(v *errList, path, s string) {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() {
		v.add(path, "must be an IPv4 address, got %q", s)
	}
}

func ipv4Port(v *errList, path, s string) {
	h, p, err := net.SplitHostPort(s)
	if err != nil {
		v.add(path, "must be ip:port, got %q", s)
		return
	}
	ipv4(v, path, h)
	port(v, path, p)
}

// host accepts an IPv4 address or a hostname.
func host(v *errList, path, s string) {
	switch {
	case s == "":
		v.add(path, "required")
	case strings.Contains(s, ":"):
		if a, err := netip.ParseAddr(s); err == nil && a.Is6() {
			v.add(path, "IPv6 is not supported, got %q", s)
		} else {
			v.add(path, "must be an IPv4 address or hostname without port, got %q", s)
		}
	case isIPv4Literal(s), validHostname(s):
	default:
		v.add(path, "must be an IPv4 address or hostname, got %q", s)
	}
}

func hostPort(v *errList, path, s string) {
	if s == "" {
		v.add(path, "required")
		return
	}
	h, p, err := net.SplitHostPort(s)
	if err != nil {
		v.add(path, "must be host:port, got %q", s)
		return
	}
	host(v, path, h)
	port(v, path, p)
}

func port(v *errList, path, p string) {
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		v.add(path, "port must be between 1 and 65535, got %q", p)
	}
}

func hostname(v *errList, path, s string) {
	if !validHostname(s) {
		v.add(path, "must be a hostname, got %q", s)
	}
}

func httpURL(v *errList, path, s string) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		v.add(path, "must be an absolute http:// or https:// URL, got %q", s)
	}
}

func isIPv4Literal(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is4()
}

// validHostname checks RFC 1123 syntax. A purely numeric last label is
// rejected so malformed IPs like "1.1.1" are not taken as hostnames.
func validHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	labels := strings.Split(s, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, r := range l {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	last := labels[len(labels)-1]
	return strings.IndexFunc(last, func(r rune) bool { return r < '0' || r > '9' }) >= 0
}
