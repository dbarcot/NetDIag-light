// Package config holds the embedded default configuration, loads and merges
// user files over it, validates the result and writes default config files.
package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// SchemaVersion is the only config schema version this build understands.
const SchemaVersion = 1

// FileName is the config file name looked up next to the exe and in %APPDATA%\netdiag.
const FileName = "netdiag.json"

// Dependency values for custom targets (SPEC §4.2).
const (
	DependsLAN      = "lan"
	DependsInternet = "internet"
)

//go:embed defaults.json
var defaultsJSON []byte

// Config is the full configuration. Every field is present in defaults.json,
// so `save-config` writes every key expanded.
type Config struct {
	Version    int        `json:"version"`
	UI         UI         `json:"ui"`
	Window     Window     `json:"window"`
	History    History    `json:"history"`
	Gate       Gate       `json:"gate"`
	Targets    Targets    `json:"targets"`
	Teams      Teams      `json:"teams"`
	Meet       Meet       `json:"meet"`
	Rates      Rates      `json:"rates"`
	Timeouts   Timeouts   `json:"timeouts"`
	Thresholds Thresholds `json:"thresholds"`
	Console    Console    `json:"console"`
	Trace      Trace      `json:"trace"`
	Limits     Limits     `json:"limits"`
	CSV        CSV        `json:"csv"`
}

type UI struct {
	Port int `json:"port"`
}

type Window struct {
	Seconds    int `json:"seconds"`
	MinSamples int `json:"min_samples"`
}

type History struct {
	Minutes int `json:"minutes"`
}

type Gate struct {
	FailCount int `json:"fail_count"`
	OKCount   int `json:"ok_count"`
}

type Targets struct {
	Ping         []PingTarget `json:"ping"`
	TCP          []TCPTarget  `json:"tcp"`
	InternetICMP []string     `json:"internet_icmp"`
	InternetTCP  string       `json:"internet_tcp"`
	PublicDNS    []string     `json:"public_dns"`
	DNSTestName  string       `json:"dns_test_name"`
	STUN         []string     `json:"stun"`
	HTTPCheck    []HTTPCheck  `json:"http_check"`
	Services     []string     `json:"services"`
	Trace        string       `json:"trace"`
}

// PingTarget is a custom ICMP target. Host is an IPv4 address or hostname.
type PingTarget struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	Rate      int    `json:"rate"`
	Group     string `json:"group"`
	DependsOn string `json:"depends_on"`
}

// TCPTarget is a custom TCP connect target. Host is "host:port".
type TCPTarget struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	Rate      int    `json:"rate"`
	Group     string `json:"group"`
	DependsOn string `json:"depends_on"`
}

// HTTPCheck is one captive-portal check (SPEC §4.3).
type HTTPCheck struct {
	URL          string `json:"url"`
	ExpectStatus int    `json:"expect_status"`
	ExpectBody   string `json:"expect_body"`
}

type Teams struct {
	Relays   []string `json:"relays"`
	UDPPorts []int    `json:"udp_ports"`
}

type Meet struct {
	STUN []string `json:"stun"`
}

// Rates are probe intervals in seconds (SPEC §4.4).
type Rates struct {
	Link         int `json:"link"`
	LANPing      int `json:"lan_ping"`
	LANARP       int `json:"lan_arp"`
	WiFi         int `json:"wifi"`
	CPU          int `json:"cpu"`
	InternetICMP int `json:"internet_icmp"`
	InternetTCP  int `json:"internet_tcp"`
	DNSSystem    int `json:"dns_system"`
	DNSPublic    int `json:"dns_public"`
	STUN         int `json:"stun"`
	NAT          int `json:"nat"`
	HTTP         int `json:"http"`
	Services     int `json:"services"`
	MTU          int `json:"mtu"`
	TeamsUDP     int `json:"teams_udp"`
	TeamsTCP     int `json:"teams_tcp"`
	Meet         int `json:"meet"`
	Media        int `json:"media"`
	SrcPorts     int `json:"src_ports"`
}

// Timeouts are probe timeouts in milliseconds (SPEC §4.4).
type Timeouts struct {
	ICMP int `json:"icmp"`
	ARP  int `json:"arp"`
	TCP  int `json:"tcp"`
	DNS  int `json:"dns"`
	STUN int `json:"stun"`
	HTTP int `json:"http"`
	TLS  int `json:"tls"`
}

type Thresholds struct {
	LossPct        float64 `json:"loss_pct"`
	RTTMs          float64 `json:"rtt_ms"`
	JitterMs       float64 `json:"jitter_ms"`
	MOSMin         float64 `json:"mos_min"`
	MOSGood        float64 `json:"mos_good"`
	MTUMin         int     `json:"mtu_min"`
	WiFiSignalPct  int     `json:"wifi_signal_pct"`
	CPUModeratePct int     `json:"cpu_moderate_pct"`
	CPUHighPct     int     `json:"cpu_high_pct"`
	CPUHeavyPct    int     `json:"cpu_heavy_pct"`
}

type Console struct {
	Sparkline bool `json:"sparkline"`
}

type Trace struct {
	MaxHops    int  `json:"max_hops"`
	ReverseDNS bool `json:"reverse_dns"`
	IntervalMs int  `json:"interval_ms"`
}

type Limits struct {
	MaxCustomTargets int `json:"max_custom_targets"`
}

type CSV struct {
	Dir            string `json:"dir"`
	Separator      string `json:"separator"`
	Decimal        string `json:"decimal"`
	BOM            bool   `json:"bom"`
	SummarySeconds int    `json:"summary_seconds"`
	Rotate         string `json:"rotate"`
	MaxMB          int    `json:"max_mb"`
}

// Default returns a fresh copy of the embedded defaults.
func Default() *Config {
	var c Config
	if err := json.Unmarshal(defaultsJSON, &c); err != nil {
		panic(fmt.Sprintf("config: embedded defaults are invalid: %v", err))
	}
	return &c
}

// Marshal returns the config as indented JSON with every key expanded.
func (c *Config) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
