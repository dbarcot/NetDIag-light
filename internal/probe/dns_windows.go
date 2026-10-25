//go:build windows

package probe

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	moddnsapi             = windows.NewLazySystemDLL("dnsapi.dll")
	procDnsQueryW         = moddnsapi.NewProc("DnsQuery_W")
	procDnsRecordListFree = moddnsapi.NewProc("DnsRecordListFree")
)

const (
	dnsTypeA            = 1
	dnsQueryBypassCache = 0x8 // DNS_QUERY_BYPASS_CACHE
	dnsFreeRecordList   = 1   // DNS_FREE_TYPE DnsFreeRecordList

	// DNS_STATUS values (windns.h)
	dnsErrRcodeFormatError    = 9001
	dnsErrRcodeServerFailure  = 9002
	dnsErrRcodeNameError      = 9003
	dnsErrRcodeNotImplemented = 9004
	dnsErrRcodeRefused        = 9005
	dnsInfoNoRecords          = 9501
	dnsErrRecordTimedOut      = 9705
)

// ip4Array is IP4_ARRAY with one server: the pExtra of DnsQuery_W.
type ip4Array struct {
	AddrCount uint32
	AddrArray [1]uint32
}

// dnsRecord is the head of DNS_RECORDW (64-bit); Data holds the A address
// (IP4_ADDRESS, bytes in network order) at its start.
type dnsRecord struct {
	Next     *dnsRecord
	Name     *uint16
	Type     uint16
	DataLen  uint16
	Flags    uint32
	TTL      uint32
	Reserved uint32
	Data     [16]byte
}

// WinDNS implements DNSQuerier for DNS-01 with DnsQuery_W and
// DNS_QUERY_BYPASS_CACHE, sending the query to the one server passed in
// pExtra (an IP4_ARRAY), so each adapter DNS server is tested on its own.
// Go's net.Resolver is never used here: it goes through GetAddrInfoW and
// the DNS client cache.
//
// DnsQuery_W cannot be cancelled or given a timeout: when the probe's
// timeout or ctx ends first the call keeps running in the background, and
// further queries to that server get ErrDNSBusy until it returns.
type WinDNS struct {
	mu       sync.Mutex
	inflight map[netip.Addr]bool
}

type dnsResult struct {
	ans DNSAnswer
	err error
}

func (w *WinDNS) Query(ctx context.Context, server netip.Addr, name string, timeout time.Duration) (DNSAnswer, error) {
	if !server.Is4() {
		return DNSAnswer{}, fmt.Errorf("%w: DNS server %s is not IPv4", ErrLocal, server)
	}
	w.mu.Lock()
	if w.inflight == nil {
		w.inflight = map[netip.Addr]bool{}
	}
	if w.inflight[server] {
		w.mu.Unlock()
		return DNSAnswer{}, ErrDNSBusy
	}
	w.inflight[server] = true
	w.mu.Unlock()

	done := make(chan dnsResult, 1)
	go func() {
		ans, err := dnsQuery(server, name)
		w.mu.Lock()
		delete(w.inflight, server)
		w.mu.Unlock()
		done <- dnsResult{ans, err}
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r := <-done:
		return r.ans, r.err
	case <-t.C:
		return DNSAnswer{}, context.DeadlineExceeded
	case <-ctx.Done():
		return DNSAnswer{}, ctx.Err()
	}
}

func dnsQuery(server netip.Addr, name string) (DNSAnswer, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return DNSAnswer{}, fmt.Errorf("%w: %w", ErrLocal, err)
	}
	b := server.As4()
	extra := ip4Array{AddrCount: 1, AddrArray: [1]uint32{ipAddrLE(b)}}
	var rec *dnsRecord
	status, _, _ := procDnsQueryW.Call(
		uintptr(unsafe.Pointer(namePtr)), dnsTypeA, dnsQueryBypassCache,
		uintptr(unsafe.Pointer(&extra)), uintptr(unsafe.Pointer(&rec)), 0)
	if rec != nil {
		defer procDnsRecordListFree.Call(uintptr(unsafe.Pointer(rec)), dnsFreeRecordList) //nolint:errcheck // nothing to do on failure
	}
	if status != 0 {
		return dnsStatusAnswer(uint32(status))
	}
	var ans DNSAnswer
	for r := rec; r != nil; r = r.Next {
		if r.Type == dnsTypeA {
			ans.Addrs = append(ans.Addrs, netip.AddrFrom4([4]byte(r.Data[:4])))
		}
	}
	return ans, nil
}

// ipAddrLE is the in-memory IP4_ADDRESS of an address: its bytes in
// network order, read as a little-endian DWORD.
func ipAddrLE(b [4]byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// dnsStatusAnswer maps a failed DNS_STATUS: the server's error codes are
// an answer with an Rcode, timeouts and everything else an error.
func dnsStatusAnswer(status uint32) (DNSAnswer, error) {
	switch status {
	case dnsErrRcodeNameError:
		return DNSAnswer{Rcode: "NXDOMAIN"}, nil
	case dnsErrRcodeServerFailure:
		return DNSAnswer{Rcode: "SERVFAIL"}, nil
	case dnsErrRcodeRefused:
		return DNSAnswer{Rcode: "REFUSED"}, nil
	case dnsErrRcodeFormatError:
		return DNSAnswer{Rcode: "FORMERR"}, nil
	case dnsErrRcodeNotImplemented:
		return DNSAnswer{Rcode: "NOTIMP"}, nil
	case dnsInfoNoRecords:
		return DNSAnswer{}, nil // NOERROR without an A record
	case dnsErrRecordTimedOut, uint32(windows.ERROR_TIMEOUT), uint32(windows.WSAETIMEDOUT):
		return DNSAnswer{}, context.DeadlineExceeded
	default:
		return DNSAnswer{}, windows.Errno(status)
	}
}
