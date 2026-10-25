//go:build windows

package sysapi

import "golang.org/x/sys/windows"

// Win32 functions that golang.org/x/sys/windows does not wrap.
var (
	modiphlpapi            = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetBestRoute2      = modiphlpapi.NewProc("GetBestRoute2")
	procResolveIPNetEntry2 = modiphlpapi.NewProc("ResolveIpNetEntry2")

	modkernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes        = modkernel32.NewProc("GetSystemTimes")
	procGetConsoleProcessList = modkernel32.NewProc("GetConsoleProcessList")
	procReadConsoleInputW     = modkernel32.NewProc("ReadConsoleInputW")

	// wlanapi.dll is missing on Server editions without the Wireless LAN
	// feature; Find() failing means "no Wi-Fi", not an error.
	modwlanapi                = windows.NewLazySystemDLL("wlanapi.dll")
	procWlanOpenHandle        = modwlanapi.NewProc("WlanOpenHandle")
	procWlanCloseHandle       = modwlanapi.NewProc("WlanCloseHandle")
	procWlanEnumInterfaces    = modwlanapi.NewProc("WlanEnumInterfaces")
	procWlanQueryInterface    = modwlanapi.NewProc("WlanQueryInterface")
	procWlanGetNetworkBssList = modwlanapi.NewProc("WlanGetNetworkBssList")
	procWlanFreeMemory        = modwlanapi.NewProc("WlanFreeMemory")
)
