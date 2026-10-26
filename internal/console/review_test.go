package console

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/config"
	"netdiag/internal/engine"
	"netdiag/internal/probe"
	"netdiag/internal/sim"
	"netdiag/internal/store"
	"netdiag/internal/sysapi"
	"netdiag/internal/verdict"
)

// ---- R5-01 sanitizing ----

func TestClean(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"\x1b[2J", "?[2J"},
		{"a\r\nb", "a??b"},
		{"evil\u202egnp.exe", "evil?gnp.exe"},
		{"\u2066x\u2069", "?x?"},
		{"a\u009bb", "a?b"}, // C1 CSI
		{"a\xffb", "a?b"},   // invalid UTF-8
		{"a\x00b", "a?b"},
		{"a\tb", "a?b"},
		{"a\x7fb", "a?b"},
		{"wifi 😀", "wifi ?"}, // outside the BMP
		{"Připojení k síti", "Připojení k síti"},
		{"日本 e\u0301 \ufffd", "日本 e\u0301 \ufffd"}, // a real U+FFFD is kept
	}
	for _, tt := range tests {
		if got := clean(tt.in); got != tt.want {
			t.Errorf("clean(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

const hostile = "x\x1b[2J\x1b]0;pwn\x07\r\n\u202eevil\u009b\xff\x00\tend"

// hostileSnapshot has attacker-controlled text everywhere it can come from
// the network or the OS.
func hostileSnapshot(t *testing.T, ssid, hopName string) (engine.Snapshot, *store.Store, *config.Config) {
	t.Helper()
	cfg := config.Default()
	now := t0.Add(30 * time.Second)
	st := store.New(clock.NewFake(now), time.Minute, time.Minute)
	ad := sysapi.Adapter{Index: 7, GUID: "{w}", Name: "Wi-Fi " + hostile, Type: sysapi.AdapterWiFi, Up: true,
		IPv4: []netip.Prefix{netip.MustParsePrefix("192.168.1.23/24")}, DNSServers: []netip.Addr{netip.MustParseAddr("192.168.1.1")}}
	sig := 70
	snap := engine.Snapshot{
		Time: now,
		Primary: sysapi.Primary{Adapter: ad, Gateway: netip.MustParseAddr("192.168.1.1"), Reason: sysapi.PrimaryByRoute},
		WiFi:    &sysapi.WiFiStatus{InterfaceGUID: "{w}", SSID: ssid, SignalPct: &sig, Band: sysapi.Band5GHz, BandSource: sysapi.BandFromFrequency},
		Result: verdict.Result{Verdict: verdict.OK, Detail: hostile, Flags: []string{"CUSTOM_TARGET_DOWN:" + hostile},
			Quality: verdict.CallQuality{Status: verdict.QualityMeasuring}},
		Hops: []probe.Hop{{TTL: 1, IPs: []netip.Addr{netip.MustParseAddr("10.0.0.1")}, Names: []string{hopName}, Final: true}},
		AdapterEvents: []engine.AdapterChange{{Seq: 1, Time: now, From: engine.AdapterInfo{Name: hostile}, To: engine.AdapterInfo{Name: "eth"}}},
	}
	cust := engine.Spec{Probe: "CUST-ICMP", Target: "host" + hostile, Name: "name" + hostile}
	cust.ID = "CUST:" + cust.Name
	snap.Specs = []engine.Spec{cust}
	st.Add(probe.Sample{Series: cust.ID, Time: now.Add(-time.Second), Result: probe.Skipped, Detail: "skipped: " + hostile})
	st.Add(probe.Sample{Series: probe.HopSeriesID(1), Time: now.Add(-time.Second), Result: probe.OK, RTT: time.Millisecond})
	return snap, st, cfg
}

func assertNoControl(t *testing.T, where, s string) {
	t.Helper()
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0xfffd {
			t.Errorf("%s: control rune %U in %q", where, r, s)
			return
		}
	}
}

func TestBuildView_HostileTextIsSanitized(t *testing.T) {
	t.Parallel()
	snap, st, cfg := hostileSnapshot(t, "ssid"+hostile, "hop"+hostile)
	v := BuildView(snap, st, cfg, Meta{Version: "1", ConfigSource: "c" + hostile})
	v.Keys = true
	for _, w := range []int{80, 200} {
		for i, l := range Render(v, w, time.UTC) {
			for _, sg := range l { // with colour on, only our own SGR runs add ESC
				assertNoControl(t, "tty line "+string(rune('0'+i%10)), sg.text)
			}
		}
	}
	// plain mode: changes, adapter events and the summary block
	out := &bytes.Buffer{}
	u := &ui{o: Options{Config: cfg, Location: time.UTC, Meta: Meta{Version: "1"}}, src: fixedSource{snap, st}, w: out}
	u.plainChanges(snap)
	out.WriteString(u.summary(snap, "summary", plainWidth, false))
	for _, l := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		assertNoControl(t, "plain", l)
	}
	const cleaned = "x?[2J?]0;pwn????evil????end"
	if clean(hostile) != cleaned {
		t.Fatalf("clean(hostile) = %q", clean(hostile))
	}
	if !strings.Contains(out.String(), "adapter change: "+cleaned) || !strings.Contains(out.String(), "namex?[2J?]0;pw…") {
		t.Errorf("sanitized texts missing:\n%s", out.String())
	}
	text := ""
	for _, l := range Render(v, 200, time.UTC) {
		text += l.String(false) + "\n"
	}
	for _, want := range []string{"Wi-Fi: ssid" + cleaned, "Adapter: Wi-Fi " + cleaned, "hop" + cleaned, "skipped: " + cleaned} {
		if !strings.Contains(text, want) {
			t.Errorf("frame lacks %q:\n%s", want, text)
		}
	}
}

// fixedSource always returns the same snapshot.
type fixedSource struct {
	snap engine.Snapshot
	st   *store.Store
}

func (f fixedSource) Snapshot() engine.Snapshot { return f.snap }
func (f fixedSource) Store() *store.Store       { return f.st }
func (f fixedSource) Reset()                    {}

// ---- R5-02 display width ----

func TestDisplayWidth(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]int{"abc": 3, "č": 1, "日本": 4, "e\u0301": 1, "a\u200db": 2, "한글": 4, "ＡＢ": 4, "▁█×…": 4} {
		if got := stringWidth(s); got != want {
			t.Errorf("width(%q) = %d, want %d", s, got, want)
		}
	}
	for _, tt := range []struct {
		in   string
		n    int
		want string
	}{{"日本語", 4, "日… "}, {"日本語", 6, "日本語"}, {"日本語", 5, "日本…"}, {"ae\u0301x", 3, "ae\u0301x"}, {"ae\u0301xy", 3, "ae\u0301…"}} {
		got := trunc(tt.in, tt.n)
		if got != tt.want || (stringWidth(tt.in) > tt.n && stringWidth(got) != tt.n) {
			t.Errorf("trunc(%q, %d) = %q (width %d), want %q", tt.in, tt.n, got, stringWidth(got), tt.want)
		}
	}
	if got := (line{{"abc", ""}, {"日本語", sgrRed}}).fit(5); got.width() != 5 || got.String(false) != "abc… " {
		t.Errorf("fit pads where a wide rune does not fit: %q width %d", got.String(false), got.width())
	}
	l := line{{"ab", ""}, {"日本語", sgrRed}}
	if got := l.fit(5); got.width() != 5 || got.String(false) != "ab日…" {
		t.Errorf("fit: %q width %d", got.String(false), got.width())
	}
}

func TestRender_WideTextFitsWidth(t *testing.T) {
	t.Parallel()
	frame := func(ssid, hop string, w int) []line {
		snap, st, cfg := hostileSnapshot(t, ssid, hop)
		return Render(BuildView(snap, st, cfg, Meta{Version: "1"}), w, time.UTC)
	}
	cjk := strings.Repeat("日本語", 30) + "…"
	cjkHop := strings.Repeat("ルーター", 40) + ".example"
	for _, w := range []int{80, 120, 200} {
		wide := frame(cjk, cjkHop, w)
		ascii := frame("Home", "gw.example", w)
		if len(wide) != len(ascii) {
			t.Errorf("width %d: %d lines with CJK text, %d with ASCII", w, len(wide), len(ascii))
		}
		for i, l := range wide {
			if n := stringWidth(l.String(false)); n > w {
				t.Errorf("width %d: line %d is %d columns: %q", w, i, n, l.String(false))
			}
		}
	}
}

// ---- R5-04 panics in goroutines ----

type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestRun_WorkPanicReturnsError(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(t0)
	con := ttyConsole()
	logs := &logBuffer{}
	o := options(c, con, &syncBuffer{})
	o.Logger = slog.New(slog.NewTextHandler(logs, nil))
	err := Run(context.Background(), o, newStub(c), func(context.Context) error { panic("probe boom") })
	if err == nil || !strings.Contains(err.Error(), "internal error: probe boom") {
		t.Fatalf("err = %v", err)
	}
	if in, _ := con.InMode(); in != origIn {
		t.Errorf("stdin mode %#x not restored", in)
	}
	if out, _ := con.OutMode(); out != origOut {
		t.Errorf("stdout mode %#x not restored", out)
	}
	if !strings.Contains(con.Output(), vtShowCursor) {
		t.Error("cursor not shown")
	}
	if l := logs.String(); !strings.Contains(l, "probe boom") || !strings.Contains(l, "goroutine") {
		t.Errorf("panic not logged with its stack: %s", l)
	}
}

// panicKeys is a console whose key reader panics.
type panicKeys struct{ *sysapi.FakeConsole }

func (panicKeys) ReadKey(context.Context) (sysapi.Key, error) { panic("key boom") }

func TestRun_KeyReaderPanicStopsKeysOnly(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(t0)
	src := newStub(c)
	con := ttyConsole()
	logs := &logBuffer{}
	o := options(c, panicKeys{con}, &syncBuffer{})
	o.Logger = slog.New(slog.NewTextHandler(logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, o, src, blockWork) }()
	waitUntil(t, "the key reader panic logged", func() bool { return strings.Contains(logs.String(), "key boom") })
	if !c.WaitTimers(1, 5*time.Second) {
		t.Fatal("no ticker")
	}
	c.Advance(time.Second)
	waitUntil(t, "still rendering", func() bool { n, _ := src.count(); return n >= 2 })
	select {
	case err := <-done:
		t.Fatalf("Run ended after the key reader panic: %v", err)
	default:
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if in, _ := con.InMode(); in != origIn {
		t.Errorf("stdin mode %#x not restored", in)
	}
}

// ---- R5-05 bounded shutdown ----

func TestRun_ShutdownGraceWhenWorkHangs(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(t0)
	src := newStub(c)
	con := ttyConsole()
	release := make(chan struct{})
	returned := make(chan struct{})
	work := func(context.Context) error { // ignores ctx
		<-release
		close(returned)
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), options(c, con, &syncBuffer{}), src, work) }()
	waitUntil(t, "first frame", func() bool { n, _ := src.count(); return n >= 1 })
	con.Keys <- sysapi.Key{Rune: 'q'}
	waitUntil(t, "Stopping…", func() bool { return strings.Contains(con.Output(), "Stopping…") })
	if !c.WaitTimers(1, 5*time.Second) {
		t.Fatal("no grace timer")
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned before the grace period: %v", err)
	default:
	}
	c.Advance(ShutdownGrace)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the grace period")
	}
	if !strings.Contains(con.Output(), "final: OK") {
		t.Error("no final summary")
	}
	if in, _ := con.InMode(); in != origIn {
		t.Errorf("stdin mode %#x not restored", in)
	}
	close(release) // the late return goes into the buffered channel
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("late work return blocked")
	}
}

// ---- R5-06 adapter event sequence ----

func TestPlainChanges_EventsBySeq(t *testing.T) {
	t.Parallel()
	ev := func(seq int) engine.AdapterChange {
		return engine.AdapterChange{Seq: seq, Time: t0, From: engine.AdapterInfo{Name: "a"}, To: engine.AdapterInfo{Name: string(rune('a' + seq))}}
	}
	out := &bytes.Buffer{}
	u := &ui{o: Options{Location: time.UTC}, w: out}
	u.plainChanges(engine.Snapshot{AdapterEvents: []engine.AdapterChange{ev(1), ev(2)}})
	u.plainChanges(engine.Snapshot{AdapterEvents: []engine.AdapterChange{ev(2), ev(3)}, AdapterEventsDropped: 1})
	u.plainChanges(engine.Snapshot{AdapterEvents: []engine.AdapterChange{ev(4)}, AdapterEventsDropped: 3})
	u.plainChanges(engine.Snapshot{AdapterEvents: []engine.AdapterChange{ev(4)}, AdapterEventsDropped: 3})
	text := out.String()
	for _, to := range []string{"-> b ", "-> c ", "-> d ", "-> e "} {
		if n := strings.Count(text, to); n != 1 {
			t.Errorf("event %q printed %d times:\n%s", to, n, text)
		}
	}
}

// ---- R5-07 public IP freshness ----

func TestPublicIP_Freshness(t *testing.T) {
	t.Parallel()
	now := t0.Add(10 * time.Minute)
	spec := func(probeID, id string) engine.Spec {
		s := engine.Spec{Probe: probeID}
		s.ID = id
		return s
	}
	snap := engine.Snapshot{Time: now, Specs: []engine.Spec{spec("RT-01", "RT-01:a"), spec("NAT-01", "NAT-01:stun")}}
	mapped := func(id, ip string, age time.Duration) probe.Sample {
		return probe.Sample{Series: id, Time: now.Add(-age), Result: probe.OK, Mapped: netip.AddrPortFrom(netip.MustParseAddr(ip), 5000)}
	}
	tests := []struct {
		name    string
		samples []probe.Sample
		want    string
	}{
		{"fresh", []probe.Sample{mapped("NAT-01:stun", "198.51.100.7", 10*time.Second)}, "198.51.100.7"},
		{"exactly 120 s", []probe.Sample{mapped("NAT-01:stun", "198.51.100.7", 120*time.Second)}, "198.51.100.7"},
		{"121 s old", []probe.Sample{mapped("NAT-01:stun", "198.51.100.7", 121*time.Second)}, "-"},
		{"RT-01 newer than NAT-01 wins", []probe.Sample{
			mapped("NAT-01:stun", "198.51.100.7", 30*time.Second), mapped("RT-01:a", "203.0.113.9", 5*time.Second)}, "203.0.113.9"},
		{"none", nil, "-"},
	}
	for _, tt := range tests {
		st := store.New(clock.NewFake(now), time.Minute, 15*time.Minute)
		for _, s := range tt.samples {
			st.Add(s)
		}
		if got := publicIP(snap, st); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
}

// ---- R5-08 final summary width ----

// engineAt plays a scenario to second at and returns its engine.
func engineAt(t *testing.T, name string, at int) *engine.Engine {
	t.Helper()
	var eng *engine.Engine
	if err := sim.Play(loadScenario(t, name), func(sec int, e *engine.Engine) bool {
		eng = e
		return sec < at
	}); err != nil {
		t.Fatal(err)
	}
	return eng
}

func TestRun_FinalSummaryFitsConsoleWidth(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(t0)
	con := ttyConsole()
	con.Cols = 80
	eng := engineAt(t, "teams_partial_3480", 30)
	o := options(c, con, &syncBuffer{})
	o.Config = loadScenario(t, "teams_partial_3480").Config()
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), o, eng, blockWork) }()
	waitUntil(t, "first frame", func() bool { return strings.Contains(con.Output(), "q quit") })
	con.Keys <- sysapi.Key{Rune: 'q'}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	out := con.Output()
	final := out[strings.LastIndex(out, vtShowCursor)+len(vtShowCursor):]
	if !strings.Contains(final, "final: ") || !strings.Contains(final, "TEAMS-01") {
		t.Fatalf("final summary:\n%s", final)
	}
	for _, l := range strings.Split(final, "\n") {
		if n := stringWidth(strings.TrimRight(l, "\r")); n > 80 {
			t.Errorf("summary line is %d columns: %q", n, l)
		}
	}
}
