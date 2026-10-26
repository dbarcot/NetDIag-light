package console

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"netdiag/internal/config"
	"netdiag/internal/engine"
	"netdiag/internal/probe"
	"netdiag/internal/stats"
	"netdiag/internal/store"
	"netdiag/internal/sysapi"
	"netdiag/internal/verdict"
)

// SparkSeconds is the sparkline span: one character per second (SPEC §9).
const SparkSeconds = 60

// Meta is the run information shown in the header.
type Meta struct {
	Version      string
	ConfigSource string // config file, "built-in defaults" or "simulation <name>"
	WebURL       string // "" while the web UI is off
}

// CellKind is the state of one sparkline second.
type CellKind uint8

const (
	CellEmpty CellKind = iota // no sample, a skipped sample or a recovery sample
	CellOK
	CellLost
)

// Cell is one second of a sparkline.
type Cell struct {
	Kind CellKind
	RTT  float64 // ms, CellOK only
}

// Row is one line of the targets table.
type Row struct {
	Name, Host string
	Stats      stats.Summary
	// Skipped: the newest sample was skipped; Reason says why.
	Skipped bool
	Reason  string
	// IsMTU rows show the path MTU (newest ok result, 0 = none yet)
	// instead of RTT columns.
	IsMTU bool
	MTU   int
	Spark []Cell // SparkSeconds cells, oldest first; nil for MTU rows
}

// HopRow is one line of the hops table.
type HopRow struct {
	TTL   int
	Hosts string // every IP seen at this TTL, with its reverse DNS name
	Stats stats.Summary
	Final bool
	// DimLoss: loss here does not continue to the final hop (router ICMP
	// rate limiting); shown dimmed and ignored by verdicts (SPEC §7).
	DimLoss bool
}

// View is everything one console frame shows, derived from a snapshot.
type View struct {
	Meta
	Time   time.Time
	Result verdict.Result

	Adapter, LocalIP, Gateway, GatewayMAC, DNS, PublicIP string
	WiFi                                                 string // "" when the primary adapter is not Wi-Fi
	CPU                                                  string

	// QualityLabel names the call-quality source ("" while measuring).
	QualityLabel string
	QualitySpark []Cell
	Teams        string // the Teams/Meet line

	Rows        []Row
	TraceTarget string
	Hops        []HopRow

	RTTThreshold float64 // thresholds.rtt_ms (sparkline colour)
	Sparkline    bool    // console.sparkline
	Keys         bool    // key handling is active (footer hint)
	Stopping     bool    // a stop was requested (footer "Stopping…")
}

// BuildView derives a frame from a snapshot and the samples in st.
func BuildView(snap engine.Snapshot, st *store.Store, cfg *config.Config, meta Meta) View {
	v := View{
		Meta: meta, Time: snap.Time, Result: snap.Result,
		RTTThreshold: cfg.Thresholds.RTTMs, Sparkline: cfg.Console.Sparkline,
		TraceTarget: cfg.Targets.Trace,
	}
	p := snap.Primary
	v.Adapter, v.LocalIP, v.Gateway, v.DNS = "none", "-", "none", "-"
	if p.Found() {
		v.Adapter = p.Adapter.Name
		if len(p.Adapter.IPv4) > 0 {
			v.LocalIP = p.Adapter.IPv4[0].Addr().String()
		}
		var dns []string
		for _, d := range p.Adapter.DNSServers {
			dns = append(dns, d.String())
		}
		if len(dns) > 0 {
			v.DNS = strings.Join(dns, ", ")
		}
	}
	switch {
	case p.OnLink:
		v.Gateway = "on-link"
	case p.Gateway.IsValid():
		v.Gateway = p.Gateway.String()
	}
	v.GatewayMAC, v.PublicIP = gatewayMAC(snap, st), publicIP(snap, st)
	v.WiFi = wifiText(snap)
	v.CPU = cpuText(snap, cfg.Thresholds)

	history := func(id string) []probe.Sample { return st.History(id) }
	q := snap.Result.Quality
	if q.Source != "" {
		v.QualityLabel = qualityLabel(q.Source)
		v.QualitySpark = spark(history(q.Source), snap.Time)
	}
	v.Teams = teamsText(snap, st, cfg)

	for _, sp := range snap.Specs {
		if sp.Probe == "SYS-01" || sp.Probe == "TRC-01" {
			continue
		}
		v.Rows = append(v.Rows, buildRow(sp, history(sp.ID), st.Stats(sp.ID), snap.Time))
	}
	v.Hops = buildHops(snap.Hops, st)
	sanitize(&v)
	return v
}

// sanitize passes every string of the view that is not one of our own
// constants through clean: SSIDs, reverse DNS names, adapter names, config
// values and probe error details may hold control or bidi characters.
func sanitize(v *View) {
	for _, p := range []*string{
		&v.Version, &v.ConfigSource, &v.WebURL,
		&v.Adapter, &v.LocalIP, &v.Gateway, &v.GatewayMAC, &v.DNS, &v.PublicIP, &v.WiFi, &v.CPU,
		&v.QualityLabel, &v.Teams, &v.TraceTarget, &v.Result.Detail,
	} {
		*p = clean(*p)
	}
	v.Result.Verdict = verdict.Verdict(clean(string(v.Result.Verdict)))
	flags := make([]string, len(v.Result.Flags))
	for i, f := range v.Result.Flags {
		flags[i] = clean(f)
	}
	v.Result.Flags = flags
	for i := range v.Rows {
		r := &v.Rows[i]
		r.Name, r.Host, r.Reason = clean(r.Name), clean(r.Host), clean(r.Reason)
	}
	for i := range v.Hops {
		v.Hops[i].Hosts = clean(v.Hops[i].Hosts)
	}
}

func buildRow(sp engine.Spec, h []probe.Sample, s stats.Summary, now time.Time) Row {
	r := Row{Name: sp.Probe, Host: sp.Target, Stats: s}
	if sp.Name != "" {
		r.Name = sp.Name
	}
	if r.Host == "" {
		r.Host = "-"
	}
	if n := len(h); n > 0 && h[n-1].Result == probe.Skipped {
		r.Skipped, r.Reason = true, strings.TrimPrefix(h[n-1].Detail, "skipped: ")
	}
	if sp.Probe == "MTU-01" {
		r.IsMTU = true
		for i := len(h) - 1; i >= 0; i-- {
			if h[i].Result == probe.OK {
				r.MTU = int(h[i].Value)
				break
			}
		}
		return r
	}
	r.Spark = spark(h, now)
	return r
}

// spark turns the last SparkSeconds of samples into cells, newest on the
// right: the cell of a sample is its age in whole seconds counted from
// the right edge; the newest sample of a second wins.
func spark(h []probe.Sample, now time.Time) []Cell {
	cells := make([]Cell, SparkSeconds)
	for _, s := range h {
		age := now.Sub(s.Time)
		if age < 0 || age >= SparkSeconds*time.Second {
			continue
		}
		i := SparkSeconds - 1 - int(age/time.Second)
		switch {
		case s.Recovery || s.Result == probe.Skipped:
			cells[i] = Cell{}
		case s.Result == probe.OK:
			cells[i] = Cell{Kind: CellOK, RTT: s.RTTms()}
		default:
			cells[i] = Cell{Kind: CellLost}
		}
	}
	return cells
}

func buildHops(hops []probe.Hop, st *store.Store) []HopRow {
	out := make([]HopRow, 0, len(hops))
	finalLoss, haveFinal := 0.0, false
	for _, h := range hops {
		r := HopRow{TTL: h.TTL, Final: h.Final, Stats: st.Stats(probe.HopSeriesID(h.TTL))}
		var hosts []string
		for i, ip := range h.IPs {
			s := ip.String()
			if i < len(h.Names) && h.Names[i] != "" {
				s += " (" + strings.TrimSuffix(h.Names[i], ".") + ")"
			}
			hosts = append(hosts, s)
		}
		r.Hosts = strings.Join(hosts, " ")
		if r.Hosts == "" {
			r.Hosts = "???"
		}
		if h.Final && !haveFinal {
			finalLoss, haveFinal = r.Stats.LossPct, true
		}
		out = append(out, r)
	}
	for i := range out {
		r := &out[i]
		r.DimLoss = !r.Final && r.Stats.LossPct > 0 && (!haveFinal || finalLoss < r.Stats.LossPct)
	}
	return out
}

// gatewayMAC is the MAC of the newest confirmed LAN-02 result.
func gatewayMAC(snap engine.Snapshot, st *store.Store) string {
	for _, sp := range snap.Specs {
		if sp.Probe != "LAN-02" {
			continue
		}
		h := st.History(sp.ID)
		for i := len(h) - 1; i >= 0; i-- {
			if h[i].Result == probe.OK {
				mac, _, _ := strings.Cut(h[i].Detail, " ")
				if _, err := net.ParseMAC(mac); err == nil {
					return mac
				}
			}
		}
	}
	return ""
}

// publicIPMaxAge is how long a STUN answer stays good for the header: an
// older one may describe a network this machine has left.
const publicIPMaxAge = 120 * time.Second

// publicIP is the mapped address of the newest STUN answer (RT-01, NAT-01),
// "-" when there is none or it is older than publicIPMaxAge.
func publicIP(snap engine.Snapshot, st *store.Store) string {
	var best probe.Sample
	for _, sp := range snap.Specs {
		if sp.Probe != "RT-01" && sp.Probe != "NAT-01" {
			continue
		}
		h := st.History(sp.ID)
		for i := len(h) - 1; i >= 0; i-- {
			if h[i].Result == probe.OK && h[i].Mapped.IsValid() {
				if h[i].Time.After(best.Time) {
					best = h[i]
				}
				break
			}
		}
	}
	if !best.Mapped.IsValid() || snap.Time.Sub(best.Time) > publicIPMaxAge {
		return "-"
	}
	return best.Mapped.Addr().String()
}

func wifiText(snap engine.Snapshot) string {
	if snap.Primary.Adapter.Type != sysapi.AdapterWiFi {
		return ""
	}
	w := snap.WiFi
	if w == nil || !w.HasDetails() {
		return "details unavailable"
	}
	var parts []string
	switch {
	case w.SSID != "":
		parts = append(parts, w.SSID)
	case w.SSIDHidden:
		parts = append(parts, "SSID hidden (location services off)")
	}
	if w.SignalPct != nil {
		parts = append(parts, fmt.Sprintf("%d%%", *w.SignalPct))
	}
	if b := w.BandLabel(); b != "" {
		parts = append(parts, b)
	}
	return strings.Join(parts, " ")
}

// cpuText is the CPU load level with the 10 s average (SPEC §9 header).
func cpuText(snap engine.Snapshot, th config.Thresholds) string {
	if !snap.HasCPU {
		return "-"
	}
	pct := snap.CPUAvg10
	level := "Light"
	switch {
	case pct >= float64(th.CPUHeavyPct):
		level = "Heavy"
	case pct >= float64(th.CPUHighPct):
		level = "High"
	case pct >= float64(th.CPUModeratePct):
		level = "Moderate"
	}
	return fmt.Sprintf("%s %.0f%%", level, pct)
}

// qualityLabel names a call-quality source; the NET-01 fallback says so.
func qualityLabel(id string) string {
	switch id {
	case engine.MediaTeams:
		return "MEDIA-01 Teams"
	case engine.MediaMeet:
		return "MEDIA-01 Meet"
	}
	return strings.Replace(id, ":", " ", 1) + " (MEDIA-01 unavailable)"
}

// teamsText is the Teams/Meet line: per-port status, Meet status, MOS.
func teamsText(snap engine.Snapshot, st *store.Store, cfg *config.Config) string {
	flags := snap.Result.Flags
	var b strings.Builder
	switch {
	case len(cfg.Teams.Relays) == 0:
		b.WriteString("Teams: disabled (teams.relays empty)")
	case slices.Contains(flags, verdict.FlagTeamsRelayUnreachable):
		b.WriteString("Teams: Teams test relay not responding - result unknown")
	default:
		relay := cfg.Teams.Relays[0]
		b.WriteString("Teams:")
		for _, port := range cfg.Teams.UDPPorts {
			id := "TEAMS-01:" + net.JoinHostPort(relay, strconv.Itoa(port))
			fmt.Fprintf(&b, " UDP %d %s", port, seriesStatus(st.History(id)))
		}
		fmt.Fprintf(&b, ", TCP 443 %s", seriesStatus(st.History("TEAMS-02:"+net.JoinHostPort(relay, "443"))))
		fmt.Fprintf(&b, ", MOS %s", mosText(st, engine.MediaTeams, cfg.Window.MinSamples))
	}
	if len(cfg.Meet.STUN) == 0 {
		b.WriteString("   Meet: disabled")
		return b.String()
	}
	fmt.Fprintf(&b, "   Meet: %s, MOS %s", seriesStatus(st.History("MEET-01:"+cfg.Meet.STUN[0])),
		mosText(st, engine.MediaMeet, cfg.Window.MinSamples))
	return b.String()
}

// seriesStatus is "ok" or "FAIL" for the newest result, "skipped" when the
// newest sample was skipped, "-" without any.
func seriesStatus(h []probe.Sample) string {
	if len(h) == 0 {
		return "-"
	}
	if h[len(h)-1].Result == probe.Skipped {
		return "skipped"
	}
	for i := len(h) - 1; i >= 0; i-- {
		switch {
		case h[i].Recovery:
		case h[i].Result == probe.OK:
			return "ok"
		case h[i].Result == probe.Fail:
			return "FAIL"
		}
	}
	return "-"
}

// mosText is the MOS of a media series once it has min samples.
func mosText(st *store.Store, id string, minSamples int) string {
	s := st.Stats(id)
	if s.Sent < minSamples || !s.HasRTT {
		return "-"
	}
	j := 0.0
	if s.HasJitter {
		j = s.Jitter
	}
	return fmt.Sprintf("%.2f", stats.MOS(s.Avg, j, s.LossPct))
}
