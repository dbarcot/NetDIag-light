package sim

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sort"

	"netdiag/internal/config"
	"netdiag/internal/engine"
	"netdiag/internal/probe"
	"netdiag/internal/sysapi"
)

// Scenario is one simulation script.
type Scenario struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	DurationS   int             `json:"duration_s"`
	ConfigJSON  json.RawMessage `json:"config"`  // partial config over the defaults
	Sysinfo     json.RawMessage `json:"sysinfo"` // see sysinfo
	Default     Rule            `json:"default"`
	Rules       []Rule          `json:"rules"`
	Events      []Event         `json:"events"`
	Expect      []Expect        `json:"expect"`

	cfg  *config.Config
	base map[string]any
}

// Rule sets probe results for matching series and times. Every field is
// optional; rules apply in order, later ones overriding the fields they set.
type Rule struct {
	Probe    string    `json:"probe"`  // probe ID or "*"
	Probes   []string  `json:"probes"` // several probe IDs
	Target   string    `json:"target"` // series target, or the last ":"-part of the series ID
	FromS    *int      `json:"from_s"` // inclusive, default 0
	ToS      *int      `json:"to_s"`   // exclusive, default end
	Result   string    `json:"result"` // ok, fail, skipped, captive, dns
	RTTms    []float64 `json:"rtt_ms"` // [mean, stddev]
	LossPct  *float64  `json:"loss_pct"`
	ARPState string    `json:"arp_state"` // LAN-02 only
	Value    *float64  `json:"value"`     // MTU-01 MTU, SYS-01 CPU %, NAT-01 1 = symmetric
	ErrClass string    `json:"err_class"`
	Bound    string    `json:"bound"` // MTU-01: signalled (default) or silent
	Detail   string    `json:"detail"`
	// Pattern is a per-second sequence of o (ok), f (fail), s (skipped),
	// c (captive), repeated from from_s.
	Pattern string `json:"pattern"`
}

// Event changes system state or the CSV error state at a time.
type Event struct {
	AtS      int             `json:"at_s"`
	Sysinfo  json.RawMessage `json:"sysinfo"`
	CSVError *bool           `json:"csv_error"`
}

// Expect is checked after the samples of second at_s.
type Expect struct {
	AtS            int               `json:"at_s"`
	Verdict        string            `json:"verdict"`
	ExitCode       *int              `json:"exit_code"`
	DetailContains string            `json:"detail_contains"`
	FlagsInclude   []string          `json:"flags_include"`
	FlagsExclude   []string          `json:"flags_exclude"`
	Skipped        []string          `json:"skipped"` // probe IDs whose newest samples are all skipped
	Gates          map[string]string `json:"gates"`   // "L3": "closed"
	QualitySource  string            `json:"quality_source"`
	QualityStatus  string            `json:"quality_status"`
	AdapterChanges *int              `json:"adapter_changes"`
	PrimaryAdapter string            `json:"primary_adapter"`
}

// sysinfo is the scenario's view of the OS state.
type sysinfo struct {
	Adapter struct {
		Name     string `json:"name"`
		Type     string `json:"type"` // wifi, ethernet
		Up       *bool  `json:"up"`
		Hardware *bool  `json:"hardware"`
		IPv4     string `json:"ipv4"` // "192.168.1.23/24"; "" = none
	} `json:"adapter"`
	Gateway struct {
		IP  string `json:"ip"`
		MAC string `json:"mac"`
	} `json:"gateway"`
	DNSServers []string `json:"dns_servers"`
	Route      string   `json:"route"` // default, onlink, none
	WiFi       *struct {
		SSID       string `json:"ssid"`
		SSIDHidden bool   `json:"ssid_hidden"`
		SignalPct  *int   `json:"signal_pct"`
		RSSI       *int   `json:"rssi"`
		Band       string `json:"band"`        // 2.4GHz, 5GHz, 6GHz
		BandSource string `json:"band_source"` // frequency (default), channel
		Channel    *int   `json:"channel"`
	} `json:"wifi"`
}

func strictDecode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Parse reads and validates a scenario.
func Parse(data []byte) (*Scenario, error) {
	var sc Scenario
	if err := strictDecode(data, &sc); err != nil {
		return nil, fmt.Errorf("scenario: %w", err)
	}
	if sc.Name == "" || sc.DurationS <= 0 {
		return nil, fmt.Errorf("scenario: name and a positive duration_s are required")
	}
	cfgJSON := []byte("{}")
	if len(sc.ConfigJSON) > 0 {
		cfgJSON = sc.ConfigJSON
	}
	cfg, err := config.Parse(cfgJSON)
	if err != nil {
		return nil, fmt.Errorf("scenario %s: config: %w", sc.Name, err)
	}
	sc.cfg = cfg
	if err := json.Unmarshal(sc.Sysinfo, &sc.base); err != nil {
		return nil, fmt.Errorf("scenario %s: sysinfo: %w", sc.Name, err)
	}
	for _, at := range append([]int{0}, eventTimes(sc.Events)...) {
		if _, err := sc.SystemAt(at); err != nil {
			return nil, fmt.Errorf("scenario %s: %w", sc.Name, err)
		}
	}
	if err := sc.validate(); err != nil {
		return nil, fmt.Errorf("scenario %s: %w", sc.Name, err)
	}
	sort.SliceStable(sc.Events, func(i, j int) bool { return sc.Events[i].AtS < sc.Events[j].AtS })
	sort.SliceStable(sc.Expect, func(i, j int) bool { return sc.Expect[i].AtS < sc.Expect[j].AtS })
	return &sc, nil
}

func eventTimes(ev []Event) []int {
	out := make([]int, len(ev))
	for i, e := range ev {
		out[i] = e.AtS
	}
	return out
}

var results = []string{"", "ok", "fail", "skipped", "captive", "dns"}
var arpStates = []string{"", "reachable", "permanent", "probe", "delay", "stale", "incomplete", "unreachable", "busy"}

func (sc *Scenario) validate() error {
	check := func(where string, r Rule) error {
		for _, p := range append([]string{r.Probe}, r.Probes...) {
			if p != "" && p != "*" && !slices.Contains(engine.ProbeIDs, p) {
				return fmt.Errorf("%s: unknown probe %q", where, p)
			}
		}
		if !slices.Contains(results, r.Result) {
			return fmt.Errorf("%s: unknown result %q", where, r.Result)
		}
		if !slices.Contains([]string{"", probe.BoundSignalled, probe.BoundSilent}, r.Bound) {
			return fmt.Errorf("%s: unknown bound %q", where, r.Bound)
		}
		if !slices.Contains(arpStates, r.ARPState) {
			return fmt.Errorf("%s: unknown arp_state %q", where, r.ARPState)
		}
		if r.RTTms != nil && len(r.RTTms) != 2 {
			return fmt.Errorf("%s: rtt_ms must be [mean, stddev]", where)
		}
		for _, c := range r.Pattern {
			if !slices.Contains([]rune("ofsc"), c) {
				return fmt.Errorf("%s: pattern may only contain o, f, s, c", where)
			}
		}
		return nil
	}
	if err := check("default", sc.Default); err != nil {
		return err
	}
	for i, r := range sc.Rules {
		if r.Probe == "" && len(r.Probes) == 0 {
			return fmt.Errorf("rules[%d]: probe or probes required", i)
		}
		if err := check(fmt.Sprintf("rules[%d]", i), r); err != nil {
			return err
		}
	}
	for i, e := range sc.Expect {
		if e.AtS < 0 || e.AtS > sc.DurationS {
			return fmt.Errorf("expect[%d]: at_s outside the scenario", i)
		}
		for l, st := range e.Gates {
			if !slices.Contains([]string{"L1", "L2", "L3", "L4", "L5"}, l) || !slices.Contains([]string{"open", "closed", "unknown"}, st) {
				return fmt.Errorf("expect[%d]: bad gate %s=%s", i, l, st)
			}
		}
	}
	return nil
}

// Config returns the scenario's effective config.
func (sc *Scenario) Config() *config.Config { return sc.cfg }

// CSVErrorAt returns the CSV error state at second t.
func (sc *Scenario) CSVErrorAt(t int) bool {
	on := false
	for _, e := range sc.Events {
		if e.AtS <= t && e.CSVError != nil {
			on = *e.CSVError
		}
	}
	return on
}

// SystemAt returns the simulated OS state at second t: the base sysinfo
// with every event up to t merged over it.
func (sc *Scenario) SystemAt(t int) (engine.System, error) {
	si, err := sc.sysinfoAt(t)
	if err != nil {
		return engine.System{}, err
	}
	return si.system()
}

// gatewayMACAt is the scenario's gateway MAC at second t ("" if unset).
func (sc *Scenario) gatewayMACAt(t int) string {
	si, err := sc.sysinfoAt(t)
	if err != nil {
		return ""
	}
	return si.Gateway.MAC
}

func (sc *Scenario) sysinfoAt(t int) (sysinfo, error) {
	merged := deepCopy(sc.base)
	for _, e := range sc.Events {
		if e.AtS > t || len(e.Sysinfo) == 0 {
			continue
		}
		var overlay map[string]any
		if err := json.Unmarshal(e.Sysinfo, &overlay); err != nil {
			return sysinfo{}, fmt.Errorf("event at %d s: %w", e.AtS, err)
		}
		mergeInto(merged, overlay)
	}
	raw, _ := json.Marshal(merged)
	var si sysinfo
	if err := strictDecode(raw, &si); err != nil {
		return sysinfo{}, fmt.Errorf("sysinfo at %d s: %w", t, err)
	}
	return si, nil
}

const simAdapterIndex = 10

func (si sysinfo) system() (engine.System, error) {
	ad := si.Adapter
	typ := sysapi.AdapterEthernet
	name := "Ethernet"
	switch ad.Type {
	case "wifi":
		typ, name = sysapi.AdapterWiFi, "Wi-Fi"
	case "ethernet", "":
	default:
		return engine.System{}, fmt.Errorf("adapter.type %q", ad.Type)
	}
	if ad.Name != "" {
		name = ad.Name
	}
	a := sysapi.Adapter{
		Index: simAdapterIndex, GUID: "{SIM-ADAPTER}", Name: name, Description: "simulated " + string(typ),
		Type: typ, Hardware: ad.Hardware == nil || *ad.Hardware, Up: ad.Up == nil || *ad.Up, MTU: 1500,
		MAC: net.HardwareAddr{0x02, 0, 0, 0, 0, 0x10},
	}
	if ad.IPv4 != "" {
		p, err := netip.ParsePrefix(ad.IPv4)
		if err != nil {
			return engine.System{}, fmt.Errorf("adapter.ipv4: %w", err)
		}
		a.IPv4 = []netip.Prefix{p}
	}
	for _, d := range si.DNSServers {
		ip, err := netip.ParseAddr(d)
		if err != nil {
			return engine.System{}, fmt.Errorf("dns_servers: %w", err)
		}
		a.DNSServers = append(a.DNSServers, ip)
	}
	sys := engine.System{Adapters: []sysapi.Adapter{
		{Index: 1, Name: "Loopback Pseudo-Interface 1", Type: sysapi.AdapterLoopback, Up: true,
			IPv4: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/8")}},
		a,
	}}
	usable := a.Up && (sysapi.Primary{Adapter: a, Reason: sysapi.PrimaryByRoute}).Address() == sysapi.AddressOK
	switch {
	case !usable || si.Route == "none":
		sys.RouteErr = sysapi.ErrNoRoute
	case si.Route == "onlink":
		sys.Route = sysapi.Route{InterfaceIndex: simAdapterIndex}
	case si.Route == "" || si.Route == "default":
		gw, err := netip.ParseAddr(si.Gateway.IP)
		if err != nil {
			return engine.System{}, fmt.Errorf("gateway.ip is required for a default route: %w", err)
		}
		sys.Route = sysapi.Route{InterfaceIndex: simAdapterIndex, Gateway: gw}
	default:
		return engine.System{}, fmt.Errorf("route %q", si.Route)
	}
	if w := si.WiFi; w != nil && typ == sysapi.AdapterWiFi {
		st := sysapi.WiFiStatus{InterfaceGUID: a.GUID, Description: a.Description, Connected: a.Up,
			SSID: w.SSID, SSIDHidden: w.SSIDHidden, SignalPct: w.SignalPct, RSSI: w.RSSI, Channel: w.Channel,
			Band: sysapi.Band(w.Band)}
		if st.SSIDHidden {
			st.SSID = ""
		}
		switch w.BandSource {
		case "", "frequency":
			st.BandSource = sysapi.BandFromFrequency
		case "channel":
			st.BandSource = sysapi.BandFromChannelNo
		default:
			return engine.System{}, fmt.Errorf("wifi.band_source %q", w.BandSource)
		}
		if st.Band == "" {
			st.BandSource = ""
		}
		sys.WiFi = []sysapi.WiFiStatus{st}
	}
	return sys, nil
}

func deepCopy(m map[string]any) map[string]any {
	raw, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

// mergeInto merges overlay into dst: objects key by key, everything else
// replaced.
func mergeInto(dst, overlay map[string]any) {
	for k, v := range overlay {
		if om, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				mergeInto(dm, om)
				continue
			}
		}
		dst[k] = v
	}
}
