//go:build windows

package sysapi

import (
	"fmt"
	"net"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// NewWiFi returns the Windows WLAN implementation.
func NewWiFi() WLANWiFi { return WLANWiFi{open: openWLAN} }

const (
	wlanClientVersion           = 2 // Vista+ API
	wlanInterfaceStateConnected = 1
	wlanOpcodeCurrentConnection = 7
	wlanOpcodeChannelNumber     = 8
	wlanOpcodeRSSI              = 0x10000102
	dot11BSSTypeAny             = 3
)

// wlanInterfaceInfo is WLAN_INTERFACE_INFO (wlanapi.h), 532 bytes.
type wlanInterfaceInfo struct {
	InterfaceGUID windows.GUID
	Description   [256]uint16
	State         uint32
}

type dot11SSID struct {
	Length uint32
	SSID   [32]byte
}

// wlanConnectionAttributes is WLAN_CONNECTION_ATTRIBUTES with the nested
// association and security attributes flattened, 604 bytes.
type wlanConnectionAttributes struct {
	State          uint32
	ConnectionMode uint32
	ProfileName    [256]uint16
	// WLAN_ASSOCIATION_ATTRIBUTES
	SSID          dot11SSID
	BSSType       uint32
	BSSID         [6]byte
	PhyType       uint32
	PhyIndex      uint32
	SignalQuality uint32
	RxRate        uint32
	TxRate        uint32
	// WLAN_SECURITY_ATTRIBUTES
	SecurityEnabled int32
	OneXEnabled     int32
	AuthAlgorithm   uint32
	CipherAlgorithm uint32
}

// wlanBSSEntry is WLAN_BSS_ENTRY, 360 bytes.
type wlanBSSEntry struct {
	SSID               dot11SSID
	PhyID              uint32
	BSSID              [6]byte
	BSSType            uint32
	PhyType            uint32
	RSSI               int32
	LinkQuality        uint32
	InRegDomain        uint8
	BeaconPeriod       uint16
	Timestamp          uint64
	HostTimestamp      uint64
	CapabilityInfo     uint16
	CenterFrequencyKHz uint32
	RateSetLength      uint32
	RateSet            [126]uint16
	IEOffset           uint32
	IESize             uint32
}

type winWLAN struct {
	handle windows.Handle
	guids  map[string]windows.GUID
}

func openWLAN() (wlanAPI, error) {
	if err := procWlanOpenHandle.Find(); err != nil {
		return nil, errWLANUnavailable
	}
	var negotiated uint32
	var h windows.Handle
	r, _, _ := procWlanOpenHandle.Call(wlanClientVersion, 0, uintptr(unsafe.Pointer(&negotiated)), uintptr(unsafe.Pointer(&h)))
	switch errno := windows.Errno(r); errno {
	case 0:
		return &winWLAN{handle: h, guids: map[string]windows.GUID{}}, nil
	case windows.ERROR_SERVICE_NOT_ACTIVE, windows.ERROR_SERVICE_DOES_NOT_EXIST, windows.ERROR_SERVICE_DISABLED:
		return nil, errWLANUnavailable
	default:
		return nil, fmt.Errorf("WlanOpenHandle: %w", errno)
	}
}

func (w *winWLAN) close() {
	_, _, _ = procWlanCloseHandle.Call(uintptr(w.handle), 0)
}

func (w *winWLAN) interfaces() ([]wlanIface, error) {
	var list unsafe.Pointer
	r, _, _ := procWlanEnumInterfaces.Call(uintptr(w.handle), 0, uintptr(unsafe.Pointer(&list)))
	if r != 0 {
		return nil, fmt.Errorf("WlanEnumInterfaces: %w", windows.Errno(r))
	}
	defer wlanFree(list)
	n := *(*uint32)(list)
	items := unsafe.Slice((*wlanInterfaceInfo)(unsafe.Add(list, 8)), n)
	out := make([]wlanIface, 0, n)
	for _, it := range items {
		g := strings.ToUpper(it.InterfaceGUID.String())
		w.guids[g] = it.InterfaceGUID
		out = append(out, wlanIface{
			GUID:        g,
			Description: windows.UTF16ToString(it.Description[:]),
			Connected:   it.State == wlanInterfaceStateConnected,
		})
	}
	return out, nil
}

// query runs WlanQueryInterface and passes the returned buffer to read
// before freeing it.
func (w *winWLAN) query(guid string, opcode uint32, read func(data unsafe.Pointer, size uint32)) error {
	g, ok := w.guids[guid]
	if !ok {
		return fmt.Errorf("unknown WLAN interface %s", guid)
	}
	var size uint32
	var data unsafe.Pointer
	r, _, _ := procWlanQueryInterface.Call(uintptr(w.handle), uintptr(unsafe.Pointer(&g)), uintptr(opcode), 0,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&data)), 0)
	if r != 0 {
		// ERROR_ACCESS_DENIED matches fs.ErrPermission (location privacy).
		return fmt.Errorf("WlanQueryInterface(%#x): %w", opcode, windows.Errno(r))
	}
	defer wlanFree(data)
	read(data, size)
	return nil
}

func (w *winWLAN) connection(guid string) (wlanConn, error) {
	var c wlanConn
	err := w.query(guid, wlanOpcodeCurrentConnection, func(data unsafe.Pointer, size uint32) {
		if size < uint32(unsafe.Sizeof(wlanConnectionAttributes{})) {
			return
		}
		a := (*wlanConnectionAttributes)(data)
		c = wlanConn{
			SSID:          ssidString(a.SSID),
			BSSID:         append(net.HardwareAddr(nil), a.BSSID[:]...),
			SignalQuality: int(a.SignalQuality),
		}
	})
	return c, err
}

func (w *winWLAN) rssi(guid string) (int, error) {
	var v int
	err := w.query(guid, wlanOpcodeRSSI, func(data unsafe.Pointer, size uint32) {
		if size >= 4 {
			v = int(*(*int32)(data))
		}
	})
	return v, err
}

func (w *winWLAN) channel(guid string) (int, error) {
	var v int
	err := w.query(guid, wlanOpcodeChannelNumber, func(data unsafe.Pointer, size uint32) {
		if size >= 4 {
			v = int(*(*uint32)(data))
		}
	})
	return v, err
}

// bssList returns the cached BSS list (no scan is triggered).
func (w *winWLAN) bssList(guid string) ([]wlanBSS, error) {
	g, ok := w.guids[guid]
	if !ok {
		return nil, fmt.Errorf("unknown WLAN interface %s", guid)
	}
	var list unsafe.Pointer
	r, _, _ := procWlanGetNetworkBssList.Call(uintptr(w.handle), uintptr(unsafe.Pointer(&g)), 0, dot11BSSTypeAny, 0, 0,
		uintptr(unsafe.Pointer(&list)))
	if r != 0 {
		return nil, fmt.Errorf("WlanGetNetworkBssList: %w", windows.Errno(r))
	}
	defer wlanFree(list)
	n := *(*uint32)(unsafe.Add(list, 4))
	out := make([]wlanBSS, 0, n)
	for _, e := range unsafe.Slice((*wlanBSSEntry)(unsafe.Add(list, 8)), n) {
		out = append(out, wlanBSS{
			BSSID:        append(net.HardwareAddr(nil), e.BSSID[:]...),
			FrequencyMHz: int(e.CenterFrequencyKHz / 1000),
		})
	}
	return out, nil
}

func wlanFree(p unsafe.Pointer) {
	if p != nil {
		_, _, _ = procWlanFreeMemory.Call(uintptr(p))
	}
}

func ssidString(s dot11SSID) string {
	n := min(int(s.Length), len(s.SSID))
	return string(s.SSID[:n])
}
