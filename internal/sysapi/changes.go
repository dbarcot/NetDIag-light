package sysapi

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// adapterFingerprint summarizes what matters for gate re-evaluation: a
// change in it means the network changed (SPEC §6 adapter change).
func adapterFingerprint(adapters []Adapter) string {
	var b strings.Builder
	for _, a := range adapters {
		fmt.Fprintf(&b, "%d|%t|%v|%v|%v;", a.Index, a.Up, a.IPv4, a.Gateways, a.DNSServers)
	}
	return b.String()
}

// PollChanges is the polling fallback for Changes: on every tick it lists
// the adapters and signals when their fingerprint differs from the last
// one. Errors while listing count as "unchanged". The channel closes when
// ctx ends.
func PollChanges(ctx context.Context, a Adapters, tick <-chan time.Time) <-chan struct{} {
	ch := make(chan struct{}, 1)
	go func() {
		defer close(ch)
		var last string
		if list, err := a.Adapters(ctx); err == nil {
			last = adapterFingerprint(list)
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick:
			}
			list, err := a.Adapters(ctx)
			if err != nil {
				continue
			}
			if fp := adapterFingerprint(list); fp != last {
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
