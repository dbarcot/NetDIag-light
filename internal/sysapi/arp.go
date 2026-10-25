package sysapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
)

// ErrARPBusy is returned when the previous resolution of the same address
// is still running; LAN-02 counts it as a failure with this detail.
var ErrARPBusy = errors.New("previous ARP still pending")

// NeighborState is NL_NEIGHBOR_STATE (nldef.h).
type NeighborState uint32

const (
	NeighborUnreachable NeighborState = 0
	NeighborIncomplete  NeighborState = 1
	NeighborProbe       NeighborState = 2
	NeighborDelay       NeighborState = 3
	NeighborStale       NeighborState = 4
	NeighborReachable   NeighborState = 5
	NeighborPermanent   NeighborState = 6
)

func (s NeighborState) String() string {
	switch s {
	case NeighborUnreachable:
		return "unreachable"
	case NeighborIncomplete:
		return "incomplete"
	case NeighborProbe:
		return "probe"
	case NeighborDelay:
		return "delay"
	case NeighborStale:
		return "stale"
	case NeighborReachable:
		return "reachable"
	case NeighborPermanent:
		return "permanent"
	default:
		return fmt.Sprintf("state(%d)", uint32(s))
	}
}

// ARPOutcome is how LAN-02 and the L3 gate read a neighbor state.
type ARPOutcome string

const (
	ARPResolved    ARPOutcome = "resolved"    // reachable or permanent: the gateway answered
	ARPUnconfirmed ARPOutcome = "unconfirmed" // probe/delay/stale: a cached MAC, not proof the gateway is alive
	ARPFailed      ARPOutcome = "failed"
)

// ClassifyNeighbor maps a neighbor state to an outcome. Only reachable and
// permanent count as resolved, so a stale cache entry never keeps L3 open.
func ClassifyNeighbor(s NeighborState) ARPOutcome {
	switch s {
	case NeighborReachable, NeighborPermanent:
		return ARPResolved
	case NeighborProbe, NeighborDelay, NeighborStale:
		return ARPUnconfirmed
	default:
		return ARPFailed
	}
}

// ARPResult is a neighbor entry after a resolution attempt.
type ARPResult struct {
	MAC   net.HardwareAddr
	State NeighborState
}

// Outcome classifies the result's state.
func (r ARPResult) Outcome() ARPOutcome { return ClassifyNeighbor(r.State) }

// neighborResult turns a neighbor row into a result: unreachable,
// incomplete or no MAC is ErrUnresolved; other states return the MAC with
// the state for the caller to classify.
func neighborResult(state NeighborState, mac []byte) (ARPResult, error) {
	r := ARPResult{State: state}
	if len(mac) == 0 || ClassifyNeighbor(state) == ARPFailed {
		return r, ErrUnresolved
	}
	r.MAC = append(net.HardwareAddr(nil), mac...)
	return r, nil
}

type arpKey struct {
	ip      netip.Addr
	ifIndex uint32
}

// SerialARP implements ARP on top of a blocking resolve function and keeps
// at most one call in flight per (ip, ifIndex). The underlying call cannot
// be cancelled; when ctx ends first it keeps running in the background and
// further requests for the same key get ErrARPBusy until it returns.
type SerialARP struct {
	resolve  func(ip netip.Addr, ifIndex uint32) (ARPResult, error)
	mu       sync.Mutex
	inflight map[arpKey]bool
}

// NewSerialARP wraps a blocking resolve function.
func NewSerialARP(resolve func(netip.Addr, uint32) (ARPResult, error)) *SerialARP {
	return &SerialARP{resolve: resolve, inflight: map[arpKey]bool{}}
}

func (s *SerialARP) Resolve(ctx context.Context, ip netip.Addr, ifIndex uint32) (ARPResult, error) {
	if !ip.Is4() {
		return ARPResult{}, fmt.Errorf("ARP: %s is not IPv4", ip)
	}
	key := arpKey{ip, ifIndex}
	s.mu.Lock()
	if s.inflight[key] {
		s.mu.Unlock()
		return ARPResult{}, ErrARPBusy
	}
	s.inflight[key] = true
	s.mu.Unlock()

	type result struct {
		r   ARPResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := s.resolve(ip, ifIndex)
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
		done <- result{r, err}
	}()
	select {
	case res := <-done:
		return res.r, res.err
	case <-ctx.Done():
		return ARPResult{}, ctx.Err()
	}
}
