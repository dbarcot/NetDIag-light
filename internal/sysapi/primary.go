package sysapi

import (
	"cmp"
	"net/netip"
	"slices"
)

// RouteProbeTarget is the destination whose best route defines the primary
// adapter and gateway (SPEC §5 LAN-01).
var RouteProbeTarget = netip.MustParseAddr("1.1.1.1")

// PrimaryReason says how the primary adapter was chosen.
type PrimaryReason string

const (
	PrimaryByRoute    PrimaryReason = "route"             // best route toward RouteProbeTarget
	PrimaryByFallback PrimaryReason = "no-route-fallback" // no usable route: best physical adapter
	PrimaryNone       PrimaryReason = "none"              // no candidate at all
)

// Primary is the adapter all core tests run over (SPEC §5 LINK-01).
type Primary struct {
	Adapter Adapter
	Reason  PrimaryReason
	// Gateway is the route's next hop; invalid when there is no route or
	// the route is on-link.
	Gateway netip.Addr
	// OnLink is set when a route exists but has no gateway (destination
	// reachable directly on the link).
	OnLink bool
}

// Found reports whether an adapter was chosen.
func (p Primary) Found() bool { return p.Reason != PrimaryNone }

// SelectPrimary picks the primary adapter. With a route, it is the
// route's interface (whatever type, e.g. a VPN adapter Windows routes
// through). Without one — ErrNoRoute, any other route error, or a route
// to an interface missing from the list — it falls back to physical
// adapters only (ethernet, wifi): up first, then with any IPv4, then
// ethernet before wifi, then lowest index.
func SelectPrimary(adapters []Adapter, route Route, routeErr error) Primary {
	if routeErr == nil {
		if a, ok := FindAdapter(adapters, route.InterfaceIndex); ok {
			p := Primary{Adapter: a, Reason: PrimaryByRoute, Gateway: route.Gateway}
			p.OnLink = !route.Gateway.IsValid()
			return p
		}
	}
	var phys []Adapter
	for _, a := range adapters {
		if a.Type == AdapterEthernet || a.Type == AdapterWiFi {
			phys = append(phys, a)
		}
	}
	if len(phys) == 0 {
		return Primary{Reason: PrimaryNone}
	}
	slices.SortStableFunc(phys, func(a, b Adapter) int {
		return cmp.Or(
			boolFirst(a.Up, b.Up),
			boolFirst(len(a.IPv4) > 0, len(b.IPv4) > 0),
			boolFirst(a.Type == AdapterEthernet, b.Type == AdapterEthernet),
			cmp.Compare(a.Index, b.Index),
		)
	})
	return Primary{Adapter: phys[0], Reason: PrimaryByFallback}
}

// boolFirst orders true before false.
func boolFirst(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return -1
	default:
		return 1
	}
}

// AddressState classifies the primary adapter's IPv4 configuration (L2).
type AddressState string

const (
	AddressOK    AddressState = "ok"    // at least one non-APIPA IPv4
	AddressAPIPA AddressState = "apipa" // only 169.254.x.x
	AddressNone  AddressState = "none"  // no IPv4 at all
)

// Address returns the L2 state of the primary adapter.
func (p Primary) Address() AddressState {
	apipa := false
	for _, pfx := range p.Adapter.IPv4 {
		if IsAPIPA(pfx.Addr()) {
			apipa = true
			continue
		}
		return AddressOK
	}
	if apipa {
		return AddressAPIPA
	}
	return AddressNone
}

// LinkUp reports whether L1 is open: a primary adapter exists and is up.
func (p Primary) LinkUp() bool { return p.Found() && p.Adapter.Up }
