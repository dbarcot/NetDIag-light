package sysapi

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"
)

func TestClassifyNeighbor(t *testing.T) {
	t.Parallel()
	tests := map[NeighborState]ARPOutcome{
		NeighborUnreachable: ARPFailed,
		NeighborIncomplete:  ARPFailed,
		NeighborProbe:       ARPUnconfirmed,
		NeighborDelay:       ARPUnconfirmed,
		NeighborStale:       ARPUnconfirmed,
		NeighborReachable:   ARPResolved,
		NeighborPermanent:   ARPResolved,
		NeighborState(42):   ARPFailed,
	}
	for s, want := range tests {
		if got := ClassifyNeighbor(s); got != want {
			t.Errorf("ClassifyNeighbor(%s) = %s, want %s", s, got, want)
		}
	}
}

func TestNeighborResult(t *testing.T) {
	t.Parallel()
	mac := []byte{0x18, 0xfd, 0x74, 0x68, 0x38, 0xd3}
	tests := []struct {
		state   NeighborState
		mac     []byte
		wantErr bool
		outcome ARPOutcome
	}{
		{NeighborReachable, mac, false, ARPResolved},
		{NeighborPermanent, mac, false, ARPResolved},
		{NeighborStale, mac, false, ARPUnconfirmed}, // cached MAC is returned but is not a success
		{NeighborProbe, mac, false, ARPUnconfirmed},
		{NeighborDelay, mac, false, ARPUnconfirmed},
		{NeighborUnreachable, mac, true, ARPFailed},
		{NeighborIncomplete, nil, true, ARPFailed},
		{NeighborReachable, nil, true, ARPResolved}, // no MAC: unresolved whatever the state says
	}
	for _, tt := range tests {
		r, err := neighborResult(tt.state, tt.mac)
		if (err != nil) != tt.wantErr || (err != nil && !errors.Is(err, ErrUnresolved)) {
			t.Errorf("%s/%d bytes: err = %v, wantErr %v", tt.state, len(tt.mac), err, tt.wantErr)
		}
		if r.Outcome() != tt.outcome || r.State != tt.state {
			t.Errorf("%s: outcome %s, want %s", tt.state, r.Outcome(), tt.outcome)
		}
		if err == nil && len(r.MAC) != len(tt.mac) {
			t.Errorf("%s: MAC %v", tt.state, r.MAC)
		}
	}
}

func TestFakeARP_States(t *testing.T) {
	t.Parallel()
	gwIP, other := netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("192.168.1.2")
	f := &FakeARP{Table: map[netip.Addr]ARPResult{
		gwIP: {MAC: []byte{1, 2, 3, 4, 5, 6}, State: NeighborStale},
	}}
	r, err := f.Resolve(context.Background(), gwIP, 1)
	if err != nil || r.Outcome() != ARPUnconfirmed {
		t.Errorf("stale gateway: %+v, %v", r, err)
	}
	if _, err := f.Resolve(context.Background(), other, 1); !errors.Is(err, ErrUnresolved) {
		t.Errorf("unknown address: %v", err)
	}
}

// blockingResolver blocks every call until release is closed and counts calls.
type blockingResolver struct {
	release chan struct{}
	calls   chan netip.Addr
}

func newBlockingResolver() *blockingResolver {
	return &blockingResolver{release: make(chan struct{}), calls: make(chan netip.Addr, 10)}
}

func (b *blockingResolver) resolve(ip netip.Addr, _ uint32) (ARPResult, error) {
	b.calls <- ip
	<-b.release
	return ARPResult{MAC: []byte{1, 2, 3, 4, 5, 6}, State: NeighborReachable}, nil
}

func TestSerialARP_NeverOverlapsSameAddress(t *testing.T) {
	t.Parallel()
	b := newBlockingResolver()
	a := NewSerialARP(b.resolve)
	ip := netip.MustParseAddr("192.168.1.1")

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := a.Resolve(ctx, ip, 4)
		errc <- err
	}()
	<-b.calls // first OS call is running
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("first call: want context.Canceled, got %v", err)
	}

	// The OS call is still blocked: a new request must not start another.
	if _, err := a.Resolve(context.Background(), ip, 4); !errors.Is(err, ErrARPBusy) || err.Error() != "previous ARP still pending" {
		t.Fatalf("second call: want ErrARPBusy, got %v", err)
	}
	select {
	case <-b.calls:
		t.Fatal("a second ResolveIpNetEntry2 was started")
	default:
	}

	// Another interface is another key and may run.
	go func() { _, _ = a.Resolve(context.Background(), ip, 5) }()
	select {
	case <-b.calls:
	case <-time.After(time.Second):
		t.Fatal("different (ip, ifIndex) was blocked")
	}

	close(b.release) // both OS calls return
	deadline := time.After(time.Second)
	for {
		r, err := a.Resolve(context.Background(), ip, 4)
		if err == nil {
			if r.Outcome() != ARPResolved {
				t.Errorf("outcome %s", r.Outcome())
			}
			return
		}
		if !errors.Is(err, ErrARPBusy) {
			t.Fatalf("after release: %v", err)
		}
		select {
		case <-deadline:
			t.Fatal("still busy after the OS call returned")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestSerialARP_RejectsIPv6(t *testing.T) {
	t.Parallel()
	a := NewSerialARP(func(netip.Addr, uint32) (ARPResult, error) { return ARPResult{}, nil })
	if _, err := a.Resolve(context.Background(), netip.MustParseAddr("fe80::1"), 1); err == nil {
		t.Error("want error for IPv6")
	}
}
