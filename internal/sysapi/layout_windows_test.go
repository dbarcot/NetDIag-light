//go:build windows

package sysapi

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Hand-written Win32 structs must match the C layout exactly.
func TestWin32StructLayouts(t *testing.T) {
	t.Parallel()
	var nr mibIPNetRow2
	var kr inputRecord
	checks := []struct {
		name      string
		got, want uintptr
	}{
		{"sizeof(SOCKADDR_INET)", unsafe.Sizeof(windows.RawSockaddrInet{}), 28},
		{"sizeof(MIB_IPFORWARD_ROW2)", unsafe.Sizeof(windows.MibIpForwardRow2{}), 104},
		{"sizeof(MIB_IF_ROW2)", unsafe.Sizeof(windows.MibIfRow2{}), 1352},
		{"MIB_IF_ROW2.InterfaceAndOperStatusFlags", unsafe.Offsetof(windows.MibIfRow2{}.InterfaceAndOperStatusFlags), 1152},
		{"MIB_IF_ROW2.OperStatus", unsafe.Offsetof(windows.MibIfRow2{}.OperStatus), 1156},
		{"sizeof(MIB_IPNET_ROW2)", unsafe.Sizeof(nr), 88},
		{"MIB_IPNET_ROW2.InterfaceIndex", unsafe.Offsetof(nr.InterfaceIndex), 28},
		{"MIB_IPNET_ROW2.InterfaceLuid", unsafe.Offsetof(nr.InterfaceLuid), 32},
		{"MIB_IPNET_ROW2.PhysicalAddress", unsafe.Offsetof(nr.PhysicalAddress), 40},
		{"MIB_IPNET_ROW2.PhysicalAddressLength", unsafe.Offsetof(nr.PhysicalAddressLength), 72},
		{"MIB_IPNET_ROW2.State", unsafe.Offsetof(nr.State), 76},
		{"MIB_IPNET_ROW2.Flags", unsafe.Offsetof(nr.Flags), 80},
		{"MIB_IPNET_ROW2.ReachabilityTime", unsafe.Offsetof(nr.ReachabilityTime), 84},
		{"sizeof(INPUT_RECORD)", unsafe.Sizeof(kr), 20},
		{"KEY_EVENT_RECORD.KeyDown", unsafe.Offsetof(kr.KeyDown), 4},
		{"KEY_EVENT_RECORD.VirtualKeyCode", unsafe.Offsetof(kr.VirtualKeyCode), 10},
		{"KEY_EVENT_RECORD.UnicodeChar", unsafe.Offsetof(kr.UnicodeChar), 14},
		{"KEY_EVENT_RECORD.ControlKeyState", unsafe.Offsetof(kr.ControlKeyState), 16},
		{"sizeof(WLAN_CONNECTION_ATTRIBUTES)", unsafe.Sizeof(wlanConnectionAttributes{}), 604},
		{"WLAN_ASSOCIATION_ATTRIBUTES.dot11Bssid", unsafe.Offsetof(wlanConnectionAttributes{}.BSSID), 560},
		{"WLAN_ASSOCIATION_ATTRIBUTES.wlanSignalQuality", unsafe.Offsetof(wlanConnectionAttributes{}.SignalQuality), 576},
		{"WLAN_BSS_ENTRY.lRssi", unsafe.Offsetof(wlanBSSEntry{}.RSSI), 56},
		{"WLAN_BSS_ENTRY.ullTimestamp", unsafe.Offsetof(wlanBSSEntry{}.Timestamp), 72},
		{"WLAN_BSS_ENTRY.ulChCenterFrequency", unsafe.Offsetof(wlanBSSEntry{}.CenterFrequencyKHz), 92},
		{"sizeof(WLAN_INTERFACE_INFO)", unsafe.Sizeof(wlanInterfaceInfo{}), 532},
		{"sizeof(WLAN_BSS_ENTRY)", unsafe.Sizeof(wlanBSSEntry{}), 360},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}
