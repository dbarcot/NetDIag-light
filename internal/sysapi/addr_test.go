package sysapi

import (
	"net/netip"
	"testing"
)

func TestAddressClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ip                    string
		apipa, private, local bool
	}{
		{"169.254.0.1", true, false, true},
		{"169.254.255.254", true, false, true},
		{"169.253.255.255", false, false, false},
		{"10.0.0.5", false, true, true},
		{"10.255.255.255", false, true, true},
		{"172.16.0.1", false, true, true},
		{"172.31.255.255", false, true, true},
		{"172.32.0.1", false, false, false},
		{"172.15.255.255", false, false, false},
		{"192.168.1.1", false, true, true},
		{"192.169.0.1", false, false, false},
		{"127.0.0.1", false, false, true},
		{"100.64.0.1", false, false, false},
		{"1.1.1.1", false, false, false},
		{"fe80::1", false, false, false},
		{"fd00::1", false, false, false},
	}
	for _, tt := range tests {
		a := netip.MustParseAddr(tt.ip)
		if got := IsAPIPA(a); got != tt.apipa {
			t.Errorf("IsAPIPA(%s) = %v, want %v", tt.ip, got, tt.apipa)
		}
		if got := IsPrivateIPv4(a); got != tt.private {
			t.Errorf("IsPrivateIPv4(%s) = %v, want %v", tt.ip, got, tt.private)
		}
		if got := IsLocalIPv4(a); got != tt.local {
			t.Errorf("IsLocalIPv4(%s) = %v, want %v", tt.ip, got, tt.local)
		}
	}
}
