//go:build windows

package sysapi

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestAdapterType(t *testing.T) {
	t.Parallel()
	tests := map[uint32]AdapterType{
		6:   AdapterEthernet,
		71:  AdapterWiFi,
		24:  AdapterLoopback,
		131: AdapterTunnel,
		23:  AdapterPPP,
		1:   AdapterOther,
		243: AdapterOther,
	}
	for in, want := range tests {
		if got := adapterType(in); got != want {
			t.Errorf("adapterType(%d) = %s, want %s", in, got, want)
		}
	}
}

func TestHardwareFlags(t *testing.T) {
	t.Parallel()
	tests := map[uint8]bool{
		0x00: false, // virtual (vEthernet, host-only)
		0x01: false, // hardware without connector (e.g. virtual function)
		0x04: false, // connector only
		0x05: true,  // physical NIC
		0x15: true,  // physical NIC, media disconnected (cable out): still hardware
		0x02: false, // filter interface
	}
	for flags, want := range tests {
		if got := hardwareFlags(flags); got != want {
			t.Errorf("hardwareFlags(%#x) = %v, want %v", flags, got, want)
		}
	}
}

func TestFiletime64(t *testing.T) {
	t.Parallel()
	if got := filetime64(windows.Filetime{HighDateTime: 1, LowDateTime: 2}); got != 1<<32|2 {
		t.Errorf("filetime64 = %#x", got)
	}
}
