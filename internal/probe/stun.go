package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/pion/stun/v3"
)

// errNoBinding is a STUN response that is not a usable binding success.
var errNoBinding = errors.New("no STUN binding success in the response")

// stunBinding sends one binding request on conn to remote and waits for
// the response with the same transaction ID; other datagrams are ignored.
// It returns our address as the server saw it (XOR-MAPPED-ADDRESS, or the
// legacy MAPPED-ADDRESS) and the round trip time.
func stunBinding(ctx context.Context, conn *net.UDPConn, remote *net.UDPAddr, timeout time.Duration) (netip.AddrPort, time.Duration, error) {
	req, err := stun.Build(stun.TransactionID, stun.BindingRequest)
	if err != nil {
		return netip.AddrPort{}, 0, fmt.Errorf("%w: %w", ErrLocal, err)
	}
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return netip.AddrPort{}, 0, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	elapsed := stopwatch()
	if _, err := conn.WriteToUDP(req.Raw, remote); err != nil {
		return netip.AddrPort{}, 0, err
	}
	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return netip.AddrPort{}, 0, ctx.Err()
			}
			return netip.AddrPort{}, 0, err
		}
		rtt := elapsed()
		if !stun.IsMessage(buf[:n]) {
			continue
		}
		resp := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
		if resp.Decode() != nil || resp.TransactionID != req.TransactionID {
			continue
		}
		if resp.Type != stun.BindingSuccess {
			return netip.AddrPort{}, rtt, errNoBinding
		}
		var xor stun.XORMappedAddress
		if err := xor.GetFrom(resp); err == nil {
			return mappedAddr(xor.IP, xor.Port), rtt, nil
		}
		var plain stun.MappedAddress
		if err := plain.GetFrom(resp); err == nil {
			return mappedAddr(plain.IP, plain.Port), rtt, nil
		}
		return netip.AddrPort{}, rtt, errNoBinding
	}
}

func mappedAddr(ip net.IP, port int) netip.AddrPort {
	a, _ := netip.AddrFromSlice(ip)
	return netip.AddrPortFrom(a.Unmap(), uint16(port))
}

// resolveUDP resolves "host:port" to the first IPv4 address.
func resolveUDP(ctx context.Context, r Resolver, server string) (*net.UDPAddr, error) {
	host, port, err := net.SplitHostPort(server)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLocal, err)
	}
	ips, err := r.LookupIPv4(ctx, host)
	if err == nil && len(ips) == 0 {
		err = noAddress(host)
	}
	if err != nil {
		return nil, err
	}
	p, err := net.LookupPort("udp", port)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLocal, err)
	}
	return &net.UDPAddr{IP: ips[0].AsSlice(), Port: p}, nil
}

func stunFailure(err error, remote *net.UDPAddr) Sample {
	if errors.Is(err, ErrLocal) {
		return Sample{Result: Fail, ErrClass: ClassLocal, Detail: err.Error()}
	}
	class, detail := ClassifyNetError(err)
	if errors.Is(err, errNoBinding) {
		class, detail = ClassOther, err.Error()
	}
	from, _ := netip.AddrFromSlice(remote.IP)
	return Sample{Result: Fail, ErrClass: class, Detail: detail, From: from.Unmap()}
}

// STUNProbe sends one STUN binding request from a fresh UDP socket (RT-01
// and, later, the Teams/Meet checks). It proves UDP reachability and shows
// the public address.
type STUNProbe struct {
	Resolver Resolver
	Server   string // host:port
	Timeout  time.Duration
	// SkipUnresolved makes a server name that does not resolve a skipped
	// sample instead of a failure: RT-01 defines L4 and so cannot wait for
	// DNS (SPEC §6).
	SkipUnresolved bool
}

func (p STUNProbe) Run(ctx context.Context) Sample {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	remote, err := resolveUDP(ctx, p.Resolver, p.Server)
	if err != nil {
		if p.SkipUnresolved && !errors.Is(err, ErrLocal) {
			return SkippedSample("name not resolved")
		}
		if errors.Is(err, ErrLocal) {
			return Sample{Result: Fail, ErrClass: ClassLocal, Detail: err.Error()}
		}
		return resolveFailure(p.Server, err)
	}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return Sample{Result: Fail, ErrClass: ClassLocal, Detail: err.Error()}
	}
	defer func() { _ = conn.Close() }()
	mapped, rtt, err := stunBinding(ctx, conn, remote, p.Timeout)
	if err != nil {
		return stunFailure(err, remote)
	}
	from, _ := netip.AddrFromSlice(remote.IP)
	return Sample{Result: OK, RTT: rtt, From: from.Unmap(), Mapped: mapped, Detail: "public " + mapped.String()}
}

// NATProbe is NAT-01: binding requests from one UDP socket to two STUN
// servers. A different mapped address or port per destination means a
// symmetric NAT, which breaks direct UDP media (Value 1; 0 otherwise).
type NATProbe struct {
	Resolver Resolver
	Servers  []string // host:port; at least two
	Timeout  time.Duration
}

func (p NATProbe) Run(ctx context.Context) Sample {
	if len(p.Servers) < 2 {
		return SkippedSample("needs two STUN servers")
	}
	ctx, cancel := context.WithTimeout(ctx, p.Timeout*time.Duration(2))
	defer cancel()
	var remotes [2]*net.UDPAddr
	for i := range remotes {
		r, err := resolveUDP(ctx, p.Resolver, p.Servers[i])
		if err != nil {
			if errors.Is(err, ErrLocal) {
				return Sample{Result: Fail, ErrClass: ClassLocal, Detail: err.Error()}
			}
			return SkippedSample("name not resolved")
		}
		remotes[i] = r
	}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return Sample{Result: Fail, ErrClass: ClassLocal, Detail: err.Error()}
	}
	defer func() { _ = conn.Close() }()
	var mapped [2]netip.AddrPort
	var first time.Duration
	for i, r := range remotes {
		m, rtt, err := stunBinding(ctx, conn, r, p.Timeout)
		if err != nil {
			return stunFailure(err, r)
		}
		mapped[i] = m
		if i == 0 {
			first = rtt
		}
	}
	s := Sample{Result: OK, RTT: first, Mapped: mapped[0]}
	if mapped[0] != mapped[1] {
		s.Value = 1
		s.Detail = fmt.Sprintf("symmetric NAT: %s via %s, %s via %s", mapped[0], p.Servers[0], mapped[1], p.Servers[1])
	} else {
		s.Detail = "same mapping " + mapped[0].String()
	}
	return s
}
