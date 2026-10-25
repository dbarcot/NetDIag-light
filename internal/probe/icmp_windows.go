//go:build windows

package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modiphlpapi         = windows.NewLazySystemDLL("iphlpapi.dll")
	procIcmpCreateFile  = modiphlpapi.NewProc("IcmpCreateFile")
	procIcmpCloseHandle = modiphlpapi.NewProc("IcmpCloseHandle")
	procIcmpSendEcho2   = modiphlpapi.NewProc("IcmpSendEcho2")
)

// ipOptionInformation is IP_OPTION_INFORMATION (64-bit), 16 bytes.
type ipOptionInformation struct {
	TTL         uint8
	TOS         uint8
	Flags       uint8
	OptionsSize uint8
	OptionsData uintptr
}

// icmpEchoReply is ICMP_ECHO_REPLY (64-bit), 40 bytes.
type icmpEchoReply struct {
	Address       uint32 // IPAddr, network byte order in memory
	Status        uint32
	RoundTripTime uint32
	DataSize      uint16
	Reserved      uint16
	Data          uintptr
	Options       ipOptionInformation
}

const ipFlagDF = 0x02

// payloadPattern is the classic Windows ping payload.
const payloadPattern = "abcdefghijklmnopqrstuvwabcdefghi"

// WinICMP sends echoes with IcmpSendEcho2 (no admin, no raw sockets).
// Each call opens its own handle, so a WinICMP is safe for concurrent use.
type WinICMP struct {
	Logger *slog.Logger // optional; RTT sanity warnings
}

func ipAddr(a netip.Addr) uint32 {
	b := a.As4()
	return binary.LittleEndian.Uint32(b[:])
}

func addrFromIPAddr(v uint32) netip.Addr {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

func (w WinICMP) Echo(ctx context.Context, req EchoRequest) (EchoReply, error) {
	if !req.Dest.Is4() {
		return EchoReply{}, fmt.Errorf("%w: %s is not IPv4", ErrLocal, req.Dest)
	}
	size := req.Size
	if size == 0 {
		size = DefaultPayloadSize
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if err := ctx.Err(); err != nil {
		return EchoReply{}, err
	}
	dl, hasDL := ctx.Deadline()
	timeout, ok := EchoTimeout(req.Timeout, dl, hasDL, time.Now())
	if !ok {
		return EchoReply{Status: StatusTimedOut}, nil
	}

	h, _, err := procIcmpCreateFile.Call()
	if windows.Handle(h) == windows.InvalidHandle {
		return EchoReply{}, fmt.Errorf("%w: IcmpCreateFile: %w", ErrLocal, err)
	}
	defer procIcmpCloseHandle.Call(h) //nolint:errcheck // nothing useful to do on close failure

	payload := make([]byte, size)
	for i := range payload {
		payload[i] = payloadPattern[i%len(payloadPattern)]
	}
	opts := ipOptionInformation{TTL: ttl}
	if req.DF {
		opts.Flags = ipFlagDF
	}
	buf := make([]byte, ReplyBufferSize(size, int(unsafe.Sizeof(icmpEchoReply{}))))

	start := qpcNow()
	n, _, callErr := procIcmpSendEcho2.Call(h, 0, 0, 0,
		uintptr(ipAddr(req.Dest)),
		uintptr(unsafe.Pointer(&payload[0])), uintptr(size),
		uintptr(unsafe.Pointer(&opts)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		uintptr(timeout.Milliseconds()))
	rtt := qpcSince(start)

	if n == 0 {
		return replyFromError(callErr)
	}
	r := (*icmpEchoReply)(unsafe.Pointer(&buf[0]))
	reply := EchoReply{
		Status: ICMPStatus(r.Status),
		From:   addrFromIPAddr(r.Address),
		RTT:    rtt,
		APIRTT: time.Duration(r.RoundTripTime) * time.Millisecond,
	}
	if reply.Status == StatusBufTooSmall {
		return EchoReply{}, fmt.Errorf("%w: reply buffer too small (%d bytes)", ErrLocal, len(buf))
	}
	if reply.Status == StatusSuccess && reply.APIRTT > 0 && rtt < reply.APIRTT-time.Millisecond && w.Logger != nil {
		w.Logger.Warn("ICMP RTT below API value", "dest", req.Dest, "go_rtt", rtt, "api_rtt", reply.APIRTT)
	}
	return reply, nil
}

// replyFromError maps the GetLastError of a call that returned no reply:
// IP_* statuses are network outcomes, everything else is a local error.
func replyFromError(err error) (EchoReply, error) {
	var errno windows.Errno
	if !errors.As(err, &errno) {
		return EchoReply{}, fmt.Errorf("%w: IcmpSendEcho2: %w", ErrLocal, err)
	}
	switch {
	case errno == windows.ERROR_INSUFFICIENT_BUFFER || ICMPStatus(errno) == StatusBufTooSmall:
		return EchoReply{}, fmt.Errorf("%w: IcmpSendEcho2: reply buffer too small", ErrLocal)
	case errno == windows.ERROR_NETWORK_UNREACHABLE:
		return EchoReply{Status: StatusDestNetUnreachable}, nil
	case errno == windows.ERROR_HOST_UNREACHABLE:
		return EchoReply{Status: StatusDestHostUnreachable}, nil
	case errno > 11000 && errno <= 11050:
		return EchoReply{Status: ICMPStatus(errno)}, nil
	default:
		return EchoReply{}, fmt.Errorf("%w: IcmpSendEcho2: %w", ErrLocal, errno)
	}
}
