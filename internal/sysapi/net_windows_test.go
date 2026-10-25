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

func TestFiletime64(t *testing.T) {
	t.Parallel()
	if got := filetime64(windows.Filetime{HighDateTime: 1, LowDateTime: 2}); got != 1<<32|2 {
		t.Errorf("filetime64 = %#x", got)
	}
}
