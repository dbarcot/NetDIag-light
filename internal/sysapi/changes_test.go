package sysapi

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"
)

func adapter(ip string, up bool) Adapter {
	return Adapter{Index: 7, Up: up, IPv4: []netip.Prefix{netip.MustParsePrefix(ip)}}
}

func expectSignal(t *testing.T, ch <-chan struct{}, want bool) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		if !want {
			t.Fatal("unexpected change signal")
		}
	case <-time.After(50 * time.Millisecond):
		if want {
			t.Fatal("no change signal")
		}
	}
}

func TestPollChanges(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	fa := &FakeAdapters{List: []Adapter{adapter("192.168.1.23/24", true)}}
	fr := &FakeRoutes{Route: Route{InterfaceIndex: 7, Gateway: netip.MustParseAddr("192.168.1.1")}}
	tick := make(chan time.Time)
	ch := PollChanges(ctx, fa, fr, tick)

	tick <- time.Time{}
	expectSignal(t, ch, false) // same state as the baseline

	fa.Set([]Adapter{adapter("192.168.1.50/24", true)}) // new DHCP address
	tick <- time.Time{}
	expectSignal(t, ch, true)

	fa.Set([]Adapter{adapter("192.168.1.50/24", false)}) // cable unplugged
	tick <- time.Time{}
	expectSignal(t, ch, true)

	fa.SetErr(errors.New("transient"))
	tick <- time.Time{}
	expectSignal(t, ch, false)

	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("want closed channel after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed after cancel")
	}
}

func TestPollChanges_CoalescesSignals(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fa := &FakeAdapters{List: []Adapter{adapter("10.0.0.2/8", true)}}
	tick := make(chan time.Time)
	ch := PollChanges(ctx, fa, &FakeRoutes{Err: ErrNoRoute}, tick)
	for _, ip := range []string{"10.0.0.3/8", "10.0.0.4/8"} {
		fa.Set([]Adapter{adapter(ip, true)})
		tick <- time.Time{} // nobody reads ch in between: must not block
	}
	tick <- time.Time{} // unchanged; returns only after the previous tick was handled
	expectSignal(t, ch, true)
	expectSignal(t, ch, false)
}

// Route changes are detected even when every adapter looks the same, e.g.
// Windows moving the default route between two up adapters.
func TestPollChanges_DetectsRouteChanges(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fa := &FakeAdapters{List: []Adapter{adapter("192.168.1.23/24", true)}}
	gw1 := netip.MustParseAddr("192.168.1.1")
	fr := &FakeRoutes{Route: Route{InterfaceIndex: 7, Gateway: gw1}}
	tick := make(chan time.Time)
	ch := PollChanges(ctx, fa, fr, tick)

	steps := []struct {
		name  string
		route Route
		err   error
		want  bool
	}{
		{"unchanged", Route{InterfaceIndex: 7, Gateway: gw1}, nil, false},
		{"default route lost", Route{}, ErrNoRoute, true},
		{"still no route", Route{}, ErrNoRoute, false},
		{"route back", Route{InterfaceIndex: 7, Gateway: gw1}, nil, true},
		{"route moved to another interface", Route{InterfaceIndex: 12, Gateway: gw1}, nil, true},
		{"gateway changed", Route{InterfaceIndex: 12, Gateway: netip.MustParseAddr("10.0.0.1")}, nil, true},
		{"transient route error counts as unchanged", Route{}, errors.New("boom"), false},
	}
	for _, s := range steps {
		fr.Set(s.route, s.err)
		tick <- time.Time{}
		t.Run(s.name, func(t *testing.T) { expectSignal(t, ch, s.want) })
	}
}

func TestFindAdapter(t *testing.T) {
	t.Parallel()
	list := []Adapter{{Index: 3, Name: "Ethernet"}, {Index: 12, Name: "Wi-Fi"}}
	if a, ok := FindAdapter(list, 12); !ok || a.Name != "Wi-Fi" {
		t.Errorf("got %+v, %v", a, ok)
	}
	if _, ok := FindAdapter(list, 99); ok {
		t.Error("found missing adapter")
	}
}
