package console

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/config"
	"netdiag/internal/engine"
	"netdiag/internal/probe"
	"netdiag/internal/stats"
	"netdiag/internal/store"
)

func ok(rtt float64) Cell { return Cell{Kind: CellOK, RTT: rtt} }

var lost, empty = Cell{Kind: CellLost}, Cell{}

func TestSparkline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		cells     []Cell
		threshold float64
		want      string // text
		wantSGR   []seg  // full segments when set
	}{
		{"flat series is the lowest block", []Cell{ok(20), ok(20), ok(20)}, 150, "▁▁▁", nil},
		{"single spike", []Cell{ok(10), ok(10), ok(90), ok(10)}, 150, "▁▁█▁", nil},
		{"scaled to the row's own range", []Cell{ok(100), ok(107), ok(114)}, 150, "▁▅█", nil},
		{"newest on the right, lost and skipped", []Cell{ok(1), lost, empty, ok(8)}, 150, "▁× █", nil},
		{"lost is red", []Cell{ok(1), lost}, 150, "", []seg{{"▁", ""}, {"×", sgrRed}}},
		{"above threshold is yellow", []Cell{ok(100), ok(140), ok(200), ok(300)}, 150, "",
			[]seg{{"▁▂", ""}, {"▅█", sgrYellow}}},
		{"all empty", []Cell{empty, empty}, 150, "  ", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sparkline(tt.cells, tt.threshold)
			if tt.wantSGR != nil {
				if len(got) != len(tt.wantSGR) {
					t.Fatalf("got %q, want %q", got, tt.wantSGR)
				}
				for i := range got {
					if got[i] != tt.wantSGR[i] {
						t.Errorf("segment %d: got %q, want %q", i, got[i], tt.wantSGR[i])
					}
				}
				return
			}
			if s := got.String(false); s != tt.want {
				t.Errorf("got %q, want %q", s, tt.want)
			}
		})
	}
}

func TestSpark_CellsFromSamples(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 1, 0, 0, time.UTC)
	at := func(ago int) time.Time { return now.Add(-time.Duration(ago) * time.Second) }
	h := []probe.Sample{
		{Time: at(60), Result: probe.OK, RTT: time.Millisecond}, // left the span
		{Time: at(59), Result: probe.OK, RTT: 5 * time.Millisecond},
		{Time: at(3), Result: probe.Fail},
		{Time: at(2), Result: probe.Skipped},
		{Time: at(1), Result: probe.OK, RTT: 9 * time.Millisecond, Recovery: true},
		{Time: at(0), Result: probe.OK, RTT: 7 * time.Millisecond},
	}
	c := spark(h, now)
	if len(c) != SparkSeconds {
		t.Fatalf("%d cells", len(c))
	}
	want := map[int]Cell{0: ok(5), 56: lost, 57: empty, 58: empty, 59: ok(7)}
	for i, w := range want {
		if c[i] != w {
			t.Errorf("cell %d = %+v, want %+v", i, c[i], w)
		}
	}
}

func TestTrunc(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in   string
		n    int
		want string
	}{{"abc", 3, "abc"}, {"abcd", 3, "ab…"}, {"abcd", 1, "…"}, {"abcd", 0, ""}, {"žluť", 4, "žluť"}} {
		if got := trunc(tt.in, tt.n); got != tt.want {
			t.Errorf("trunc(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
	l := line{{"Verdict: ", ""}, {"GW_UNREACHABLE", sgrBoldRd}}
	if got := l.fit(12); got.String(false) != "Verdict: GW…" || got.width() != 12 {
		t.Errorf("fit = %q", got.String(false))
	}
	if got := l.fit(12).String(true); got != "Verdict: "+sgrBoldRd+"GW"+sgrReset+sgrBoldRd+"…"+sgrReset {
		t.Errorf("fit keeps the style of the cut run: %q", got)
	}
}

// Long names and hosts are truncated, never wider than the console.
func TestRender_LongHostnamesNeverExceedWidth(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("very-long-label.", 12) + "example"
	v := View{
		Meta:    Meta{Version: "dev", ConfigSource: `C:\` + strings.Repeat(`deep\`, 30) + "netdiag.json"},
		Adapter: long, LocalIP: "10.0.0.2", Gateway: "10.0.0.1", DNS: long, PublicIP: "-", CPU: "-", WiFi: long,
		Teams: long, TraceTarget: long, Sparkline: true, RTTThreshold: 150,
		Rows: []Row{
			{Name: long, Host: long + ":443", Stats: stats.Summary{Sent: 3}, Spark: make([]Cell, SparkSeconds)},
			{Name: "x", Host: long, Skipped: true, Reason: long},
			{Name: "MTU-01", Host: long, IsMTU: true, MTU: 1500},
		},
		Hops: []HopRow{{TTL: 1, Hosts: long + " " + long}, {TTL: 30, Hosts: "???", Final: true}},
	}
	v.Result.Quality.HasData, v.Result.Quality.Status = true, "Good"
	v.QualityLabel = long
	for _, w := range []int{20, 40, 79, 80, 119, 199, 300} {
		for i, l := range Render(v, w, time.UTC) {
			if l.width() > w {
				t.Errorf("width %d: line %d is %d wide: %q", w, i, l.width(), l.String(false))
			}
		}
	}
}

func TestRender_SparklineColumnDroppedWhenNarrow(t *testing.T) {
	t.Parallel()
	v := viewAt(t, "ok", 30)
	header := func(w int) string {
		for _, l := range Render(v, w, time.UTC) {
			if s := l.String(false); strings.HasPrefix(s, "Name ") {
				return s
			}
		}
		t.Fatal("no table header")
		return ""
	}
	if !strings.Contains(header(200), "Last 60 s") {
		t.Error("sparkline column missing at width 200")
	}
	for _, w := range []int{80, 120} {
		if h := header(w); strings.Contains(h, "Last 60 s") || !strings.HasSuffix(h, "Jitter") {
			t.Errorf("width %d: header %q, want every stats column and no sparkline", w, h)
		}
	}
	v.Sparkline = false // console.sparkline off
	if strings.Contains(header(200), "Last 60 s") {
		t.Error("sparkline shown with console.sparkline off")
	}
}

func TestRender_SkippedRowsGreyWithReason(t *testing.T) {
	t.Parallel()
	v := viewAt(t, "gw_down", 30)
	found := false
	for _, l := range Render(v, 120, time.UTC) {
		s := l.String(true)
		if strings.HasPrefix(l.String(false), "NET-01   1.1.1.1 ") {
			found = true
			if !strings.HasPrefix(s, sgrGrey) || !strings.Contains(s, "skipped: gateway down") {
				t.Errorf("skipped row %q", s)
			}
		}
		if strings.HasPrefix(l.String(false), "NET-04 ") && strings.Contains(s, sgrGrey) {
			t.Errorf("measured row is grey: %q", s)
		}
	}
	if !found {
		t.Error("no NET-01 row")
	}
}

func TestRender_VerdictAndFlagsColoured(t *testing.T) {
	t.Parallel()
	text := renderText(viewAt(t, "teams_relay_unreachable", 30), 120, true)
	if !strings.Contains(text, "Verdict: "+sgrBoldGn+"OK"+sgrReset) || !strings.Contains(text, sgrYellow+"TEAMS_RELAY_UNREACHABLE"+sgrReset) {
		t.Errorf("verdict line:\n%s", text)
	}
	if !strings.Contains(text, "Teams: Teams test relay not responding - result unknown") {
		t.Errorf("Teams line:\n%s", text)
	}
	if strings.Contains(renderText(viewAt(t, "ok", 30), 120, false), "\x1b") {
		t.Error("escape sequences without colour")
	}
}

// The call quality line uses MEDIA-01 when present and falls back to
// NET-01 to 1.1.1.1 with a different label.
func TestRender_CallQualitySourceLabel(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"ok":                "[MEDIA-01 Teams]",
		"no_media_fallback": "[NET-01 1.1.1.1 (MEDIA-01 unavailable)]",
	} {
		text := renderText(viewAt(t, name, 30), 200, false)
		if !strings.Contains(text, "Call quality: Good  MOS ") || !strings.Contains(text, want) {
			t.Errorf("%s: want %s in\n%s", name, want, text)
		}
	}
	measuring := viewAt(t, "ok", 3)
	if text := renderText(measuring, 120, false); !strings.Contains(text, "Call quality: Measuring…\n") {
		t.Errorf("before min_samples:\n%s", text)
	}
}

func TestBuildHops_DimsLossThatDoesNotReachTheFinalHop(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st := store.New(clock.NewFake(start.Add(10*time.Second)), time.Minute, time.Minute)
	add := func(ttl int, results string) {
		for i, r := range results {
			s := probe.Sample{Series: probe.HopSeriesID(ttl), Time: start.Add(time.Duration(i) * time.Second), Result: probe.OK, RTT: time.Millisecond}
			if r == 'f' {
				s.Result = probe.Fail
			}
			st.Add(s)
		}
	}
	add(1, "oooo")
	add(2, "ofof") // rate-limited router: 50 %
	add(3, "ooof") // 25 %, same as the final hop: real loss continues
	add(4, "ooof")
	ip := func(s string) []netip.Addr { return []netip.Addr{netip.MustParseAddr(s)} }
	hops := buildHops([]probe.Hop{
		{TTL: 1, IPs: ip("10.0.0.1"), Names: []string{"gw.lan."}},
		{TTL: 2, IPs: []netip.Addr{netip.MustParseAddr("100.64.0.1"), netip.MustParseAddr("100.64.0.2")}, Names: []string{"", "b.isp"}},
		{TTL: 3, IPs: ip("100.64.1.1")},
		{TTL: 4, IPs: ip("1.1.1.1"), Final: true},
	}, st)
	wantDim := []bool{false, true, false, false}
	for i, h := range hops {
		if h.DimLoss != wantDim[i] {
			t.Errorf("hop %d: DimLoss %v (loss %.0f%%), want %v", h.TTL, h.DimLoss, h.Stats.LossPct, wantDim[i])
		}
	}
	if hops[0].Hosts != "10.0.0.1 (gw.lan)" || hops[1].Hosts != "100.64.0.1 100.64.0.2 (b.isp)" {
		t.Errorf("hosts %q, %q", hops[0].Hosts, hops[1].Hosts)
	}
	lines := hopTable(hops, 100)
	if s := lines[2].String(true); !strings.Contains(s, sgrDim+"  50.0%"+sgrReset) {
		t.Errorf("hop 2 loss not dimmed: %q", s)
	}
	if s := lines[3].String(true); strings.Contains(s, sgrDim) {
		t.Errorf("hop 3 loss dimmed: %q", s)
	}
}

func TestBuildView_Header(t *testing.T) {
	t.Parallel()
	v := viewAt(t, "wifi_ssid_hidden", 20)
	if !strings.HasPrefix(v.WiFi, "SSID hidden (location services off) ") {
		t.Errorf("Wi-Fi %q", v.WiFi)
	}
	v = viewAt(t, "cpu_high", 30)
	if !strings.HasPrefix(v.CPU, "Heavy ") && !strings.HasPrefix(v.CPU, "High ") {
		t.Errorf("CPU %q", v.CPU)
	}
	v = viewAt(t, "ok", 30)
	if v.GatewayMAC != "aa:bb:cc:dd:ee:ff" || v.Gateway != "192.168.1.1" || v.LocalIP != "192.168.1.23" || v.Adapter != "Wi-Fi" {
		t.Errorf("header %+v", v)
	}
	if v.WiFi != "Home 78% 5 GHz" {
		t.Errorf("Wi-Fi %q", v.WiFi)
	}
	for _, r := range v.Rows {
		if r.Name == "SYS-01" {
			t.Error("SYS-01 belongs in the header, not the table")
		}
	}
}

func TestCPUText(t *testing.T) {
	t.Parallel()
	th := config.Default().Thresholds
	for pct, want := range map[float64]string{10: "Light 10%", 50: "Moderate 50%", 75: "High 75%", 95: "Heavy 95%"} {
		if got := cpuText(engine.Snapshot{CPUAvg10: pct, HasCPU: true}, th); got != want {
			t.Errorf("%v: %q, want %q", pct, got, want)
		}
	}
	if got := cpuText(engine.Snapshot{}, th); got != "-" {
		t.Errorf("no CPU data: %q", got)
	}
}
