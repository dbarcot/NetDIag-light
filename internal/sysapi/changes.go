package sysapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// networkFingerprint summarizes what matters for gate re-evaluation: the
// adapters' state and addresses plus the best route toward
// RouteProbeTarget. A change in it means the network changed (SPEC §6
// adapter change). ok is false when a lookup failed transiently.
func networkFingerprint(ctx context.Context, a Adapters, r Routes) (fp string, ok bool) {
	list, err := a.Adapters(ctx)
	if err != nil {
		return "", false
	}
	var b strings.Builder
	for _, ad := range list {
		fmt.Fprintf(&b, "%d|%t|%v|%v|%v;", ad.Index, ad.Up, ad.IPv4, ad.Gateways, ad.DNSServers)
	}
	rt, err := r.BestRoute(ctx, RouteProbeTarget)
	switch {
	case errors.Is(err, ErrNoRoute):
		b.WriteString("route:none")
	case err != nil:
		return "", false
	default:
		fmt.Fprintf(&b, "route:%d|%v", rt.InterfaceIndex, rt.Gateway)
	}
	return b.String(), true
}

// PollChanges is the polling fallback for Changes: on every tick it reads
// the adapters and the best route and signals when the fingerprint differs
// from the last one. Failed lookups count as "unchanged". The channel
// closes when ctx ends.
func PollChanges(ctx context.Context, a Adapters, r Routes, tick <-chan time.Time) <-chan struct{} {
	ch := make(chan struct{}, 1)
	go func() {
		defer close(ch)
		last, _ := networkFingerprint(ctx, a, r)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick:
			}
			fp, ok := networkFingerprint(ctx, a, r)
			if ok && fp != last {
				last = fp
				signal(ch)
			}
		}
	}()
	return ch
}

// signal sends without blocking; a pending signal already covers this one.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
