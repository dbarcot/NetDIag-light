package sim

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"netdiag/internal/verdict"
)

const scenarioDir = "../../testdata/scenarios"

func loadScenarios(t *testing.T) map[string]*Scenario {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(scenarioDir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no scenarios in %s: %v", scenarioDir, err)
	}
	out := map[string]*Scenario{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sc, err := Parse(data)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(f), err)
		}
		if want := strings.TrimSuffix(filepath.Base(f), ".json"); sc.Name != want {
			t.Errorf("%s: name %q must match the file name", f, sc.Name)
		}
		out[sc.Name] = sc
	}
	return out
}

// Every scenario file runs to completion under the fake clock and meets
// its own expect block.
func TestScenarios(t *testing.T) {
	t.Parallel()
	for name, sc := range loadScenarios(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			outcomes, err := Run(sc)
			if err != nil {
				t.Fatal(err)
			}
			if len(outcomes) != len(sc.Expect) {
				t.Fatalf("%d of %d expectations checked", len(outcomes), len(sc.Expect))
			}
			for _, o := range outcomes {
				for _, e := range o.Errors {
					t.Errorf("t=%ds: %s", o.Expect.AtS, e)
				}
			}
		})
	}
}

// The scenario set covers every verdict and info flag (docs/PHASES.md
// Phase 3 acceptance) and the required list of docs/TESTING.md §3.
func TestScenarios_Coverage(t *testing.T) {
	t.Parallel()
	scs := loadScenarios(t)
	verdicts := map[string]bool{}
	flags := map[string]bool{}
	for _, sc := range scs {
		for _, e := range sc.Expect {
			verdicts[e.Verdict] = true
			for _, f := range e.FlagsInclude {
				flags[strings.SplitN(f, ":", 2)[0]] = true
			}
		}
	}
	for _, v := range append(slices.Clone(verdict.Priority), verdict.OK, verdict.Checking) {
		if !verdicts[string(v)] {
			t.Errorf("no scenario expects verdict %s", v)
		}
	}
	for _, f := range []string{
		verdict.FlagGWICMPBlocked, verdict.FlagICMPBlocked, verdict.FlagSymmetricNAT, verdict.FlagWiFiWeak,
		verdict.FlagWiFi24GHz, verdict.FlagCPUHigh, verdict.FlagTeamsSrcPortsBlocked, verdict.FlagTeamsDisabled,
		verdict.FlagTeamsRelayUnreachable, verdict.FlagServiceUnreachable, verdict.FlagCustomTargetDown, verdict.FlagCSVWriteError,
	} {
		if !flags[f] {
			t.Errorf("no scenario expects flag %s", f)
		}
	}
	required := []string{
		"ok", "no_link", "apipa", "no_address", "gw_down", "gw_icmp_blocked", "captive_portal", "isp_down",
		"icmp_blocked", "dns_local_fail", "dns_fail", "udp_blocked", "teams_udp_blocked", "teams_partial_3480",
		"teams_disabled_no_relays", "teams_relay_unreachable", "service_unreachable", "checking_startup",
		"csv_write_error", "meet_udp_blocked", "mtu_issue", "lan_loss", "isp_loss", "poor_voip", "symmetric_nat",
		"wifi_weak_24ghz", "wifi_ssid_hidden", "cpu_high", "no_media_fallback", "custom_target_down",
		"adapter_change", "flapping_gateway", "recovery", "onlink_route", "no_route_fallback",
	}
	for _, name := range required {
		if scs[name] == nil {
			t.Errorf("required scenario %s missing", name)
		}
	}
}

// Same scenario, same results: runs are reproducible.
func TestRun_Deterministic(t *testing.T) {
	t.Parallel()
	sc := loadScenarios(t)["gw_down"]
	a, _ := Run(sc)
	b, _ := Run(sc)
	for i := range a {
		if a[i].Snapshot.Result.Verdict != b[i].Snapshot.Result.Verdict ||
			a[i].Snapshot.Result.Quality.MOS != b[i].Snapshot.Result.Quality.MOS {
			t.Fatalf("expectation %d differs between runs", i)
		}
	}
}

func TestParse_RejectsBadScenarios(t *testing.T) {
	t.Parallel()
	base := `"name": "x", "duration_s": 10, "sysinfo": {"adapter": {"type": "ethernet", "ipv4": "10.0.0.2/24"}, "gateway": {"ip": "10.0.0.1"}}`
	bad := map[string]string{
		"unknown field":   `{` + base + `, "bogus": 1}`,
		"unknown probe":   `{` + base + `, "rules": [{"probe": "NET-99", "result": "fail"}]}`,
		"rule w/o probe":  `{` + base + `, "rules": [{"result": "fail"}]}`,
		"bad result":      `{` + base + `, "rules": [{"probe": "NET-01", "result": "maybe"}]}`,
		"bad arp state":   `{` + base + `, "rules": [{"probe": "LAN-02", "arp_state": "fresh"}]}`,
		"bad rtt":         `{` + base + `, "rules": [{"probe": "NET-01", "rtt_ms": [1]}]}`,
		"bad pattern":     `{` + base + `, "rules": [{"probe": "NET-01", "pattern": "ox"}]}`,
		"bad gate":        `{` + base + `, "expect": [{"at_s": 1, "gates": {"L9": "open"}}]}`,
		"expect too late": `{` + base + `, "expect": [{"at_s": 11}]}`,
		"bad config":      `{` + base + `, "config": {"ui": {"port": 0}}}`,
		"no gateway":      `{"name": "x", "duration_s": 10, "sysinfo": {"adapter": {"type": "ethernet", "ipv4": "10.0.0.2/24"}}}`,
		"bad sysinfo key": `{"name": "x", "duration_s": 10, "sysinfo": {"adaptr": {}}}`,
		"no duration":     `{"name": "x", "sysinfo": {}}`,
	}
	for name, data := range bad {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
