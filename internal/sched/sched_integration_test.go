//go:build integration && windows

package sched

import (
	"context"
	"testing"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/probe"
	"netdiag/internal/store"
	"netdiag/internal/sysapi"
)

// Scheduler + real ICMP + store on the real clock: LAN-01 to the gateway
// at 1 probe/s for about 3.5 s.
func TestPipeline_GatewayPingIntoStore(t *testing.T) {
	rt, err := sysapi.WinRoutes{}.BestRoute(context.Background(), sysapi.RouteProbeTarget)
	if err != nil || !rt.Gateway.IsValid() {
		t.Skip("no default gateway")
	}
	c := clock.Real{}
	st := store.New(c, time.Minute, time.Minute)
	id := "LAN-01:gateway"
	st.Register(id, time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 3500*time.Millisecond)
	defer cancel()
	s := New(c)
	s.Run(ctx, []Series{{
		ID: id, Probe: "LAN-01", Target: rt.Gateway.String(), Interval: time.Second,
		P: probe.ICMPProbe{ICMP: probe.WinICMP{}, Dest: rt.Gateway, Timeout: time.Second},
	}}, st.Add)

	sum := st.Stats(id)
	t.Logf("LAN-01 %s: sent %d, loss %.1f %%, last %.1f, avg %.1f, best %.1f, worst %.1f, stdev %.2f, jitter %.2f ms",
		rt.Gateway, sum.Sent, sum.LossPct, sum.Last, sum.Avg, sum.Best, sum.Worst, sum.StdDev, sum.Jitter)
	if sum.Sent < 3 || sum.Sent > 4 {
		t.Errorf("sent %d in 3.5 s at 1/s", sum.Sent)
	}
	if sum.OK == 0 || !sum.HasRTT || sum.Best <= 0 {
		t.Errorf("no gateway replies: %+v", sum)
	}
}
