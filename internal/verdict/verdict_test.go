package verdict

import (
	"slices"
	"strings"
	"testing"
	"time"

	"netdiag/internal/config"
	"netdiag/internal/gate"
	"netdiag/internal/probe"
	"netdiag/internal/stats"
	"netdiag/internal/sysapi"
)

// series builds SeriesData from a result string: o ok, f fail (timeout),
// c captive, d dns failure. RTT is rtt ms for ok samples.
func series(id, results string, rtt float64) SeriesData {
	probeID, target, _ := strings.Cut(id, ":")
	var samples []probe.Sample
	for _, c := range results {
		s := probe.Sample{Series: id, Result: probe.OK, RTT: time.Duration(rtt * float64(time.Millisecond))}
		switch c {
		case 'f':
			s = probe.Sample{Series: id, Result: probe.Fail, ErrClass: probe.ClassTimeout}
		case 'c':
			s = probe.Sample{Series: id, Result: probe.Fail, ErrClass: probe.ClassCaptive}
		case 'd':
			s = probe.Sample{Series: id, Result: probe.Fail, ErrClass: probe.ClassDNS}
		}
		samples = append(samples, s)
	}
	return SeriesData{ID: id, Probe: probeID, Target: target, Window: stats.Summarize(samples), Recent: samples,
		TrailingFails: TrailingFails(samples)}
}

func TestTrailingFailsAndSettledLoss(t *testing.T) {
	t.Parallel()
	tests := []struct {
		results  string
		trailing int
		loss     float64
		sent     int
	}{
		{"oooo", 0, 0, 4},
		{"ooff", 2, 0, 2},  // ongoing run: not loss yet
		{"oofo", 0, 25, 4}, // a failure followed by a success is loss
		{"ffff", 4, 0, 0},
		{"", 0, 0, 0},
	}
	for _, tt := range tests {
		s := series("LAN-01:gateway", tt.results, 1)
		if s.TrailingFails != tt.trailing {
			t.Errorf("%q: trailing %d, want %d", tt.results, s.TrailingFails, tt.trailing)
		}
		if loss, sent := s.SettledLoss(); loss != tt.loss || sent != tt.sent {
			t.Errorf("%q: settled loss %v/%d, want %v/%d", tt.results, loss, sent, tt.loss, tt.sent)
		}
	}
	skipped := []probe.Sample{{Result: probe.OK}, {Result: probe.Fail}, {Result: probe.Skipped}, {Result: probe.Fail}}
	if n := TrailingFails(skipped); n != 2 {
		t.Errorf("skipped samples do not end a run: %d", n)
	}
}

// The first failures of a gateway outage are neither LAN loss nor an
// ICMP-blocking gateway while the gates are still in hysteresis.
func TestEvaluate_OutageOnsetIsNotLossOrICMPBlock(t *testing.T) {
	t.Parallel()
	h := healthy()
	in := with(h,
		series("LAN-01:gateway", "oooooooooooooooofff", 1), // 3 new failures in a 19-sample window
		series("LAN-02:gateway", "oof", 0),                 // ARP now stale
		series("NET-01:1.1.1.1", "oooooooooooooofff", 12),
		series("NET-01:8.8.8.8", "oooooooooooooofff", 13),
		series("NET-04:1.1.1.1:443", "oooooooooooooofff", 14),
		series("RT-01:stun.cloudflare.com:3478", "of", 0),
		series("RT-01:stun.l.google.com:19302", "of", 0))
	r := Evaluate(in) // L3 and L4 still open (hysteresis)
	if r.Verdict != OK || slices.Contains(r.Flags, FlagGWICMPBlocked) {
		t.Errorf("onset: %s %v, want OK without GW_ICMP_BLOCKED", r.Verdict, r.Flags)
	}
}

func value(id string, v float64) SeriesData {
	s := series(id, "o", 1)
	s.Recent[0].Value = v
	return s
}

func openGates() [gate.L5 + 1]gate.Status {
	var g [gate.L5 + 1]gate.Status
	for _, l := range gate.Layers {
		g[l] = gate.Status{State: gate.Open}
	}
	return g
}

const okRun = "oooooooooooo" // 12 ok results ≥ min_samples

// healthy is a fully working network.
func healthy() Input {
	th := config.Default().Thresholds
	return Input{
		Gates:   openGates(),
		Address: sysapi.AddressOK,
		Series: []SeriesData{
			series("LAN-01:gateway", okRun, 1),
			series("LAN-02:gateway", "ooo", 0),
			series("NET-01:1.1.1.1", okRun, 12),
			series("NET-01:8.8.8.8", okRun, 13),
			series("NET-04:1.1.1.1:443", okRun, 14),
			series("RT-01:stun.cloudflare.com:3478", "ooo", 15),
			series("RT-01:stun.l.google.com:19302", "ooo", 15),
			series("DNS-02:1.1.1.1", "ooo", 10),
			series("HTTP-01:http://www.msftconnecttest.com/connecttest.txt", "ooo", 20),
			series("TEAMS-01:relay:3478", "ooo", 20),
			series("TEAMS-02:relay:443", "ooo", 20),
			series("MEET-01:stun.l.google.com:19302", "ooo", 20),
			series("MEDIA-01:teams", okRun, 20),
			series("MEDIA-01:meet", okRun, 25),
			series("SVC-01:meet.google.com", "o", 30),
			value("MTU-01:gateway", 1500),
		},
		TeamsEnabled:   true,
		QualitySources: []string{"MEDIA-01:teams", "MEDIA-01:meet", "NET-01:1.1.1.1"},
		Thresholds:     th,
		MinSamples:     10,
		FailCount:      3,
	}
}

func with(in Input, repl ...SeriesData) Input {
	out := in
	out.Series = append([]SeriesData(nil), in.Series...)
	for _, r := range repl {
		replaced := false
		for i := range out.Series {
			if out.Series[i].ID == r.ID {
				out.Series[i], replaced = r, true
			}
		}
		if !replaced {
			out.Series = append(out.Series, r)
		}
	}
	return out
}

func gates(in Input, l gate.Layer, st gate.State) Input {
	in.Gates[l] = gate.Status{State: st}
	for above := l + 1; above <= gate.L5 && st != gate.Open; above++ {
		in.Gates[above] = gate.Status{State: gate.Unknown}
	}
	return in
}

func TestEvaluate_Healthy(t *testing.T) {
	t.Parallel()
	r := Evaluate(healthy())
	if r.Verdict != OK || r.ExitCode != 0 || len(r.Flags) != 0 {
		t.Errorf("got %+v", r)
	}
	if r.Quality.Source != "MEDIA-01:teams" || r.Quality.Status != QualityGood {
		t.Errorf("quality %+v", r.Quality)
	}
}

const httpCheck = "HTTP-01:http://www.msftconnecttest.com/connecttest.txt"

func TestEvaluate_GateVerdicts(t *testing.T) {
	t.Parallel()
	lan := healthy()
	captive := series(httpCheck, "oc", 0)
	tests := []struct {
		name string
		in   Input
		want Verdict
	}{
		{"L1 closed", gates(lan, gate.L1, gate.Closed), NoLink},
		{"L2 closed APIPA", func() Input { i := gates(lan, gate.L2, gate.Closed); i.Address = sysapi.AddressAPIPA; return i }(), APIPA},
		{"L2 closed no IPv4", func() Input { i := gates(lan, gate.L2, gate.Closed); i.Address = sysapi.AddressNone; return i }(), NoAddress},
		{"L3 closed", gates(lan, gate.L3, gate.Closed), GWUnreachable},
		{"L4 closed", gates(lan, gate.L4, gate.Closed), ISPDown},
		{"L5 closed, public DNS ok", gates(lan, gate.L5, gate.Closed), DNSLocalFail},
		{"L5 closed, public DNS fails", with(gates(lan, gate.L5, gate.Closed), series("DNS-02:1.1.1.1", "ooff", 0)), DNSFail},
		{"L3 unknown", gates(lan, gate.L3, gate.Unknown), Checking},
		{"L1 unknown", gates(lan, gate.L1, gate.Unknown), Checking},
		{"L5 unknown is not CHECKING", gates(lan, gate.L5, gate.Unknown), OK},
		// a captive portal usually blocks the internet and often DNS
		{"captive with L4 closed", with(gates(lan, gate.L4, gate.Closed), captive), CaptivePortal},
		{"captive with L4 unknown", with(gates(lan, gate.L4, gate.Unknown), captive), CaptivePortal},
		{"captive with L5 closed, public DNS ok", with(gates(lan, gate.L5, gate.Closed), captive), CaptivePortal},
		{"captive with L5 closed, public DNS fails", with(gates(lan, gate.L5, gate.Closed), captive, series("DNS-02:1.1.1.1", "ooff", 0)), CaptivePortal},
		{"L3 closed outranks captive", with(gates(lan, gate.L3, gate.Closed), captive), GWUnreachable},
		{"L3 unknown outranks captive", with(gates(lan, gate.L3, gate.Unknown), captive), Checking},
		{"HTTP-01 DNS failure is not captive", with(gates(lan, gate.L4, gate.Closed), series(httpCheck, "ddd", 0)), ISPDown},
		{"old captive result followed by ok", with(gates(lan, gate.L4, gate.Closed), series(httpCheck, "cco", 0)), ISPDown},
		{"lowest closed gate wins", func() Input {
			i := gates(lan, gate.L4, gate.Closed)
			i.Gates[gate.L3] = gate.Status{State: gate.Closed}
			return i
		}(), GWUnreachable},
	}
	for _, tt := range tests {
		if got := Evaluate(tt.in).Verdict; got != tt.want {
			t.Errorf("%s: %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestEvaluate_NetworkVerdicts(t *testing.T) {
	t.Parallel()
	h := healthy()
	lossy := "ooooooooofooooooooof" // 10 % loss
	tests := []struct {
		name   string
		in     Input
		want   Verdict
		detail string
	}{
		{"captive portal", with(h, series("HTTP-01:http://www.msftconnecttest.com/connecttest.txt", "c", 0)), CaptivePortal, ""},
		{"UDP blocked", with(h, series("RT-01:stun.cloudflare.com:3478", "fff", 0), series("RT-01:stun.l.google.com:19302", "fff", 0)), UDPBlocked, ""},
		{"one STUN server failing is not UDP blocked", with(h, series("RT-01:stun.cloudflare.com:3478", "fff", 0)), OK, ""},
		{"two failures are not yet blocked", with(h, series("RT-01:stun.cloudflare.com:3478", "off", 0), series("RT-01:stun.l.google.com:19302", "off", 0)), OK, ""},
		{"Teams UDP blocked", with(h, series("TEAMS-01:relay:3478", "fff", 0)), TeamsUDPBlocked, ""},
		{"Teams partial ports", with(h,
			series("TEAMS-01:relay:3478", "ooo", 20), series("TEAMS-01:relay:3479", "ooo", 20),
			series("TEAMS-01:relay:3480", "fff", 0), series("TEAMS-01:relay:3481", "ooo", 20)), TeamsPartialPorts, "3480 video"},
		{"Meet UDP blocked", with(h, series("MEET-01:stun.l.google.com:19302", "fff", 0)), MeetUDPBlocked, ""},
		{"MTU issue", with(h, value("MTU-01:1.1.1.1", 1380)), MTUIssue, "path MTU 1380 to 1.1.1.1"},
		{"MTU at the threshold is fine", with(h, value("MTU-01:1.1.1.1", 1400)), OK, ""},
		{"LAN loss", with(h, series("LAN-01:gateway", lossy, 1), series("NET-01:1.1.1.1", lossy, 12), series("NET-01:8.8.8.8", lossy, 13)), LANLoss, ""},
		{"LAN loss: internet loss at half the LAN loss", with(h, series("LAN-01:gateway", "oooofoooofoooofoooof", 1),
			series("NET-01:1.1.1.1", lossy, 12), series("NET-01:8.8.8.8", lossy, 13)), LANLoss, ""},
		{"lossy gateway pings with a clean internet are not LAN loss", with(h, series("LAN-01:gateway", lossy, 1)), OK, ""},
		{"ISP loss", with(h, series("NET-01:1.1.1.1", lossy, 12), series("NET-01:8.8.8.8", lossy, 13)), ISPLoss, ""},
		{"loss on one internet target is not ISP loss", with(h, series("NET-01:1.1.1.1", lossy, 12)), OK, ""},
		{"loss below min_samples is not judged", with(h, series("LAN-01:gateway", "oof", 1)), OK, ""},
		{"poor VoIP: high RTT", with(h, series("MEDIA-01:teams", okRun, 200)), QualityPoorVoIP, "MEDIA-01:teams"},
		// priorities
		{"UDP blocked outranks MTU", with(h, series("RT-01:stun.cloudflare.com:3478", "fff", 0), series("RT-01:stun.l.google.com:19302", "fff", 0), value("MTU-01:1.1.1.1", 1300)), UDPBlocked, ""},
		{"LAN loss outranks ISP loss", with(h, series("LAN-01:gateway", lossy, 1), series("NET-01:1.1.1.1", lossy, 12), series("NET-01:8.8.8.8", lossy, 13)), LANLoss, ""},
		{"captive outranks UDP blocked", with(h, series("HTTP-01:http://www.msftconnecttest.com/connecttest.txt", "c", 0), series("RT-01:stun.cloudflare.com:3478", "fff", 0), series("RT-01:stun.l.google.com:19302", "fff", 0)), CaptivePortal, ""},
		// exclusions
		{"ICMP-blocked gateway is not LAN loss", with(h, series("LAN-01:gateway", "ffffffffffff", 0)), OK, ""},
		{"ICMP-blocked internet is not ISP loss", with(h, series("NET-01:1.1.1.1", "ffffffffffff", 0), series("NET-01:8.8.8.8", "ffffffffffff", 0)), OK, ""},
	}
	for _, tt := range tests {
		r := Evaluate(tt.in)
		if r.Verdict != tt.want || r.Detail != tt.detail {
			t.Errorf("%s: %s %q, want %s %q", tt.name, r.Verdict, r.Detail, tt.want, tt.detail)
		}
		if r.ExitCode != tt.want.ExitCode() {
			t.Errorf("%s: exit code %d", tt.name, r.ExitCode)
		}
	}
}

func TestEvaluate_TeamsRelayUnreachable(t *testing.T) {
	t.Parallel()
	h := healthy()
	tests := []struct {
		name string
		in   Input
	}{
		{"UDP and TCP to the relay fail, Meet works", with(h, series("TEAMS-01:relay:3478", "fff", 0), series("TEAMS-02:relay:443", "fff", 0), series("MEDIA-01:teams", "ffffffffffff", 0))},
		{"relay name does not resolve", with(h, series("TEAMS-01:relay:3478", "d", 0), series("TEAMS-02:relay:443", "d", 0))},
	}
	for _, tt := range tests {
		r := Evaluate(tt.in)
		if r.Verdict != OK || !slices.Contains(r.Flags, FlagTeamsRelayUnreachable) {
			t.Errorf("%s: %s %v", tt.name, r.Verdict, r.Flags)
		}
		if r.Quality.Source == "MEDIA-01:teams" {
			t.Errorf("%s: Teams media must not be the quality source while the relay is unreachable", tt.name)
		}
	}
	// without any other working UDP path it is not the relay's fault
	r := Evaluate(with(h, series("TEAMS-01:relay:3478", "fff", 0), series("TEAMS-02:relay:443", "fff", 0),
		series("MEET-01:stun.l.google.com:19302", "fff", 0),
		series("RT-01:stun.cloudflare.com:3478", "fff", 0), series("RT-01:stun.l.google.com:19302", "fff", 0)))
	if slices.Contains(r.Flags, FlagTeamsRelayUnreachable) {
		t.Errorf("flags %v", r.Flags)
	}
}

func TestEvaluate_Flags(t *testing.T) {
	t.Parallel()
	h := healthy()
	weak, strong := 35, 80
	tests := []struct {
		name string
		in   Input
		flag string
	}{
		{"gateway ignores ping, ARP reachable", with(h, series("LAN-01:gateway", "fff", 0)), FlagGWICMPBlocked},
		{"internet ignores ping, TCP works", with(h, series("NET-01:1.1.1.1", "fff", 0), series("NET-01:8.8.8.8", "fff", 0)), FlagICMPBlocked},
		{"symmetric NAT", with(h, value("NAT-01", 1)), FlagSymmetricNAT},
		{"weak Wi-Fi", func() Input { i := h; i.WiFi = &sysapi.WiFiStatus{SignalPct: &weak}; return i }(), FlagWiFiWeak},
		{"2.4 GHz by frequency", func() Input {
			i := h
			i.WiFi = &sysapi.WiFiStatus{SignalPct: &strong, Band: sysapi.Band24GHz, BandSource: sysapi.BandFromFrequency}
			return i
		}(), FlagWiFi24GHz},
		{"CPU high", func() Input { i := h; i.CPUAvg10, i.HasCPU = 75, true; return i }(), FlagCPUHigh},
		{"Teams source ports", with(h, series("SRC-01:relay", "f", 0)), FlagTeamsSrcPortsBlocked},
		{"Teams disabled", func() Input { i := h; i.TeamsEnabled = false; return i }(), FlagTeamsDisabled},
		{"service unreachable", with(h, series("SVC-01:teams.microsoft.com", "f", 0)), "SERVICE_UNREACHABLE:teams.microsoft.com"},
		{"custom target down", with(h, func() SeriesData {
			s := series("CUST:fileserver", "fff", 0)
			s.Probe, s.Name = "CUST-ICMP", "fileserver"
			return s
		}()), "CUSTOM_TARGET_DOWN:fileserver"},
		{"CSV write error", func() Input { i := h; i.CSVError = true; return i }(), FlagCSVWriteError},
	}
	for _, tt := range tests {
		r := Evaluate(tt.in)
		if !slices.Contains(r.Flags, tt.flag) {
			t.Errorf("%s: flags %v lack %s", tt.name, r.Flags, tt.flag)
		}
		if r.Verdict != OK {
			t.Errorf("%s: info flags must not change the verdict, got %s", tt.name, r.Verdict)
		}
	}
}

func TestEvaluate_FlagsNotRaised(t *testing.T) {
	t.Parallel()
	h := healthy()
	strong := 80
	tests := []struct {
		name string
		in   Input
		flag string
	}{
		{"ping lost, no ARP and internet down: no GW_ICMP_BLOCKED",
			func() Input {
				i := with(h, series("LAN-01:gateway", "fff", 0), series("LAN-02:gateway", "fff", 0))
				i.Gates[gate.L4] = gate.Status{State: gate.Closed}
				return i
			}(), FlagGWICMPBlocked},
		{"2.4 GHz from the channel only is uncertain", func() Input {
			i := h
			i.WiFi = &sysapi.WiFiStatus{SignalPct: &strong, Band: sysapi.Band24GHz, BandSource: sysapi.BandFromChannelNo}
			return i
		}(), FlagWiFi24GHz},
		{"no Wi-Fi details at all", func() Input { i := h; i.WiFi = &sysapi.WiFiStatus{SSIDHidden: true}; return i }(), FlagWiFiWeak},
		{"CPU just below", func() Input { i := h; i.CPUAvg10, i.HasCPU = 74.9, true; return i }(), FlagCPUHigh},
		{"service failing while DNS is down", func() Input {
			i := with(h, series("SVC-01:teams.microsoft.com", "f", 0))
			i.Gates[gate.L5] = gate.Status{State: gate.Closed}
			return i
		}(), "SERVICE_UNREACHABLE:teams.microsoft.com"},
	}
	for _, tt := range tests {
		if r := Evaluate(tt.in); slices.Contains(r.Flags, tt.flag) {
			t.Errorf("%s: unexpected %s in %v", tt.name, tt.flag, r.Flags)
		}
	}
}

func TestEvaluate_CallQuality(t *testing.T) {
	t.Parallel()
	h := healthy()
	// no Teams data: Meet; no Meet: NET-01 to 1.1.1.1
	in := with(h, series("MEDIA-01:teams", "", 0))
	if q := Evaluate(in).Quality; q.Source != "MEDIA-01:meet" {
		t.Errorf("source %q, want Meet", q.Source)
	}
	in = with(in, series("MEDIA-01:meet", "ooo", 20)) // below min_samples
	if q := Evaluate(in).Quality; q.Source != "NET-01:1.1.1.1" || q.Status != QualityGood {
		t.Errorf("fallback %+v", q)
	}
	in = with(in, series("NET-01:1.1.1.1", "ooo", 12))
	if q := Evaluate(in).Quality; q.HasData || q.Status != QualityMeasuring {
		t.Errorf("measuring %+v", q)
	}

	// Fair: MOS between mos_min and mos_good. 100 ms → eff 110, R 90.45, MOS ≈ 4.34 (good);
	// make it fair with loss: 10 % loss → R 65.45 → MOS ≈ 3.39 < 3.6 → poor; 4 % → R 80.45, MOS ≈ 4.01… good.
	// 6 % loss on 20 ms: R = 92.45 − 15 = 77.45, MOS ≈ 3.89 → fair.
	fair := "oooooooooooooooofooooooooooooooofooooooooooooooofo" // 3 of 50 = 6 %
	if q := Evaluate(with(h, series("MEDIA-01:teams", fair, 20))).Quality; q.Status != QualityFair {
		t.Errorf("fair: %+v", q)
	}
	// unknown jitter never exceeds the threshold
	unknownJitter := series("MEDIA-01:teams", okRun, 20)
	unknownJitter.Window.Jitter, unknownJitter.Window.HasJitter = 500, false
	if q := Evaluate(with(h, unknownJitter)).Quality; q.HasJitter || q.Status != QualityGood {
		t.Errorf("unknown jitter: %+v", q)
	}
}

func TestExitCodesAndRank(t *testing.T) {
	t.Parallel()
	for i, v := range Priority {
		if v.ExitCode() != 10+i {
			t.Errorf("%s exit code %d, want %d", v, v.ExitCode(), 10+i)
		}
		if v.Rank() != i {
			t.Errorf("%s rank %d", v, v.Rank())
		}
	}
	if OK.ExitCode() != 0 || Checking.ExitCode() != 1 {
		t.Error("OK/CHECKING exit codes")
	}
	if NoLink.Rank() >= OK.Rank() || OK.Rank() >= Checking.Rank() {
		t.Error("rank order")
	}
}

// Gateway ping loss with a clean internet is ICMP rate limiting by the
// router, not LAN loss (SPEC §8).
func TestEvaluate_GatewayICMPRateLimited(t *testing.T) {
	t.Parallel()
	h := healthy()
	lan20 := "oooofoooofoooofoooof" // 20 % loss
	tests := []struct {
		name    string
		in      Input
		want    Verdict
		flagged bool
	}{
		{"clean internet", with(h, series("LAN-01:gateway", lan20, 1)), OK, true},
		{"internet loss below half and threshold", with(h, series("LAN-01:gateway", lan20, 1),
			series("NET-01:1.1.1.1", "ooooooooooooooooooooooooooooooooooooooof", 12), // 2.5 %: > threshold 2
			series("NET-01:8.8.8.8", okRun, 13)), OK, true},
		{"one internet target lossy, the other clean", with(h, series("LAN-01:gateway", lan20, 1),
			series("NET-01:1.1.1.1", "ooooofoooooo", 12)), OK, true},
		{"every internet target lossy", with(h, series("LAN-01:gateway", lan20, 1),
			series("NET-01:1.1.1.1", "ooooofoooooo", 12), series("NET-01:8.8.8.8", "ooooofoooooo", 13)), LANLoss, false},
		{"internet target without enough samples: undecided", with(h, series("LAN-01:gateway", lan20, 1),
			series("NET-01:1.1.1.1", "ooo", 12)), OK, false},
		{"gateway ignoring ping is not rate limited", with(h, series("LAN-01:gateway", "ffffffffffff", 1)), OK, false},
		{"clean gateway", h, OK, false},
	}
	for _, tt := range tests {
		r := Evaluate(tt.in)
		if r.Verdict != tt.want || slices.Contains(r.Flags, FlagGWICMPRateLimited) != tt.flagged {
			t.Errorf("%s: %s %v, want %s flag %v", tt.name, r.Verdict, r.Flags, tt.want, tt.flagged)
		}
	}
}
