package probe

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"netdiag/internal/sysapi"
)

func TestARPProbe(t *testing.T) {
	t.Parallel()
	gw := netip.MustParseAddr("192.168.1.1")
	mac := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	arp := &sysapi.FakeARP{}
	p := ARPProbe{ARP: arp, Gateway: gw, IfIndex: 7, Timeout: time.Second}
	run := func() Sample { return p.Run(context.Background()) }

	arp.Set(gw, mac, sysapi.NeighborReachable)
	if s := run(); s.Result != OK || s.Detail != "aa:bb:cc:dd:ee:ff (reachable)" {
		t.Errorf("reachable: %+v", s)
	}
	arp.Set(gw, mac, sysapi.NeighborPermanent)
	if s := run(); s.Result != OK {
		t.Errorf("permanent: %+v", s)
	}
	for _, st := range []sysapi.NeighborState{sysapi.NeighborStale, sysapi.NeighborProbe, sysapi.NeighborDelay} {
		arp.Set(gw, mac, st)
		if s := run(); s.Result != Fail || s.ErrClass != ClassUnconfirmed {
			t.Errorf("%s: %+v, want fail/unconfirmed", st, s)
		}
	}
	arp.Set(gw, mac, sysapi.NeighborUnreachable)
	if s := run(); s.Result != Fail || s.ErrClass != ClassUnreachable {
		t.Errorf("unreachable: %+v", s)
	}
	p.Gateway = netip.MustParseAddr("192.168.1.99") // not in the table
	if s := run(); s.Result != Fail || s.ErrClass != ClassUnreachable {
		t.Errorf("unknown neighbor: %+v", s)
	}
}

type busyARP struct{}

func (busyARP) Resolve(context.Context, netip.Addr, uint32) (sysapi.ARPResult, error) {
	return sysapi.ARPResult{}, sysapi.ErrARPBusy
}

type hangARP struct{}

func (hangARP) Resolve(ctx context.Context, _ netip.Addr, _ uint32) (sysapi.ARPResult, error) {
	<-ctx.Done()
	return sysapi.ARPResult{}, ctx.Err()
}

func TestARPProbe_BusyAndTimeout(t *testing.T) {
	t.Parallel()
	gw := netip.MustParseAddr("192.168.1.1")
	s := ARPProbe{ARP: busyARP{}, Gateway: gw, Timeout: time.Second}.Run(context.Background())
	if s.Result != Fail || s.Detail != "previous ARP still pending" {
		t.Errorf("busy: %+v", s)
	}
	s = ARPProbe{ARP: hangARP{}, Gateway: gw, Timeout: 30 * time.Millisecond}.Run(context.Background())
	if s.Result != Fail || s.ErrClass != ClassTimeout {
		t.Errorf("timeout: %+v", s)
	}
}

func TestCPUProbe(t *testing.T) {
	t.Parallel()
	cpu := &sysapi.FakeCPU{Readings: []sysapi.CPUTimes{
		{Idle: 100, Kernel: 150, User: 50},
		{Idle: 150, Kernel: 250, User: 150}, // total +200, idle +50 → 75 %
	}}
	p := &CPUProbe{Meter: sysapi.NewCPUMeter(cpu)}
	if s := p.Run(context.Background()); s.Result != Skipped {
		t.Errorf("first run has no delta: %+v", s)
	}
	if s := p.Run(context.Background()); s.Result != OK || s.Value != 75 {
		t.Errorf("second run: %+v, want 75 %%", s)
	}
	cpu.Err = context.DeadlineExceeded
	if s := p.Run(context.Background()); s.Result != Fail || s.ErrClass != ClassLocal {
		t.Errorf("read error: %+v", s)
	}
}
