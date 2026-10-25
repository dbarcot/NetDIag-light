//go:build windows

package probe

import (
	"errors"
	"net/netip"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWin32ICMPLayouts(t *testing.T) {
	t.Parallel()
	var r icmpEchoReply
	checks := []struct {
		name      string
		got, want uintptr
	}{
		{"sizeof(IP_OPTION_INFORMATION)", unsafe.Sizeof(ipOptionInformation{}), 16},
		{"sizeof(ICMP_ECHO_REPLY)", unsafe.Sizeof(r), 40},
		{"ICMP_ECHO_REPLY.Data", unsafe.Offsetof(r.Data), 16},
		{"ICMP_ECHO_REPLY.Options", unsafe.Offsetof(r.Options), 24},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestTicksToDuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ticks, freq int64
		want        time.Duration
	}{
		{10_000_000, 10_000_000, time.Second},
		{1, 10_000_000, 100 * time.Nanosecond},
		{2_345, 10_000_000, 234500 * time.Nanosecond},
		{36_000_000_000, 10_000_000, time.Hour}, // no overflow
		{5, 0, 0},
	}
	for _, tt := range tests {
		if got := ticksToDuration(tt.ticks, tt.freq); got != tt.want {
			t.Errorf("ticksToDuration(%d, %d) = %v, want %v", tt.ticks, tt.freq, got, tt.want)
		}
	}
}

func TestQPC_ResolvesSubMillisecond(t *testing.T) {
	t.Parallel()
	if qpcFrequency() < 1_000_000 {
		t.Fatalf("QPC frequency %d Hz is below 1 MHz", qpcFrequency())
	}
	start := qpcNow()
	for qpcNow() == start {
	}
	if d := qpcSince(start); d <= 0 || d > time.Millisecond {
		t.Errorf("smallest QPC step %v", d)
	}
}

func TestIPAddrByteOrder(t *testing.T) {
	t.Parallel()
	a := netip.MustParseAddr("1.2.3.4")
	if v := ipAddr(a); v != 0x04030201 {
		t.Errorf("ipAddr = %#x, want in_addr memory order 0x04030201", v)
	}
	if addrFromIPAddr(ipAddr(a)) != a {
		t.Error("round trip")
	}
}

func TestReplyFromError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		errno  windows.Errno
		status ICMPStatus
		local  bool
	}{
		{windows.Errno(StatusTimedOut), StatusTimedOut, false},
		{windows.Errno(StatusTTLExpiredTransit), StatusTTLExpiredTransit, false},
		{windows.Errno(StatusPacketTooBig), StatusPacketTooBig, false},
		{windows.ERROR_NETWORK_UNREACHABLE, StatusDestNetUnreachable, false},
		{windows.ERROR_HOST_UNREACHABLE, StatusDestHostUnreachable, false},
		{windows.ERROR_INSUFFICIENT_BUFFER, 0, true},
		{windows.Errno(StatusBufTooSmall), 0, true},
		{windows.ERROR_INVALID_PARAMETER, 0, true},
	}
	for _, tt := range tests {
		r, err := replyFromError(tt.errno)
		if tt.local {
			if !errors.Is(err, ErrLocal) {
				t.Errorf("errno %d: want local error, got %v", tt.errno, err)
			}
			continue
		}
		if err != nil || r.Status != tt.status {
			t.Errorf("errno %d: got %v, %v; want status %s", tt.errno, r.Status, err, tt.status)
		}
	}
}
