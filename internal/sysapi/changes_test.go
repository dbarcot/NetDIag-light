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
	tick := make(chan time.Time)
	ch := PollChanges(ctx, fa, tick)

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
	ch := PollChanges(ctx, fa, tick)
	for _, ip := range []string{"10.0.0.3/8", "10.0.0.4/8"} {
		fa.Set([]Adapter{adapter(ip, true)})
		tick <- time.Time{} // nobody reads ch in between: must not block
	}
	tick <- time.Time{} // unchanged; returns only after the previous tick was handled
	expectSignal(t, ch, true)
	expectSignal(t, ch, false)
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
