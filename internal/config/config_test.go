package config

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefault_IsValid(t *testing.T) {
	t.Parallel()
	if err := Default().Validate(); err != nil {
		t.Fatalf("defaults invalid:\n%v", err)
	}
}

func TestDefault_MatchesSpecValues(t *testing.T) {
	t.Parallel()
	c := Default()
	checks := []struct {
		name      string
		got, want any
	}{
		{"ui.port", c.UI.Port, 48765},
		{"window.seconds", c.Window.Seconds, 60},
		{"window.min_samples", c.Window.MinSamples, 10},
		{"history.minutes", c.History.Minutes, 15},
		{"gate.fail_count", c.Gate.FailCount, 3},
		{"targets.internet_icmp", c.Targets.InternetICMP, []string{"1.1.1.1", "8.8.8.8"}},
		{"targets.internet_tcp", c.Targets.InternetTCP, "1.1.1.1:443"},
		{"teams.relays", c.Teams.Relays, []string{"worldaz.tr.teams.microsoft.com"}},
		{"teams.udp_ports", c.Teams.UDPPorts, []int{3478}},
		{"rates.mtu", c.Rates.MTU, 600},
		{"timeouts.http", c.Timeouts.HTTP, 5000},
		{"thresholds.mos_min", c.Thresholds.MOSMin, 3.6},
		{"thresholds.mtu_min", c.Thresholds.MTUMin, 1400},
		{"trace.interval_ms", c.Trace.IntervalMs, 200},
		{"csv.separator", c.CSV.Separator, ";"},
		{"csv.decimal", c.CSV.Decimal, ","},
		{"http_check count", len(c.Targets.HTTPCheck), 2},
	}
	for _, ch := range checks {
		if !reflect.DeepEqual(ch.got, ch.want) {
			t.Errorf("%s = %v, want %v", ch.name, ch.got, ch.want)
		}
	}
}

// Every struct field must be spelled out in defaults.json, so save-config
// writes every key and the embedded file cannot silently drift.
func TestDefaultsJSON_ContainsEveryKey(t *testing.T) {
	t.Parallel()
	var raw map[string]any
	if err := json.Unmarshal(defaultsJSON, &raw); err != nil {
		t.Fatal(err)
	}
	var walk func(v any, typ reflect.Type, path string)
	walk = func(v any, typ reflect.Type, path string) {
		if typ.Kind() != reflect.Struct {
			return
		}
		m, ok := v.(map[string]any)
		if !ok {
			t.Errorf("%s: not an object in defaults.json", path)
			return
		}
		for i := range typ.NumField() {
			f := typ.Field(i)
			name := jsonName(f)
			sub, ok := m[name]
			if !ok {
				t.Errorf("%s missing from defaults.json", joinPath(path, name))
				continue
			}
			walk(sub, f.Type, joinPath(path, name))
		}
	}
	walk(raw, reflect.TypeFor[Config](), "")

	var errs errList
	checkShape(mustRaw(t, defaultsJSON), reflect.TypeFor[Config](), "", &errs)
	if err := errs.err(); err != nil {
		t.Errorf("defaults.json has unknown keys or wrong types:\n%v", err)
	}
}

func mustRaw(t *testing.T, b []byte) map[string]any {
	t.Helper()
	m, err := decodeRaw(b)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParse_PartialOverrideMerges(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(`{"ui": {"port": 5000}, "thresholds": {"rtt_ms": 200}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.UI.Port != 5000 || c.Thresholds.RTTMs != 200 {
		t.Errorf("override not applied: port=%d rtt=%v", c.UI.Port, c.Thresholds.RTTMs)
	}
	d := Default()
	if c.Window.Seconds != d.Window.Seconds || c.Thresholds.JitterMs != d.Thresholds.JitterMs {
		t.Error("keys not in the file must keep their defaults")
	}
	if !reflect.DeepEqual(c.Targets, d.Targets) {
		t.Error("untouched sections must keep their defaults")
	}
}

func TestParse_EmptyObjectEqualsDefaults(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, Default()) {
		t.Error("empty file must equal defaults")
	}
}

func TestParse_ArrayEntriesDoNotInheritDefaultEntries(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(`{"targets": {"http_check": [{"url": "http://portal.example/", "expect_status": 204}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []HTTPCheck{{URL: "http://portal.example/", ExpectStatus: 204}}
	if !reflect.DeepEqual(c.Targets.HTTPCheck, want) {
		t.Errorf("http_check = %+v, want %+v", c.Targets.HTTPCheck, want)
	}
	if len(Default().Targets.HTTPCheck[0].ExpectBody) == 0 {
		t.Error("defaults were mutated")
	}
}

func TestParse_ArraysReplaceDefaults(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(`{"targets": {"internet_icmp": ["9.9.9.9"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Targets.InternetICMP, []string{"9.9.9.9"}) {
		t.Errorf("internet_icmp = %v, want [9.9.9.9]", c.Targets.InternetICMP)
	}
}

func TestParse_AcceptsUTF8BOM(t *testing.T) {
	t.Parallel()
	if _, err := Parse([]byte("\xef\xbb\xbf{\"ui\":{\"port\":5000}}")); err != nil {
		t.Fatal(err)
	}
}

func TestParse_InvalidJSONReportsLine(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte("{\n  \"ui\": {\n    \"port\": 5000,\n  }\n}"))
	if err == nil || !strings.Contains(err.Error(), "line 4") {
		t.Fatalf("want syntax error at line 4, got %v", err)
	}
	for _, bad := range []string{``, `{`, `{} {}`, `[]`, `"x"`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%q) accepted invalid input", bad)
		}
	}
}

// errorPaths parses data and returns the paths of all validation errors.
func errorPaths(t *testing.T, data string) []string {
	t.Helper()
	_, err := Parse([]byte(data))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Parse(%s): want *ValidationError, got %v", data, err)
	}
	paths := make([]string, len(ve.Fields))
	for i, f := range ve.Fields {
		paths[i] = f.Path
	}
	return paths
}

func TestParse_ErrorsNameExactKeyPath(t *testing.T) {
	t.Parallel()
	ping := func(entries string) string { return `{"targets":{"ping":[` + entries + `]}}` }
	tcp := func(entries string) string { return `{"targets":{"tcp":[` + entries + `]}}` }
	tests := []struct {
		name string
		data string
		path string
	}{
		// shape
		{"unknown top-level key", `{"uii": {}}`, "uii"},
		{"unknown nested key", `{"ui": {"prt": 1}}`, "ui.prt"},
		{"unknown key in array entry", ping(`{"name":"a","host":"10.0.0.1","hots":"x"}`), "targets.ping[0].hots"},
		{"string for int", `{"ui": {"port": "80"}}`, "ui.port"},
		{"float for int", `{"ui": {"port": 80.5}}`, "ui.port"},
		{"null", `{"ui": {"port": null}}`, "ui.port"},
		{"object for array", `{"targets": {"stun": {}}}`, "targets.stun"},
		{"number in string array", `{"targets": {"stun": ["a.example:1", 5]}}`, "targets.stun[1]"},
		{"string for bool", `{"csv": {"bom": "yes"}}`, "csv.bom"},
		{"string for float", `{"thresholds": {"rtt_ms": "150"}}`, "thresholds.rtt_ms"},
		{"array for object", `{"ui": []}`, "ui"},
		// values
		{"version", `{"version": 2}`, "version"},
		{"port zero", `{"ui": {"port": 0}}`, "ui.port"},
		{"port too big", `{"ui": {"port": 70000}}`, "ui.port"},
		{"window too short", `{"window": {"seconds": 5}}`, "window.seconds"},
		{"min_samples above window", `{"window": {"min_samples": 61}}`, "window.min_samples"},
		{"history zero", `{"history": {"minutes": 0}}`, "history.minutes"},
		{"gate fail_count zero", `{"gate": {"fail_count": 0}}`, "gate.fail_count"},
		{"gate ok_count zero", `{"gate": {"ok_count": 0}}`, "gate.ok_count"},
		{"ping rate below 1", ping(`{"name":"a","host":"10.0.0.1"},{"name":"b","host":"10.0.0.2","rate":0}`), "targets.ping[1].rate"},
		{"ping name missing", ping(`{"host":"10.0.0.1"}`), "targets.ping[0].name"},
		{"ping name too long", ping(`{"name":"` + strings.Repeat("x", 41) + `","host":"10.0.0.1"}`), "targets.ping[0].name"},
		{"ping host missing", ping(`{"name":"a"}`), "targets.ping[0].host"},
		{"ping host invalid", ping(`{"name":"a","host":"bad host"}`), "targets.ping[0].host"},
		{"ping host malformed ip", ping(`{"name":"a","host":"1.1.1"}`), "targets.ping[0].host"},
		{"ping host ipv6", ping(`{"name":"a","host":"fd00::1"}`), "targets.ping[0].host"},
		{"ping host with port", ping(`{"name":"a","host":"10.0.0.1:80"}`), "targets.ping[0].host"},
		{"ping depends_on invalid", ping(`{"name":"a","host":"10.0.0.1","depends_on":"dns"}`), "targets.ping[0].depends_on"},
		{"tcp host without port", tcp(`{"name":"a","host":"10.0.0.1"}`), "targets.tcp[0].host"},
		{"tcp port invalid", tcp(`{"name":"a","host":"10.0.0.1:0"}`), "targets.tcp[0].host"},
		{"tcp rate below 1", tcp(`{"name":"a","host":"10.0.0.1:443","rate":-1}`), "targets.tcp[0].rate"},
		{"duplicate name in ping", ping(`{"name":"a","host":"10.0.0.1"},{"name":"a","host":"10.0.0.2"}`), "targets.ping[1].name"},
		{"duplicate name across ping and tcp", `{"targets":{"ping":[{"name":"a","host":"10.0.0.1"}],"tcp":[{"name":"a","host":"10.0.0.1:80"}]}}`, "targets.tcp[0].name"},
		{"custom target cap", `{"limits":{"max_custom_targets":1},"targets":{"ping":[{"name":"a","host":"10.0.0.1"}],"tcp":[{"name":"b","host":"10.0.0.1:80"}]}}`, "targets"},
		{"internet_icmp empty", `{"targets": {"internet_icmp": []}}`, "targets.internet_icmp"},
		{"internet_icmp hostname", `{"targets": {"internet_icmp": ["1.1.1.1", "one.one.one.one"]}}`, "targets.internet_icmp[1]"},
		{"internet_tcp no port", `{"targets": {"internet_tcp": "1.1.1.1"}}`, "targets.internet_tcp"},
		{"internet_tcp hostname", `{"targets": {"internet_tcp": "one.one.one.one:443"}}`, "targets.internet_tcp"},
		{"public_dns empty", `{"targets": {"public_dns": []}}`, "targets.public_dns"},
		{"public_dns hostname", `{"targets": {"public_dns": ["dns.google"]}}`, "targets.public_dns[0]"},
		{"dns_test_name", `{"targets": {"dns_test_name": ""}}`, "targets.dns_test_name"},
		{"stun empty", `{"targets": {"stun": []}}`, "targets.stun"},
		{"stun no port", `{"targets": {"stun": ["stun.example.com"]}}`, "targets.stun[0]"},
		{"http_check url", `{"targets": {"http_check": [{"url":"ftp://x.example/","expect_status":200}]}}`, "targets.http_check[0].url"},
		{"http_check status", `{"targets": {"http_check": [{"url":"http://x.example/"}]}}`, "targets.http_check[0].expect_status"},
		{"services hostname", `{"targets": {"services": ["teams.microsoft.com", "1.2.3.4"]}}`, "targets.services[1]"},
		{"trace", `{"targets": {"trace": ""}}`, "targets.trace"},
		{"teams relay", `{"teams": {"relays": ["bad_host"]}}`, "teams.relays[0]"},
		{"teams ports empty with relays", `{"teams": {"udp_ports": []}}`, "teams.udp_ports"},
		{"teams port range", `{"teams": {"udp_ports": [3478, 70000]}}`, "teams.udp_ports[1]"},
		{"meet stun", `{"meet": {"stun": ["stun.l.google.com"]}}`, "meet.stun[0]"},
		{"rate below 1", `{"rates": {"lan_ping": 0}}`, "rates.lan_ping"},
		{"rate too high", `{"rates": {"nat": 3601}}`, "rates.nat"},
		{"timeout too low", `{"timeouts": {"icmp": 99}}`, "timeouts.icmp"},
		{"timeout too high", `{"timeouts": {"tls": 10001}}`, "timeouts.tls"},
		{"loss_pct", `{"thresholds": {"loss_pct": 101}}`, "thresholds.loss_pct"},
		{"rtt_ms", `{"thresholds": {"rtt_ms": 0}}`, "thresholds.rtt_ms"},
		{"jitter_ms", `{"thresholds": {"jitter_ms": -1}}`, "thresholds.jitter_ms"},
		{"mos_min", `{"thresholds": {"mos_min": 5}}`, "thresholds.mos_min"},
		{"mos_good below mos_min", `{"thresholds": {"mos_good": 3.5}}`, "thresholds.mos_good"},
		{"mtu_min", `{"thresholds": {"mtu_min": 500}}`, "thresholds.mtu_min"},
		{"wifi_signal_pct", `{"thresholds": {"wifi_signal_pct": 101}}`, "thresholds.wifi_signal_pct"},
		{"cpu_moderate_pct", `{"thresholds": {"cpu_moderate_pct": 0}}`, "thresholds.cpu_moderate_pct"},
		{"cpu_high not above moderate", `{"thresholds": {"cpu_high_pct": 50}}`, "thresholds.cpu_high_pct"},
		{"cpu_heavy not above high", `{"thresholds": {"cpu_heavy_pct": 75}}`, "thresholds.cpu_heavy_pct"},
		{"trace max_hops", `{"trace": {"max_hops": 0}}`, "trace.max_hops"},
		{"trace interval below 100", `{"trace": {"interval_ms": 99}}`, "trace.interval_ms"},
		{"max_custom_targets", `{"limits": {"max_custom_targets": -1}}`, "limits.max_custom_targets"},
		{"csv separator", `{"csv": {"separator": "\t"}}`, "csv.separator"},
		{"csv decimal", `{"csv": {"decimal": ";"}}`, "csv.decimal"},
		{"csv decimal equals separator", `{"csv": {"separator": ",", "decimal": ","}}`, "csv.decimal"},
		{"csv summary_seconds", `{"csv": {"summary_seconds": 0}}`, "csv.summary_seconds"},
		{"csv rotate", `{"csv": {"rotate": "weekly"}}`, "csv.rotate"},
		{"csv max_mb", `{"csv": {"max_mb": 0}}`, "csv.max_mb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			paths := errorPaths(t, tt.data)
			if len(paths) != 1 || paths[0] != tt.path {
				t.Errorf("error paths = %v, want [%s]", paths, tt.path)
			}
		})
	}
}

func TestParse_ReportsAllErrors(t *testing.T) {
	t.Parallel()
	paths := errorPaths(t, `{"ui": {"port": 0}, "rates": {"wifi": 0}}`)
	if len(paths) != 2 {
		t.Errorf("want 2 errors, got %v", paths)
	}
}

func TestParse_ErrorMessageContainsPath(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`{"targets":{"ping":[{"name":"a","host":"10.0.0.1"},{"name":"b","host":"10.0.0.2"},{"name":"c","host":"10.0.0.3","rate":0}]}}`))
	if err == nil || !strings.HasPrefix(err.Error(), "targets.ping[2].rate: ") {
		t.Errorf("got %v", err)
	}
}

func TestParse_CustomTargetDefaultsAndInference(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(`{"targets": {
		"ping": [
			{"name": "fileserver", "host": "10.0.0.5"},
			{"name": "public", "host": "9.9.9.9", "rate": 2, "group": "ext"},
			{"name": "intranet", "host": "intranet.corp.local"},
			{"name": "forced", "host": "intranet2.corp.local", "depends_on": "lan"}
		],
		"tcp": [
			{"name": "erp", "host": "erp.corp.local:443", "depends_on": "lan"},
			{"name": "nas", "host": "192.168.1.10:445"},
			{"name": "web", "host": "8.8.8.8:443"}
		]}}`))
	if err != nil {
		t.Fatal(err)
	}
	p, tc := c.Targets.Ping, c.Targets.TCP
	want := []struct {
		got, want string
	}{
		{p[0].DependsOn, DependsLAN},
		{p[1].DependsOn, DependsInternet},
		{p[2].DependsOn, DependsInternet},
		{p[3].DependsOn, DependsLAN},
		{tc[0].DependsOn, DependsLAN},
		{tc[1].DependsOn, DependsLAN},
		{tc[2].DependsOn, DependsInternet},
	}
	for i, w := range want {
		if w.got != w.want {
			t.Errorf("target %d depends_on = %q, want %q", i, w.got, w.want)
		}
	}
	if p[0].Rate != 1 || p[1].Rate != 2 {
		t.Errorf("rates = %d, %d; want default 1 and explicit 2", p[0].Rate, p[1].Rate)
	}
	if p[1].Group != "ext" {
		t.Errorf("group = %q", p[1].Group)
	}
}

func TestInferDependsOn(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"10.0.0.5":            DependsLAN,
		"172.16.4.4":          DependsLAN,
		"192.168.1.1":         DependsLAN,
		"169.254.10.10":       DependsLAN,
		"127.0.0.1":           DependsLAN,
		"1.1.1.1":             DependsInternet,
		"100.64.0.1":          DependsInternet,
		"intranet.corp.local": DependsInternet,
		"fileserver":          DependsInternet,
	}
	for host, want := range tests {
		if got := InferDependsOn(host); got != want {
			t.Errorf("InferDependsOn(%q) = %q, want %q", host, got, want)
		}
	}
	if !IsHostname("fileserver") || IsHostname("10.0.0.5") {
		t.Error("IsHostname wrong")
	}
}

func TestLoad_LookupOrder(t *testing.T) {
	t.Parallel()
	paths := Paths{ExeDir: `C:\tools`, AppData: `C:\Users\u\AppData\Roaming`}
	exeFile := filepath.Join(paths.ExeDir, FileName)
	appFile := filepath.Join(paths.AppData, "netdiag", FileName)
	explicit := `C:\cfg\my.json`

	tests := []struct {
		name       string
		files      map[string]string
		explicit   string
		wantSource string
		wantPort   int
	}{
		{"defaults when no file", nil, "", SourceDefaults, 48765},
		{"appdata", map[string]string{appFile: `{"ui":{"port":3}}`}, "", appFile, 3},
		{"exe dir before appdata", map[string]string{appFile: `{"ui":{"port":3}}`, exeFile: `{"ui":{"port":2}}`}, "", exeFile, 2},
		{"explicit first", map[string]string{appFile: `{"ui":{"port":3}}`, exeFile: `{"ui":{"port":2}}`, explicit: `{"ui":{"port":1}}`}, explicit, explicit, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := NewFakeFS()
			for p, d := range tt.files {
				fsys.Put(p, []byte(d))
			}
			c, src, err := Load(fsys, tt.explicit, paths)
			if err != nil {
				t.Fatal(err)
			}
			if src != tt.wantSource || c.UI.Port != tt.wantPort {
				t.Errorf("source=%q port=%d, want %q %d", src, c.UI.Port, tt.wantSource, tt.wantPort)
			}
		})
	}
}

func TestLoad_ExplicitMissingIsError(t *testing.T) {
	t.Parallel()
	_, src, err := Load(NewFakeFS(), `C:\nope.json`, Paths{})
	if err == nil || src != `C:\nope.json` {
		t.Fatalf("want error for missing explicit file, got %v (source %q)", err, src)
	}
}

func TestLoad_InvalidFoundFileIsErrorNotSkipped(t *testing.T) {
	t.Parallel()
	paths := Paths{ExeDir: `C:\tools`}
	fsys := NewFakeFS()
	fsys.Put(filepath.Join(paths.ExeDir, FileName), []byte(`{"ui":{"port":0}}`))
	if _, _, err := Load(fsys, "", paths); err == nil {
		t.Fatal("invalid config next to the exe must be reported, not skipped")
	}
}

func TestLoad_EmptyPathsUseDefaults(t *testing.T) {
	t.Parallel()
	_, src, err := Load(NewFakeFS(), "", Paths{})
	if err != nil || src != SourceDefaults {
		t.Fatalf("got %q, %v", src, err)
	}
}
