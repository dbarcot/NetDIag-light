// Package verdict derives the primary verdict, the info flags and the call
// quality status from gate states and per-series statistics (SPEC §8).
package verdict

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"

	"netdiag/internal/config"
	"netdiag/internal/gate"
	"netdiag/internal/probe"
	"netdiag/internal/stats"
	"netdiag/internal/sysapi"
)

// Verdict is a primary verdict.
type Verdict string

// Verdicts in priority order (SPEC §8), plus CHECKING.
const (
	OK                Verdict = "OK"
	Checking          Verdict = "CHECKING"
	NoLink            Verdict = "NO_LINK"
	APIPA             Verdict = "APIPA"
	NoAddress         Verdict = "NO_ADDRESS"
	GWUnreachable     Verdict = "GW_UNREACHABLE"
	CaptivePortal     Verdict = "CAPTIVE_PORTAL"
	ISPDown           Verdict = "ISP_DOWN"
	DNSLocalFail      Verdict = "DNS_LOCAL_FAIL"
	DNSFail           Verdict = "DNS_FAIL"
	UDPBlocked        Verdict = "UDP_BLOCKED"
	TeamsUDPBlocked   Verdict = "TEAMS_UDP_BLOCKED"
	TeamsPartialPorts Verdict = "TEAMS_PARTIAL_PORTS"
	MeetUDPBlocked    Verdict = "MEET_UDP_BLOCKED"
	MTUIssue          Verdict = "MTU_ISSUE"
	LANLoss           Verdict = "LAN_LOSS"
	ISPLoss           Verdict = "ISP_LOSS"
	QualityPoorVoIP   Verdict = "QUALITY_POOR_VOIP"
)

// Priority lists the verdicts with exit codes in priority order.
var Priority = []Verdict{
	NoLink, APIPA, NoAddress, GWUnreachable, CaptivePortal, ISPDown, DNSLocalFail, DNSFail,
	UDPBlocked, TeamsUDPBlocked, TeamsPartialPorts, MeetUDPBlocked, MTUIssue, LANLoss, ISPLoss, QualityPoorVoIP,
}

var exitCodes = map[Verdict]int{
	OK: 0, NoLink: 10, APIPA: 11, NoAddress: 12, GWUnreachable: 13, CaptivePortal: 14, ISPDown: 15,
	DNSLocalFail: 16, DNSFail: 17, UDPBlocked: 18, TeamsUDPBlocked: 19, TeamsPartialPorts: 20,
	MeetUDPBlocked: 21, MTUIssue: 22, LANLoss: 23, ISPLoss: 24, QualityPoorVoIP: 25,
}

// ExitCode returns the process exit code of a verdict; CHECKING has none
// of its own and maps to 1 (SPEC §8.1).
func (v Verdict) ExitCode() int {
	if c, ok := exitCodes[v]; ok {
		return c
	}
	return 1
}

// Rank orders verdicts for "worst of the run": lower is worse; OK is last
// and CHECKING ranks after OK (it is ignored for the worst verdict).
func (v Verdict) Rank() int {
	if i := slices.Index(Priority, v); i >= 0 {
		return i
	}
	if v == OK {
		return len(Priority)
	}
	return len(Priority) + 1
}

// Info flags (SPEC §8), in display order.
const (
	FlagGWICMPBlocked         = "GW_ICMP_BLOCKED"
	FlagGWICMPRateLimited     = "GW_ICMP_RATE_LIMITED"
	FlagHTTPCheckFiltered     = "HTTP_CHECK_FILTERED" // ":<host>" appended
	FlagICMPBlocked           = "ICMP_BLOCKED"
	FlagSymmetricNAT          = "SYMMETRIC_NAT"
	FlagWiFiWeak              = "WIFI_WEAK"
	FlagWiFi24GHz             = "WIFI_24GHZ"
	FlagCPUHigh               = "CPU_HIGH"
	FlagTeamsSrcPortsBlocked  = "TEAMS_SRC_PORTS_BLOCKED"
	FlagTeamsDisabled         = "TEAMS_DISABLED"
	FlagTeamsRelayUnreachable = "TEAMS_RELAY_UNREACHABLE"
	FlagServiceUnreachable    = "SERVICE_UNREACHABLE" // ":<host>" appended
	FlagCustomTargetDown      = "CUSTOM_TARGET_DOWN"  // ":<name>" appended
	FlagCSVWriteError         = "CSV_WRITE_ERROR"
)

// Call quality status texts (SPEC §8).
const (
	QualityMeasuring = "Measuring…"
	QualityGood      = "Good"
	QualityFair      = "Fair"
	QualityPoor      = "Poor"
)

// SeriesData is what the verdict needs from one series.
type SeriesData struct {
	ID, Probe, Target string
	Name              string // custom target name (CUST-*)
	Window            stats.Summary
	// Recent holds the newest non-skipped samples, oldest first.
	Recent []probe.Sample
	// TrailingFails is the number of newest consecutive failures in the
	// window (an ongoing run, not yet followed by a success).
	TrailingFails int
}

// SettledLoss is the window loss without the trailing run of failures: an
// ongoing run is either the start of an outage (the gates decide) or
// becomes loss once a success follows. It keeps the first seconds of a
// gateway outage from reading as LAN_LOSS.
func (s SeriesData) SettledLoss() (lossPct float64, sent int) {
	sent = s.Window.Sent - s.TrailingFails
	return stats.LossPct(s.Window.OK, s.Window.Failed-s.TrailingFails), sent
}

// TrailingFails counts the newest consecutive failures, ignoring skipped
// and recovery samples (window ordered oldest first).
func TrailingFails(window []probe.Sample) int {
	n := 0
	for i := len(window) - 1; i >= 0; i-- {
		if window[i].Recovery {
			continue
		}
		switch window[i].Result {
		case probe.Fail:
			n++
		case probe.OK:
			return n
		}
	}
	return n
}

// Failing reports whether the last n non-skipped samples all failed.
func (s SeriesData) Failing(n int) bool {
	if len(s.Recent) < n || n <= 0 {
		return false
	}
	for _, sm := range s.Recent[len(s.Recent)-n:] {
		if sm.Result != probe.Fail {
			return false
		}
	}
	return true
}

// Last returns the newest non-skipped sample.
func (s SeriesData) Last() (probe.Sample, bool) {
	if len(s.Recent) == 0 {
		return probe.Sample{}, false
	}
	return s.Recent[len(s.Recent)-1], true
}

// LastOK reports whether the newest non-skipped sample is ok.
func (s SeriesData) LastOK() bool {
	l, ok := s.Last()
	return ok && l.Result == probe.OK
}

// LastFail reports whether the newest non-skipped sample failed.
func (s SeriesData) LastFail() bool {
	l, ok := s.Last()
	return ok && l.Result == probe.Fail
}

// Input is everything the verdict is computed from.
type Input struct {
	Gates   [gate.L5 + 1]gate.Status
	Address sysapi.AddressState
	Series  []SeriesData
	// WiFi is the primary adapter's Wi-Fi status; nil when it is not Wi-Fi.
	WiFi     *sysapi.WiFiStatus
	CPUAvg10 float64 // average CPU % over the last 10 s
	HasCPU   bool
	// TeamsEnabled is false when teams.relays is empty.
	TeamsEnabled bool
	CSVError     bool
	// QualitySources are call-quality series IDs in preference order
	// (MEDIA-01 Teams, MEDIA-01 Meet, NET-01 to 1.1.1.1).
	QualitySources []string
	Thresholds     config.Thresholds
	MinSamples     int
	FailCount      int // consecutive failures for "blocked" checks (gate.fail_count)
}

// CallQuality is the call-quality line (console) and section (web).
type CallQuality struct {
	Source    string // series ID; "" while no source has data
	HasData   bool   // the source has at least MinSamples results
	MOS       float64
	RTTms     float64 // average RTT in the window
	JitterMs  float64
	HasJitter bool
	LossPct   float64
	Status    string
}

// Result is the evaluated state.
type Result struct {
	Verdict  Verdict
	ExitCode int
	Detail   string // e.g. "3480 video" for TEAMS_PARTIAL_PORTS
	Flags    []string
	Quality  CallQuality
}

// teamsPortFeature names a Teams port (SPEC §8).
func teamsPortFeature(port string) string {
	switch port {
	case "3478":
		return "general"
	case "3479":
		return "audio"
	case "3480":
		return "video"
	case "3481":
		return "sharing"
	default:
		return ""
	}
}

type eval struct {
	in  Input
	by  map[string][]SeriesData // by probe ID
	ids map[string]SeriesData
}

func (e *eval) probe(id string) []SeriesData { return e.by[id] }

func (e *eval) open(l gate.Layer) bool { return e.in.Gates[l].State == gate.Open }

// all reports whether every series of a probe satisfies f (false when
// the probe has no series).
func all(list []SeriesData, f func(SeriesData) bool) bool {
	if len(list) == 0 {
		return false
	}
	for _, s := range list {
		if !f(s) {
			return false
		}
	}
	return true
}

func anyOf(list []SeriesData, f func(SeriesData) bool) bool {
	for _, s := range list {
		if f(s) {
			return true
		}
	}
	return false
}

// Evaluate computes the verdict, flags and call quality.
func Evaluate(in Input) Result {
	e := &eval{in: in, by: map[string][]SeriesData{}, ids: map[string]SeriesData{}}
	sorted := append([]SeriesData(nil), in.Series...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, s := range sorted {
		e.by[s.Probe] = append(e.by[s.Probe], s)
		e.ids[s.ID] = s
	}
	fc := in.FailCount
	failing := func(s SeriesData) bool { return s.Failing(fc) }
	lastOK := func(s SeriesData) bool { return s.LastOK() }

	net04OK := anyOf(e.probe("NET-04"), lastOK)
	// internet traffic through the gateway works right now (not merely an
	// L4 gate still open during its hysteresis)
	internetNow := e.open(gate.L4) && (anyOf(e.probe("NET-01"), lastOK) || net04OK || anyOf(e.probe("RT-01"), lastOK))
	gwICMPBlocked := e.open(gate.L3) && all(e.probe("LAN-01"), failing) &&
		(anyOf(e.probe("LAN-02"), lastOK) || internetNow)
	icmpBlocked := all(e.probe("NET-01"), failing) && net04OK
	relayUnreachable := in.TeamsEnabled && e.relayUnreachable(failing, lastOK)

	r := Result{}
	r.Quality = e.quality(relayUnreachable)
	lanLoss := e.lanLoss(gwICMPBlocked)
	r.Verdict, r.Detail = e.primary(failing, lastOK, net04OK, icmpBlocked, relayUnreachable, lanLoss, r.Quality)
	r.ExitCode = r.Verdict.ExitCode()
	r.Flags = e.flags(failing, gwICMPBlocked, lanLoss == lanLossRateLimited, icmpBlocked, relayUnreachable)
	return r
}

// relayUnreachable: the Teams test relay itself is dead, not the network
// (SPEC §8.2).
func (e *eval) relayUnreachable(failing, lastOK func(SeriesData) bool) bool {
	otherUDPWorks := anyOf(e.probe("MEET-01"), lastOK) || anyOf(e.probe("RT-01"), lastOK)
	if !e.open(gate.L4) || !otherUDPWorks {
		return false
	}
	teams := append(append([]SeriesData(nil), e.probe("TEAMS-01")...), e.probe("TEAMS-02")...)
	dnsFail := anyOf(teams, func(s SeriesData) bool {
		l, ok := s.Last()
		return ok && l.Result == probe.Fail && l.ErrClass == probe.ClassDNS
	})
	return dnsFail || (all(e.probe("TEAMS-01"), failing) && all(e.probe("TEAMS-02"), failing))
}

// lossy: enough settled samples and settled loss above the threshold.
func (e *eval) lossy(s SeriesData) bool {
	loss, sent := s.SettledLoss()
	return sent >= e.in.MinSamples && loss > e.in.Thresholds.LossPct
}

// lanLossKind classifies LAN-01 loss (SPEC §8).
type lanLossKind int

const (
	lanLossNone        lanLossKind = iota
	lanLossReal                    // LAN_LOSS
	lanLossRateLimited             // GW_ICMP_RATE_LIMITED
)

// lanLoss decides whether lossy gateway pings are real LAN loss. Home
// routers often drop pings to themselves under load while forwarding fine
// (like mtr intermediate-hop loss), so the loss counts only when every
// NET-01 target also shows it: settled loss ≥ half the LAN-01 loss, or
// above the threshold. A NET-01 target without enough settled samples
// leaves the question open (neither verdict nor flag).
func (e *eval) lanLoss(gwICMPBlocked bool) lanLossKind {
	if gwICMPBlocked {
		return lanLossNone // a gateway that ignores ping is not lossy
	}
	lanPct := -1.0
	for _, s := range e.probe("LAN-01") {
		if e.lossy(s) {
			loss, _ := s.SettledLoss()
			lanPct = max(lanPct, loss)
		}
	}
	net := e.probe("NET-01")
	if lanPct < 0 || len(net) == 0 {
		return lanLossNone
	}
	for _, s := range net {
		if _, sent := s.SettledLoss(); sent < e.in.MinSamples {
			return lanLossNone
		}
	}
	if all(net, func(s SeriesData) bool {
		loss, _ := s.SettledLoss()
		return loss >= lanPct/2 || loss > e.in.Thresholds.LossPct
	}) {
		return lanLossReal
	}
	return lanLossRateLimited
}

func (e *eval) primary(failing, lastOK func(SeriesData) bool, net04OK, icmpBlocked, relayUnreachable bool, lanLoss lanLossKind, q CallQuality) (Verdict, string) {
	g := e.in.Gates
	// Root cause = lowest closed gate; CHECKING while L1–L4 are undecided.
	// A captive portal blocks the internet and often DNS, so once L1–L3
	// are open it wins over ISP_DOWN and the DNS verdicts (SPEC §8).
	for _, l := range gate.Layers {
		if l == gate.L4 && e.captive() {
			return CaptivePortal, ""
		}
		switch g[l].State {
		case gate.Closed:
			switch l {
			case gate.L1:
				return NoLink, g[l].Reason
			case gate.L2:
				if e.in.Address == sysapi.AddressAPIPA {
					return APIPA, g[l].Reason
				}
				return NoAddress, g[l].Reason
			case gate.L3:
				return GWUnreachable, g[l].Reason
			case gate.L4:
				return ISPDown, g[l].Reason
			case gate.L5:
				if anyOf(e.probe("DNS-02"), lastOK) {
					return DNSLocalFail, g[l].Reason
				}
				return DNSFail, g[l].Reason
			}
		case gate.Unknown:
			if l <= gate.L4 {
				return Checking, ""
			}
		}
	}

	if all(e.probe("RT-01"), failing) && net04OK {
		return UDPBlocked, ""
	}
	if e.in.TeamsEnabled && !relayUnreachable {
		teams01 := e.probe("TEAMS-01")
		if all(teams01, failing) && anyOf(e.probe("TEAMS-02"), lastOK) {
			return TeamsUDPBlocked, ""
		}
		var bad []string
		for _, s := range teams01 {
			if failing(s) {
				port := s.Target[strings.LastIndexByte(s.Target, ':')+1:]
				if f := teamsPortFeature(port); f != "" {
					port += " " + f
				}
				bad = append(bad, port)
			}
		}
		if len(bad) > 0 && len(bad) < len(teams01) {
			return TeamsPartialPorts, strings.Join(bad, ", ")
		}
	}
	if all(e.probe("MEET-01"), failing) {
		svcMeet := anyOf(e.probe("SVC-01"), func(s SeriesData) bool { return s.Target == "meet.google.com" && s.LastOK() })
		if svcMeet || net04OK {
			return MeetUDPBlocked, ""
		}
	}
	th := e.in.Thresholds
	for _, s := range e.probe("MTU-01") {
		if l, ok := s.Last(); ok && l.Result == probe.OK && l.Value > 0 && int(l.Value) < th.MTUMin {
			return MTUIssue, fmt.Sprintf("path MTU %d to %s", int(l.Value), s.Target)
		}
	}
	lossy := e.lossy
	lan := e.probe("LAN-01")
	if lanLoss == lanLossReal {
		return LANLoss, ""
	}
	lanClean := all(lan, func(s SeriesData) bool {
		_, sent := s.SettledLoss()
		return s.Window.Sent == 0 || (sent >= e.in.MinSamples && !lossy(s))
	}) || len(lan) == 0
	if !icmpBlocked && lanClean && all(e.probe("NET-01"), lossy) {
		return ISPLoss, ""
	}
	if q.Status == QualityPoor {
		return QualityPoorVoIP, q.Source
	}
	return OK, ""
}

func isCaptive(s SeriesData) bool {
	l, ok := s.Last()
	return ok && l.ErrClass == probe.ClassCaptive
}

// captive reports a captive portal: the newest result of at least one
// HTTP-01 check is a redirect or a status/body mismatch and no check's
// newest result is ok. One check redirected while another passes is a web
// filter, not a portal (SPEC §8, HTTP_CHECK_FILTERED).
func (e *eval) captive() bool {
	return anyOf(e.probe("HTTP-01"), isCaptive) && !anyOf(e.probe("HTTP-01"), SeriesData.LastOK)
}

// urlHost returns the host of an HTTP check URL (the whole string if it
// does not parse).
func urlHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return raw
}

// quality picks the call-quality source (SPEC §5 MEDIA-01 note) and rates
// it. A Teams source is not used while the Teams relay is unreachable.
func (e *eval) quality(relayUnreachable bool) CallQuality {
	var src SeriesData
	found := false
	for _, id := range e.in.QualitySources {
		s, ok := e.ids[id]
		if !ok || (relayUnreachable && s.Probe == "MEDIA-01" && strings.HasSuffix(id, ":teams")) {
			continue
		}
		if s.Window.Sent >= e.in.MinSamples {
			src, found = s, true
			break
		}
	}
	if !found {
		return CallQuality{Status: QualityMeasuring}
	}
	w := src.Window
	jitter := 0.0
	if w.HasJitter {
		jitter = w.Jitter
	}
	q := CallQuality{
		Source: src.ID, HasData: true, RTTms: w.Avg, JitterMs: jitter, HasJitter: w.HasJitter, LossPct: w.LossPct,
		MOS: stats.MOS(w.Avg, jitter, w.LossPct),
	}
	th := e.in.Thresholds
	exceeded := (w.HasJitter && w.Jitter > th.JitterMs) || (w.HasRTT && w.Avg > th.RTTMs) || !w.HasRTT
	switch {
	case q.MOS < th.MOSMin || exceeded:
		q.Status = QualityPoor
	case q.MOS >= th.MOSGood:
		q.Status = QualityGood
	default:
		q.Status = QualityFair
	}
	return q
}

func (e *eval) flags(failing func(SeriesData) bool, gwICMPBlocked, gwRateLimited, icmpBlocked, relayUnreachable bool) []string {
	var f []string
	add := func(cond bool, flag string) {
		if cond {
			f = append(f, flag)
		}
	}
	th := e.in.Thresholds
	add(gwICMPBlocked, FlagGWICMPBlocked)
	add(gwRateLimited, FlagGWICMPRateLimited)
	if http := e.probe("HTTP-01"); anyOf(http, SeriesData.LastOK) {
		for _, s := range http {
			add(isCaptive(s), FlagHTTPCheckFiltered+":"+urlHost(s.Target))
		}
	}
	add(icmpBlocked, FlagICMPBlocked)
	add(anyOf(e.probe("NAT-01"), func(s SeriesData) bool {
		l, ok := s.Last()
		return ok && l.Result == probe.OK && l.Value == 1
	}), FlagSymmetricNAT)
	if w := e.in.WiFi; w != nil && w.HasDetails() {
		add(w.SignalPct != nil && *w.SignalPct < th.WiFiSignalPct, FlagWiFiWeak)
		add(w.Is24GHz(), FlagWiFi24GHz)
	}
	add(e.in.HasCPU && e.in.CPUAvg10 >= float64(th.CPUHighPct), FlagCPUHigh)
	// a dead test relay says nothing about the source ports
	add(e.in.TeamsEnabled && !relayUnreachable && anyOf(e.probe("SRC-01"), SeriesData.LastFail), FlagTeamsSrcPortsBlocked)
	add(!e.in.TeamsEnabled, FlagTeamsDisabled)
	add(relayUnreachable, FlagTeamsRelayUnreachable)
	if e.open(gate.L5) {
		for _, s := range e.probe("SVC-01") {
			add(s.LastFail(), FlagServiceUnreachable+":"+s.Target)
		}
	}
	for _, p := range []string{"CUST-ICMP", "CUST-TCP"} {
		for _, s := range e.probe(p) {
			add(failing(s), FlagCustomTargetDown+":"+s.Name)
		}
	}
	add(e.in.CSVError, FlagCSVWriteError)
	return f
}
