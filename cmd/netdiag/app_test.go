package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"netdiag/internal/clock"
	"netdiag/internal/config"
	"netdiag/internal/engine"
	"netdiag/internal/sysapi"
)

const simNoLink = `{"name": "no_link", "description": "cable out", "duration_s": 5,
	"sysinfo": {"adapter": {"type": "ethernet", "up": false}, "dns_servers": []}}`

func TestRun_SimPlaysScenario(t *testing.T) {
	t.Parallel()
	a := newTestApp(2)
	a.fs.Put(`C:\sc\no_link.json`, []byte(simNoLink))
	if code := a.main([]string{"run", "--sim", `C:\sc\no_link.json`}); code != exitOK {
		t.Fatalf("exit %d, stderr %q", code, a.errOut)
	}
	out := a.out.String()
	if !strings.Contains(out, `simulation "no_link"`) || !strings.Contains(out, "final: NO_LINK (no link) [exit 10]") {
		t.Errorf("output:\n%s", out)
	}
}

// On a console, --sim shows the full live view (SPEC §9) and restores the
// console modes when the scenario ends.
func TestRun_SimShowsConsoleView(t *testing.T) {
	t.Parallel()
	a := newTestApp(2)
	con := &sysapi.FakeConsole{Count: 2, In: 0x1f7, Out: 0x3, Cols: 120, Keys: make(chan sysapi.Key)}
	a.console = con
	a.fs.Put(`C:\sc\no_link.json`, []byte(simNoLink))
	if code := a.main([]string{"run", "--sim", `C:\sc\no_link.json`}); code != exitOK {
		t.Fatalf("exit %d, stderr %q", code, a.errOut)
	}
	view := con.Output()
	for _, want := range []string{"\x1b[H", "Config: simulation no_link", "Verdict: ", "NO_LINK", "Call quality: ", "Name ", "LAN-01", "final: NO_LINK"} {
		if !strings.Contains(view, want) {
			t.Errorf("console output lacks %q:\n%s", want, view)
		}
	}
	if in, _ := con.InMode(); in != 0x1f7 {
		t.Errorf("stdin mode %#x not restored", in)
	}
}

func TestRun_SimErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		file string
		want string
	}{
		{[]string{"run", "--sim", `C:\nope.json`}, "", "--sim:"},
		{[]string{"run", "--sim", `C:\bad.json`}, `{"name": "x"}`, "scenario"},
		{[]string{"run", "--sim", `C:\bad.json`, "--headless", "--duration", "1m", "--json", `C:\r.json`}, simNoLink, "not implemented"},
	}
	for _, tt := range tests {
		a := newTestApp(2)
		if tt.file != "" {
			a.fs.Put(`C:\bad.json`, []byte(tt.file))
		}
		if code := a.main(tt.args); code != exitRuntime || !strings.Contains(a.errOut.String(), tt.want) {
			t.Errorf("%v: exit %d, stderr %q", tt.args, code, a.errOut)
		}
	}
}

type testApp struct {
	*app
	out, errOut *bytes.Buffer
	stdin       *countingReader
	fs          *config.FakeFS
}

// countingReader records whether anything read from stdin.
type countingReader struct {
	r     io.Reader
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	return c.r.Read(p)
}

func newTestApp(consoleCount int) *testApp {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	in := &countingReader{r: strings.NewReader("\n")}
	fsys := config.NewFakeFS()
	return &testApp{
		app: &app{
			ctx:     context.Background(),
			clock:   clock.NewScaled(200), // simulations run 200× faster
			stdin:   in,
			stdout:  out,
			stderr:  errOut,
			console: &sysapi.FakeConsole{Count: consoleCount, NoIn: true, NoOut: true}, // redirected: plain output
			fs:      fsys,
			paths:   config.Paths{ExeDir: `C:\tools`, AppData: `C:\Users\u\AppData\Roaming`},
			version: "1.2.3",
			commit:  "abc1234",
			live:    func(context.Context, *config.Config, *engine.Engine, *slog.Logger) error { return nil },
			openLog: func(string) (*slog.Logger, func(), error) { return nil, func() {}, nil },
		},
		out: out, errOut: errOut, stdin: in, fs: fsys,
	}
}

func TestMain_NoArgsPrintsHelpAndExitsZero(t *testing.T) {
	t.Parallel()
	a := newTestApp(2)
	if code := a.main(nil); code != exitOK {
		t.Errorf("exit = %d, want 0", code)
	}
	for _, want := range []string{"Usage:", "netdiag run", "save-config", "--headless", "Exit codes"} {
		if !strings.Contains(a.out.String(), want) {
			t.Errorf("help lacks %q", want)
		}
	}
	if a.errOut.Len() != 0 {
		t.Errorf("stderr not empty: %q", a.errOut)
	}
}

func TestMain_HelpAliases(t *testing.T) {
	t.Parallel()
	full := newTestApp(2)
	full.main(nil)
	for _, args := range [][]string{{"help"}, {"-h"}, {"-help"}, {"--help"}, {"run", "-h"}, {"run", "-help"}, {"run", "--help"}} {
		a := newTestApp(2)
		if code := a.main(args); code != exitOK || a.out.String() != full.out.String() {
			t.Errorf("%v: exit %d, want full help, got %q", args, code, a.out)
		}
	}
}

func TestSaveConfig_HelpIsCommandSpecific(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"-h", "-help", "--help"} {
		a := newTestApp(2)
		if code := a.main([]string{"save-config", arg}); code != exitOK {
			t.Errorf("%s: exit %d", arg, code)
		}
		out := a.out.String()
		if !strings.HasPrefix(out, "Usage:\n  netdiag save-config [<path>]\n") || strings.Contains(out, "netdiag run") {
			t.Errorf("%s: want save-config usage only, got %q", arg, out)
		}
		if len(a.fs.Files) != 0 {
			t.Errorf("%s: help must not write files", arg)
		}
	}
}

func TestMain_HelpHidesNothingUnspecified(t *testing.T) {
	t.Parallel()
	a := newTestApp(2)
	a.main(nil)
	if strings.Contains(a.out.String(), "--sim") {
		t.Error("help must not mention the hidden --sim flag")
	}
	for i, line := range strings.Split(a.out.String(), "\n") {
		if len([]rune(line)) > 79 {
			t.Errorf("help line %d longer than 79 columns: %q", i+1, line)
		}
	}
}

func TestMain_UnknownCommand(t *testing.T) {
	t.Parallel()
	a := newTestApp(2)
	if code := a.main([]string{"frobnicate"}); code != exitRuntime {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(a.errOut.String(), `unknown command "frobnicate"`) || !strings.Contains(a.errOut.String(), "Usage:") {
		t.Errorf("stderr = %q", a.errOut)
	}
}

func TestMain_Version(t *testing.T) {
	t.Parallel()
	a := newTestApp(1)
	if code := a.main([]string{"version"}); code != exitOK {
		t.Errorf("exit = %d", code)
	}
	if !regexp.MustCompile(`^netdiag 1\.2\.3 \(abc1234, go[^)]+\)\n$`).MatchString(a.out.String()) {
		t.Errorf("version output = %q", a.out)
	}
	if a.stdin.reads != 0 {
		t.Error("version must not pause, even from Explorer")
	}
}

func TestMain_DoubleClickPausesAfterHelp(t *testing.T) {
	t.Parallel()
	a := newTestApp(1)
	a.main(nil)
	if !strings.Contains(a.out.String(), "Press Enter to exit") {
		t.Error("no pause prompt with process count 1")
	}
	if a.stdin.reads == 0 {
		t.Error("did not wait for input")
	}
}

func TestMain_NoPauseInSharedConsole(t *testing.T) {
	t.Parallel()
	a := newTestApp(2)
	a.main(nil)
	if strings.Contains(a.out.String(), "Press Enter") || a.stdin.reads != 0 {
		t.Error("must not pause when other processes share the console")
	}
}

func TestMain_NoPauseWithoutConsole(t *testing.T) {
	t.Parallel()
	a := newTestApp(0)
	a.console = &sysapi.FakeConsole{Err: errors.New("no console")}
	a.main(nil)
	if a.stdin.reads != 0 {
		t.Error("must not pause without a console")
	}
}

func TestMain_DoubleClickPausesAfterFatalError(t *testing.T) {
	t.Parallel()
	a := newTestApp(1)
	a.fs.Put(filepath.Join(`C:\tools`, config.FileName), []byte(`{"ui":{"port":0}}`))
	if code := a.main([]string{"run"}); code != exitConfig {
		t.Errorf("exit = %d, want 2", code)
	}
	if a.stdin.reads == 0 {
		t.Error("did not pause after fatal error")
	}
}

func TestMain_DoubleClickPauseHandlesEOF(t *testing.T) {
	t.Parallel()
	a := newTestApp(1)
	a.stdin.r = strings.NewReader("")
	if code := a.main(nil); code != exitOK {
		t.Errorf("exit = %d", code)
	}
}

func TestRun_ConfigErrorNamesKeyPathAndExitsTwo(t *testing.T) {
	t.Parallel()
	a := newTestApp(2)
	a.fs.Put(`C:\c.json`, []byte(`{"targets":{"ping":[{"name":"a","host":"10.0.0.1"},{"name":"b","host":"10.0.0.2","rate":0}]}}`))
	if code := a.main([]string{"run", "--config", `C:\c.json`}); code != exitConfig {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(a.errOut.String(), "targets.ping[1].rate") {
		t.Errorf("stderr = %q", a.errOut)
	}
}

func TestRun_MissingConfigFileExitsTwo(t *testing.T) {
	t.Parallel()
	a := newTestApp(2)
	if code := a.main([]string{"run", "--config", `C:\missing.json`}); code != exitConfig {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestRun_ValidConfigReportsSource(t *testing.T) {
	t.Parallel()
	a := newTestApp(2)
	if code := a.main([]string{"run"}); code != exitOK {
		t.Fatalf("exit = %d, stderr %q", code, a.errOut)
	}
	if !strings.Contains(a.out.String(), "Config: "+config.SourceDefaults) {
		t.Errorf("out = %q", a.out)
	}
}

func TestRun_FlagValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		want string // substring of stderr; empty = must succeed
	}{
		{[]string{"run", "--port", "8080"}, ""},
		{[]string{"run", "--csv", `C:\logs`, "--log", `C:\l.txt`}, ""},
		{[]string{"run", "--headless", "--duration", "90s", "--json", `C:\r.json`}, ""},
		{[]string{"run", "--headless", "--duration", "10s", "--json", `C:\r.json`, "--csv", `C:\x`}, ""},
		{[]string{"run", "--bogus"}, "flag provided but not defined"},
		{[]string{"run", "extra"}, `unexpected argument "extra"`},
		{[]string{"run", "--port", "0"}, "--port must be between"},
		{[]string{"run", "--port", "70000"}, "--port must be between"},
		{[]string{"run", "--port", "x"}, "invalid value"},
		{[]string{"run", "--headless", "--json", `C:\r.json`}, "requires --duration"},
		{[]string{"run", "--headless", "--duration", "1m"}, "requires --json"},
		{[]string{"run", "--headless", "--duration", "9s", "--json", `C:\r.json`}, "at least 10s"},
		{[]string{"run", "--headless", "--duration", "5", "--json", `C:\r.json`}, "invalid value"},
		{[]string{"run", "--headless", "--duration", "1m", "--json", `C:\r.json`, "--port", "1"}, "cannot be used with --headless"},
		{[]string{"run", "--duration", "1m"}, "require --headless"},
		{[]string{"run", "--json", `C:\r.json`}, "require --headless"},
	}
	for _, tt := range tests {
		a := newTestApp(2)
		code := a.main(tt.args)
		if tt.want == "" {
			if code != exitOK {
				t.Errorf("%v: exit %d, stderr %q", tt.args, code, a.errOut)
			}
			continue
		}
		if code != exitRuntime || !strings.Contains(a.errOut.String(), tt.want) {
			t.Errorf("%v: exit %d, stderr %q; want exit 1 with %q", tt.args, code, a.errOut, tt.want)
		}
	}
}

func TestRun_PortFlagOverridesConfig(t *testing.T) {
	t.Parallel()
	a := newTestApp(2)
	o, err := a.parseRun([]string{"--port", "9000"})
	if err != nil || !o.portSet || o.port != 9000 {
		t.Fatalf("got %+v, %v", o, err)
	}
}

func TestSaveConfig_WritesAndPrintsPath(t *testing.T) {
	t.Parallel()
	a := newTestApp(2)
	if code := a.main([]string{"save-config"}); code != exitOK {
		t.Fatalf("exit = %d, stderr %q", code, a.errOut)
	}
	want := filepath.Join(`C:\tools`, config.FileName)
	if strings.TrimSpace(a.out.String()) != want {
		t.Errorf("printed %q, want %q", a.out, want)
	}
	if _, ok := a.fs.Get(want); !ok {
		t.Error("file not written")
	}
}

func TestSaveConfig_OutputAcceptedByRun(t *testing.T) {
	t.Parallel()
	a := newTestApp(2)
	p := `D:\cfg\netdiag.json`
	if code := a.main([]string{"save-config", p}); code != exitOK {
		t.Fatalf("save-config exit = %d, stderr %q", code, a.errOut)
	}
	a.out.Reset()
	if code := a.main([]string{"run", "--config", p}); code != exitOK {
		t.Fatalf("run --config exit = %d, stderr %q", code, a.errOut)
	}
	if !strings.Contains(a.out.String(), "Config: "+p) {
		t.Errorf("out = %q", a.out)
	}
}

func TestSaveConfig_RefusesOverwrite(t *testing.T) {
	t.Parallel()
	a := newTestApp(2)
	a.main([]string{"save-config"})
	if code := a.main([]string{"save-config"}); code != exitRuntime {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(a.errOut.String(), "refusing to overwrite") {
		t.Errorf("stderr = %q", a.errOut)
	}
}

func TestSaveConfig_Usage(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"save-config", "a", "b"}, {"save-config", "--force"}} {
		a := newTestApp(2)
		if code := a.main(args); code != exitRuntime {
			t.Errorf("%v: exit = %d, want 1", args, code)
		}
	}
}

func TestRun_StartsLiveProbingWithTheLoadedConfig(t *testing.T) {
	t.Parallel()
	a := newTestApp(2)
	var gotCfg *config.Config
	var gotLog string
	a.openLog = func(path string) (*slog.Logger, func(), error) {
		gotLog = path
		return slog.New(slog.DiscardHandler), func() {}, nil
	}
	var gotLogger *slog.Logger
	a.live = func(_ context.Context, cfg *config.Config, eng *engine.Engine, logger *slog.Logger) error {
		gotCfg, gotLogger = cfg, logger
		if eng == nil {
			t.Error("no engine for the console")
		}
		return nil
	}
	if code := a.main([]string{"run", "--port", "9999", "--csv", `C:\logs`, "--log", `C:\l.txt`}); code != exitOK {
		t.Fatalf("exit %d, stderr %q", code, a.errOut)
	}
	if gotCfg == nil || gotCfg.UI.Port != 9999 || gotCfg.CSV.Dir != `C:\logs` || gotLog != `C:\l.txt` || gotLogger == nil {
		t.Errorf("live got cfg %+v, log %q", gotCfg, gotLog)
	}
	if !strings.Contains(a.out.String(), "final: CHECKING") || strings.Contains(a.out.String(), "not implemented") {
		t.Errorf("out = %q", a.out)
	}
}

func TestRun_LiveErrorExitsOne(t *testing.T) {
	t.Parallel()
	a := newTestApp(2)
	a.live = func(context.Context, *config.Config, *engine.Engine, *slog.Logger) error {
		return errors.New("list adapters: boom")
	}
	if code := a.main([]string{"run"}); code != exitRuntime || !strings.Contains(a.errOut.String(), "list adapters: boom") {
		t.Errorf("exit %d, stderr %q", code, a.errOut)
	}
}

func TestRun_HeadlessIsNotImplementedYet(t *testing.T) {
	t.Parallel()
	a := newTestApp(2)
	a.live = func(context.Context, *config.Config, *engine.Engine, *slog.Logger) error {
		t.Error("headless must not start live probing yet")
		return nil
	}
	if code := a.main([]string{"run", "--headless", "--duration", "30s", "--json", `C:\r.json`}); code != exitOK {
		t.Errorf("exit %d", code)
	}
	if !strings.Contains(a.out.String(), "Headless mode is not implemented") {
		t.Errorf("out = %q", a.out)
	}
}

func TestRun_LogOpenErrorExitsOne(t *testing.T) {
	t.Parallel()
	a := newTestApp(2)
	a.openLog = func(string) (*slog.Logger, func(), error) { return nil, nil, errors.New("access denied") }
	if code := a.main([]string{"run", "--log", `C:\x.log`}); code != exitRuntime || !strings.Contains(a.errOut.String(), "--log: access denied") {
		t.Errorf("exit %d, stderr %q", code, a.errOut)
	}
}
