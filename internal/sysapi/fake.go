package sysapi

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
)

// ModeChange is one SetInMode/SetOutMode call recorded by FakeConsole.
type ModeChange struct {
	Stream string // "in" or "out"
	Mode   uint32
}

// FakeConsole is a Console for tests. It records every mode change.
type FakeConsole struct {
	mu         sync.Mutex
	Count      int   // ProcessCount result
	Err        error // ProcessCount error
	In, Out    uint32
	NoIn       bool // stdin redirected
	NoOut      bool // stdout redirected
	FailSetIn  bool
	FailSetOut bool
	Cols       int
	Changes    []ModeChange
	Keys       chan Key // ReadKey reads from here
}

func (c *FakeConsole) ProcessCount() (int, error) { return c.Count, c.Err }

func (c *FakeConsole) InMode() (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.NoIn {
		return 0, ErrNotConsole
	}
	return c.In, nil
}

func (c *FakeConsole) OutMode() (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.NoOut {
		return 0, ErrNotConsole
	}
	return c.Out, nil
}

func (c *FakeConsole) SetInMode(m uint32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.NoIn || c.FailSetIn {
		return errors.New("fake: SetInMode failed")
	}
	c.In = m
	c.Changes = append(c.Changes, ModeChange{"in", m})
	return nil
}

func (c *FakeConsole) SetOutMode(m uint32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.NoOut || c.FailSetOut {
		return errors.New("fake: SetOutMode failed")
	}
	c.Out = m
	c.Changes = append(c.Changes, ModeChange{"out", m})
	return nil
}

// ModeChanges returns a copy of the recorded mode changes.
func (c *FakeConsole) ModeChanges() []ModeChange {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ModeChange(nil), c.Changes...)
}

func (c *FakeConsole) Width() (int, error) {
	if c.NoOut {
		return 0, ErrNotConsole
	}
	return c.Cols, nil
}

func (c *FakeConsole) ReadKey(ctx context.Context) (Key, error) {
	select {
	case k, ok := <-c.Keys:
		if !ok {
			return Key{}, errors.New("fake: key stream closed")
		}
		return k, nil
	case <-ctx.Done():
		return Key{}, ctx.Err()
	}
}

// FakeAdapters returns a fixed adapter list (or Err); List may be replaced
// between calls under the lock via Set.
type FakeAdapters struct {
	mu   sync.Mutex
	List []Adapter
	Err  error
}

func (f *FakeAdapters) Adapters(ctx context.Context) ([]Adapter, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	return append([]Adapter(nil), f.List...), nil
}

// SetErr makes Adapters fail (nil to recover).
func (f *FakeAdapters) SetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Err = err
}

// Set replaces the adapter list.
func (f *FakeAdapters) Set(list []Adapter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.List = list
}

// FakeRoutes returns Route for every destination, or Err.
type FakeRoutes struct {
	Route Route
	Err   error
}

func (f *FakeRoutes) BestRoute(context.Context, netip.Addr) (Route, error) {
	return f.Route, f.Err
}

// FakeARP resolves the IPs in Table; anything else is ErrUnresolved.
type FakeARP struct {
	Table map[netip.Addr]net.HardwareAddr
}

func (f *FakeARP) Resolve(_ context.Context, ip netip.Addr, _ uint32) (net.HardwareAddr, error) {
	if mac, ok := f.Table[ip]; ok {
		return mac, nil
	}
	return nil, ErrUnresolved
}

// FakeWiFi returns a fixed status list.
type FakeWiFi struct {
	List []WiFiStatus
	Err  error
}

func (f *FakeWiFi) Status(context.Context) ([]WiFiStatus, error) { return f.List, f.Err }

// FakeCPU returns the readings in order, repeating the last one.
type FakeCPU struct {
	mu       sync.Mutex
	Readings []CPUTimes
	Err      error
}

func (f *FakeCPU) Times() (CPUTimes, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return CPUTimes{}, f.Err
	}
	if len(f.Readings) == 0 {
		return CPUTimes{}, nil
	}
	t := f.Readings[0]
	if len(f.Readings) > 1 {
		f.Readings = f.Readings[1:]
	}
	return t, nil
}

// FakeChanges hands out C; tests signal on it directly.
type FakeChanges struct {
	C chan struct{}
}

func (f *FakeChanges) Watch(ctx context.Context) (<-chan struct{}, error) {
	return f.C, nil
}
