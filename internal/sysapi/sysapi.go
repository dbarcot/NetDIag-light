package sysapi

import (
	"context"
	"errors"
	"net"
	"net/netip"
)

// Errors shared by implementations and fakes.
var (
	ErrNoRoute    = errors.New("no route to destination")
	ErrUnresolved = errors.New("neighbor address not resolved")
	ErrNotConsole = errors.New("not a console")
)

// AdapterType is a coarse adapter classification.
type AdapterType string

const (
	AdapterEthernet AdapterType = "ethernet"
	AdapterWiFi     AdapterType = "wifi"
	AdapterLoopback AdapterType = "loopback"
	AdapterTunnel   AdapterType = "tunnel"
	AdapterPPP      AdapterType = "ppp"
	AdapterOther    AdapterType = "other"
)

// Adapter is one network adapter as reported by GetAdaptersAddresses.
type Adapter struct {
	Index       uint32 // IPv4 interface index
	LUID        uint64
	GUID        string // AdapterName, e.g. "{4D36E972-...}"; matches WiFiStatus.InterfaceGUID
	Name        string // friendly name, e.g. "Wi-Fi"
	Description string
	Type        AdapterType
	Up          bool // OperStatus == up
	MAC         net.HardwareAddr
	MTU         uint32
	IPv4        []netip.Prefix // unicast addresses with on-link prefix length
	Gateways    []netip.Addr   // IPv4 default gateways
	DNSServers  []netip.Addr   // IPv4 DNS servers
}

// Route is the best route toward a destination (GetBestRoute2).
type Route struct {
	InterfaceIndex uint32
	InterfaceLUID  uint64
	Gateway        netip.Addr // invalid (zero) when the destination is on-link
	Source         netip.Addr // local address used toward the destination
}

// Adapters lists network adapters.
type Adapters interface {
	Adapters(ctx context.Context) ([]Adapter, error)
}

// Routes finds the best route toward an IPv4 destination; ErrNoRoute when none.
type Routes interface {
	BestRoute(ctx context.Context, dest netip.Addr) (Route, error)
}

// ARP resolves an on-link IPv4 address on an interface. It returns the MAC
// with the neighbor state (check Outcome: a stale entry is "unconfirmed",
// not a success), ErrUnresolved when the neighbor does not answer, and
// ErrARPBusy when the previous call for the same address is still running.
type ARP interface {
	Resolve(ctx context.Context, ip netip.Addr, ifIndex uint32) (ARPResult, error)
}

// WiFi reports the state of every WLAN interface. No WLAN service, DLL or
// interface is a clean empty result, not an error.
type WiFi interface {
	Status(ctx context.Context) ([]WiFiStatus, error)
}

// CPU reads cumulative system CPU times.
type CPU interface {
	Times() (CPUTimes, error)
}

// Changes delivers a signal whenever IPv4 interfaces, addresses or routes
// change. Signals are coalesced; the channel closes when ctx ends.
type Changes interface {
	Watch(ctx context.Context) (<-chan struct{}, error)
}

// System bundles every OS boundary, so callers take one value and tests
// swap in fakes.
type System struct {
	Adapters Adapters
	Routes   Routes
	ARP      ARP
	WiFi     WiFi
	CPU      CPU
	Changes  Changes
	Console  Console
}

// FindAdapter returns the adapter with the given interface index.
func FindAdapter(adapters []Adapter, ifIndex uint32) (Adapter, bool) {
	for _, a := range adapters {
		if a.Index == ifIndex {
			return a, true
		}
	}
	return Adapter{}, false
}
