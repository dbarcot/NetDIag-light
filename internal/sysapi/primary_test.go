package sysapi

import (
	"errors"
	"net/netip"
	"testing"
)

func pfx(s string) []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix(s)} }

var (
	ethUp     = Adapter{Index: 4, Name: "Ethernet", Type: AdapterEthernet, Hardware: true, Up: true, IPv4: pfx("192.168.1.10/24")}
	ethDown   = Adapter{Index: 4, Name: "Ethernet", Type: AdapterEthernet, Hardware: true, Up: false}
	ethAPIPA  = Adapter{Index: 4, Name: "Ethernet", Type: AdapterEthernet, Hardware: true, Up: true, IPv4: pfx("169.254.7.9/16")}
	ethNoIP   = Adapter{Index: 4, Name: "Ethernet", Type: AdapterEthernet, Hardware: true, Up: true}
	wifiUp    = Adapter{Index: 9, Name: "Wi-Fi", Type: AdapterWiFi, Hardware: true, Up: true, IPv4: pfx("10.0.0.20/24")}
	wifiDown  = Adapter{Index: 9, Name: "Wi-Fi", Type: AdapterWiFi, Hardware: true, Up: false}
	loopback  = Adapter{Index: 1, Name: "Loopback", Type: AdapterLoopback, Up: true, IPv4: pfx("127.0.0.1/8")}
	vpnTunnel = Adapter{Index: 2, Name: "VPN", Type: AdapterTunnel, Up: true, IPv4: pfx("172.20.0.5/32")}
	// Hyper-V/WSL switch adapter: type ethernet, always up with an IPv4, not hardware.
	vEthernet = Adapter{Index: 30, Name: "vEthernet (WSL)", Type: AdapterEthernet, Hardware: false, Up: true, IPv4: pfx("172.28.16.1/20")}
	gw        = netip.MustParseAddr("10.0.0.1")
)

func TestSelectPrimary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		adapters []Adapter
		route    Route
		routeErr error
		want     string // adapter name, "" for none
		reason   PrimaryReason
		onLink   bool
		link     bool
		addr     AddressState
	}{
		{"cable out: eth down, wifi down", []Adapter{loopback, ethDown, wifiDown}, Route{}, ErrNoRoute,
			"Ethernet", PrimaryByFallback, false, false, AddressNone},
		{"APIPA: no route, eth up with 169.254", []Adapter{loopback, ethAPIPA, wifiDown}, Route{}, ErrNoRoute,
			"Ethernet", PrimaryByFallback, false, true, AddressAPIPA},
		{"eth up without any IPv4", []Adapter{ethNoIP}, Route{}, ErrNoRoute,
			"Ethernet", PrimaryByFallback, false, true, AddressNone},
		{"Wi-Fi only", []Adapter{loopback, wifiUp}, Route{InterfaceIndex: 9, Gateway: gw}, nil,
			"Wi-Fi", PrimaryByRoute, false, true, AddressOK},
		{"eth and wifi up, route on wifi wins", []Adapter{ethUp, wifiUp}, Route{InterfaceIndex: 9, Gateway: gw}, nil,
			"Wi-Fi", PrimaryByRoute, false, true, AddressOK},
		{"route through VPN tunnel is used", []Adapter{ethUp, vpnTunnel}, Route{InterfaceIndex: 2}, nil,
			"VPN", PrimaryByRoute, true, true, AddressOK},
		{"tunnel ignored in fallback", []Adapter{vpnTunnel, loopback, wifiDown}, Route{}, ErrNoRoute,
			"Wi-Fi", PrimaryByFallback, false, false, AddressNone},
		{"cable out with an up vEthernet: physical NIC chosen, NO_LINK", []Adapter{vEthernet, ethDown, loopback}, Route{}, ErrNoRoute,
			"Ethernet", PrimaryByFallback, false, false, AddressNone},
		{"only virtual adapters: none", []Adapter{vEthernet, loopback}, Route{}, ErrNoRoute,
			"", PrimaryNone, false, false, AddressNone},
		{"route through vEthernet is still used", []Adapter{vEthernet, ethDown}, Route{InterfaceIndex: 30}, nil,
			"vEthernet (WSL)", PrimaryByRoute, true, true, AddressOK},
		{"on-link route", []Adapter{ethUp}, Route{InterfaceIndex: 4}, nil,
			"Ethernet", PrimaryByRoute, true, true, AddressOK},
		{"fallback prefers up over down", []Adapter{ethDown, wifiUp}, Route{}, ErrNoRoute,
			"Wi-Fi", PrimaryByFallback, false, true, AddressOK},
		{"fallback prefers IPv4 over none", []Adapter{ethNoIP, wifiUp}, Route{}, ErrNoRoute,
			"Wi-Fi", PrimaryByFallback, false, true, AddressOK},
		{"fallback prefers ethernet over wifi", []Adapter{wifiUp, ethUp}, Route{}, ErrNoRoute,
			"Ethernet", PrimaryByFallback, false, true, AddressOK},
		{"fallback lowest index as tie-break", []Adapter{{Index: 12, Name: "Ethernet 2", Type: AdapterEthernet, Hardware: true, Up: true},
			{Index: 3, Name: "Ethernet 1", Type: AdapterEthernet, Hardware: true, Up: true}}, Route{}, ErrNoRoute,
			"Ethernet 1", PrimaryByFallback, false, true, AddressNone},
		{"other route error falls back", []Adapter{ethUp}, Route{}, errors.New("GetBestRoute2: boom"),
			"Ethernet", PrimaryByFallback, false, true, AddressOK},
		{"route to unknown interface falls back", []Adapter{ethUp}, Route{InterfaceIndex: 77, Gateway: gw}, nil,
			"Ethernet", PrimaryByFallback, false, true, AddressOK},
		{"no physical adapter", []Adapter{loopback, vpnTunnel}, Route{}, ErrNoRoute,
			"", PrimaryNone, false, false, AddressNone},
		{"no adapters", nil, Route{}, ErrNoRoute,
			"", PrimaryNone, false, false, AddressNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := SelectPrimary(tt.adapters, tt.route, tt.routeErr)
			if p.Adapter.Name != tt.want || p.Reason != tt.reason || p.OnLink != tt.onLink {
				t.Errorf("got %q reason=%s onLink=%v; want %q %s %v", p.Adapter.Name, p.Reason, p.OnLink, tt.want, tt.reason, tt.onLink)
			}
			if p.LinkUp() != tt.link || p.Address() != tt.addr {
				t.Errorf("LinkUp=%v Address=%s; want %v %s", p.LinkUp(), p.Address(), tt.link, tt.addr)
			}
			if p.Found() != (tt.reason != PrimaryNone) {
				t.Error("Found inconsistent with reason")
			}
		})
	}
}

func TestSelectPrimary_GatewayFromRoute(t *testing.T) {
	t.Parallel()
	p := SelectPrimary([]Adapter{wifiUp}, Route{InterfaceIndex: 9, Gateway: gw}, nil)
	if p.Gateway != gw || p.OnLink {
		t.Errorf("gateway=%v onLink=%v", p.Gateway, p.OnLink)
	}
	p = SelectPrimary([]Adapter{wifiUp}, Route{}, ErrNoRoute)
	if p.Gateway.IsValid() {
		t.Error("fallback has no gateway")
	}
}

func TestPrimary_AddressMixedAPIPAAndValid(t *testing.T) {
	t.Parallel()
	a := Adapter{Up: true, Type: AdapterEthernet, IPv4: []netip.Prefix{
		netip.MustParsePrefix("169.254.1.1/16"), netip.MustParsePrefix("192.168.0.5/24"),
	}}
	if got := (Primary{Adapter: a, Reason: PrimaryByRoute}).Address(); got != AddressOK {
		t.Errorf("Address = %s, want ok", got)
	}
}
