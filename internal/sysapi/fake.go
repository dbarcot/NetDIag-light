package sysapi

import (
	"bytes"
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
	Rows       int // Height; 0 = no console height (error)
	Changes    []ModeChange
	Keys       chan Key // ReadKey reads from here
	output     bytes.Buffer
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

func (c *FakeConsole) Height() (int, error) {
	if c.NoOut || c.Rows == 0 {
		return 0, ErrNotConsole
	}
	return c.Rows, nil
}

func (c *FakeConsole) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.NoOut {
		return 0, ErrNotConsole
	}
	return c.output.Write(p)
}

// Output returns everything written with Write.
func (c *FakeConsole) Output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.output.String()
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
	mu    sync.Mutex
	Route Route
	Err   error
}

func (f *FakeRoutes) BestRoute(context.Context, netip.Addr) (Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Route, f.Err
}

// Set replaces the route and error.
func (f *FakeRoutes) Set(r Route, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Route, f.Err = r, err
}

// FakeARP answers from Table with the same rules as Windows (an entry in
// a failed state is ErrUnresolved); unknown IPs are ErrUnresolved.
type FakeARP struct {
	mu    sync.Mutex
	Table map[netip.Addr]ARPResult
}

func (f *FakeARP) Resolve(_ context.Context, ip netip.Addr, _ uint32) (ARPResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.Table[ip]
	if !ok {
		return ARPResult{State: NeighborUnreachable}, ErrUnresolved
	}
	return neighborResult(r.State, r.MAC)
}

// Set changes the entry for ip.
func (f *FakeARP) Set(ip netip.Addr, mac net.HardwareAddr, state NeighborState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Table == nil {
		f.Table = map[netip.Addr]ARPResult{}
	}
	f.Table[ip] = ARPResult{MAC: mac, State: state}
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
