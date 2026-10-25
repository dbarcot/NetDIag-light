//go:build windows

package sysapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Interface types from ipifcons.h.
const (
	ifTypeEthernet  = 6
	ifTypePPP       = 23
	ifTypeLoopback  = 24
	ifTypeIEEE80211 = 71
	ifTypeTunnel    = 131
)

func adapterType(ifType uint32) AdapterType {
	switch ifType {
	case ifTypeEthernet:
		return AdapterEthernet
	case ifTypeIEEE80211:
		return AdapterWiFi
	case ifTypeLoopback:
		return AdapterLoopback
	case ifTypeTunnel:
		return AdapterTunnel
	case ifTypePPP:
		return AdapterPPP
	default:
		return AdapterOther
	}
}

// WinAdapters implements Adapters with GetAdaptersAddresses.
type WinAdapters struct{}

func (WinAdapters) Adapters(ctx context.Context) ([]Adapter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	const flags = windows.GAA_FLAG_INCLUDE_GATEWAYS | windows.GAA_FLAG_SKIP_ANYCAST | windows.GAA_FLAG_SKIP_MULTICAST
	size := uint32(16 * 1024)
	for {
		buf := make([]byte, size)
		first := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, flags, 0, first, &size)
		if errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
			continue // size now holds the required length
		}
		if errors.Is(err, windows.ERROR_NO_DATA) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("GetAdaptersAddresses: %w", err)
		}
		var out []Adapter
		for a := first; a != nil; a = a.Next {
			out = append(out, convertAdapter(a))
		}
		return out, nil
	}
}

func convertAdapter(a *windows.IpAdapterAddresses) Adapter {
	ad := Adapter{
		Index:       a.IfIndex,
		LUID:        a.Luid,
		GUID:        windows.BytePtrToString(a.AdapterName),
		Name:        windows.UTF16PtrToString(a.FriendlyName),
		Description: windows.UTF16PtrToString(a.Description),
		Type:        adapterType(a.IfType),
		Up:          a.OperStatus == windows.IfOperStatusUp,
		MTU:         a.Mtu,
	}
	if n := a.PhysicalAddressLength; n > 0 && int(n) <= len(a.PhysicalAddress) {
		ad.MAC = append(net.HardwareAddr(nil), a.PhysicalAddress[:n]...)
	}
	for u := a.FirstUnicastAddress; u != nil; u = u.Next {
		if ip, ok := sockaddrIPv4(u.Address); ok {
			ad.IPv4 = append(ad.IPv4, netip.PrefixFrom(ip, int(u.OnLinkPrefixLength)))
		}
	}
	for g := a.FirstGatewayAddress; g != nil; g = g.Next {
		if ip, ok := sockaddrIPv4(g.Address); ok {
			ad.Gateways = append(ad.Gateways, ip)
		}
	}
	for d := a.FirstDnsServerAddress; d != nil; d = d.Next {
		if ip, ok := sockaddrIPv4(d.Address); ok {
			ad.DNSServers = append(ad.DNSServers, ip)
		}
	}
	return ad
}

func sockaddrIPv4(sa windows.SocketAddress) (netip.Addr, bool) {
	if sa.Sockaddr == nil || sa.Sockaddr.Addr.Family != windows.AF_INET {
		return netip.Addr{}, false
	}
	raw := (*windows.RawSockaddrInet4)(unsafe.Pointer(sa.Sockaddr))
	return netip.AddrFrom4(raw.Addr), true
}

func rawInetIPv4(sa *windows.RawSockaddrInet) (netip.Addr, bool) {
	if sa.Family != windows.AF_INET {
		return netip.Addr{}, false
	}
	return netip.AddrFrom4((*windows.RawSockaddrInet4)(unsafe.Pointer(sa)).Addr), true
}

func toRawInet(ip netip.Addr) windows.RawSockaddrInet {
	var sa windows.RawSockaddrInet
	v4 := (*windows.RawSockaddrInet4)(unsafe.Pointer(&sa))
	v4.Family = windows.AF_INET
	v4.Addr = ip.As4()
	return sa
}

// WinRoutes implements Routes with GetBestRoute2.
type WinRoutes struct{}

func (WinRoutes) BestRoute(ctx context.Context, dest netip.Addr) (Route, error) {
	if err := ctx.Err(); err != nil {
		return Route{}, err
	}
	if !dest.Is4() {
		return Route{}, fmt.Errorf("BestRoute: %s is not IPv4", dest)
	}
	sa := toRawInet(dest)
	var row windows.MibIpForwardRow2
	var src windows.RawSockaddrInet
	r, _, _ := procGetBestRoute2.Call(
		0, 0, 0, // any interface, any source
		uintptr(unsafe.Pointer(&sa)),
		0,
		uintptr(unsafe.Pointer(&row)),
		uintptr(unsafe.Pointer(&src)),
	)
	switch errno := windows.Errno(r); errno {
	case 0:
	case windows.ERROR_NETWORK_UNREACHABLE, windows.ERROR_NOT_FOUND, windows.ERROR_HOST_UNREACHABLE:
		return Route{}, ErrNoRoute
	default:
		return Route{}, fmt.Errorf("GetBestRoute2: %w", errno)
	}
	rt := Route{InterfaceIndex: row.InterfaceIndex, InterfaceLUID: row.InterfaceLuid}
	if gw, ok := rawInetIPv4(&row.NextHop); ok && !gw.IsUnspecified() {
		rt.Gateway = gw
	}
	if s, ok := rawInetIPv4(&src); ok {
		rt.Source = s
	}
	return rt, nil
}

// mibIPNetRow2 is MIB_IPNET_ROW2 (netioapi.h), 88 bytes.
type mibIPNetRow2 struct {
	Address               windows.RawSockaddrInet
	InterfaceIndex        uint32
	InterfaceLuid         uint64
	PhysicalAddress       [32]byte
	PhysicalAddressLength uint32
	State                 uint32
	Flags                 uint8
	ReachabilityTime      uint32
}

// NL_NEIGHBOR_STATE values that mean "no MAC known".
const (
	nlnsUnreachable = 0
	nlnsIncomplete  = 1
)

// WinARP implements ARP with ResolveIpNetEntry2, which sends an ARP
// request and blocks until it is answered or times out. No admin needed.
type WinARP struct{}

func (WinARP) Resolve(ctx context.Context, ip netip.Addr, ifIndex uint32) (net.HardwareAddr, error) {
	if !ip.Is4() {
		return nil, fmt.Errorf("ARP: %s is not IPv4", ip)
	}
	type result struct {
		mac net.HardwareAddr
		err error
	}
	// The call cannot be cancelled; on ctx end it finishes in the
	// background within the OS ARP timeout (a few seconds).
	done := make(chan result, 1)
	go func() {
		mac, err := resolveIPNet(ip, ifIndex)
		done <- result{mac, err}
	}()
	select {
	case r := <-done:
		return r.mac, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func resolveIPNet(ip netip.Addr, ifIndex uint32) (net.HardwareAddr, error) {
	row := mibIPNetRow2{Address: toRawInet(ip), InterfaceIndex: ifIndex}
	r, _, _ := procResolveIPNetEntry2.Call(uintptr(unsafe.Pointer(&row)), 0)
	switch errno := windows.Errno(r); errno {
	case 0:
	case windows.ERROR_BAD_NET_NAME, windows.ERROR_NOT_FOUND, windows.ERROR_NETWORK_UNREACHABLE, windows.ERROR_HOST_UNREACHABLE:
		return nil, ErrUnresolved
	default:
		return nil, fmt.Errorf("ResolveIpNetEntry2: %w", errno)
	}
	n := row.PhysicalAddressLength
	if n == 0 || n > uint32(len(row.PhysicalAddress)) || row.State == nlnsUnreachable || row.State == nlnsIncomplete {
		return nil, ErrUnresolved
	}
	return append(net.HardwareAddr(nil), row.PhysicalAddress[:n]...), nil
}

// WinCPU implements CPU with GetSystemTimes.
type WinCPU struct{}

func (WinCPU) Times() (CPUTimes, error) {
	var idle, kernel, user windows.Filetime
	r, _, err := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r == 0 {
		return CPUTimes{}, fmt.Errorf("GetSystemTimes: %w", err)
	}
	return CPUTimes{Idle: filetime64(idle), Kernel: filetime64(kernel), User: filetime64(user)}, nil
}

func filetime64(f windows.Filetime) uint64 {
	return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime)
}
