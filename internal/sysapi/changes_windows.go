//go:build windows

package sysapi

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// PollInterval is the fallback polling period when change notifications
// cannot be registered (matches LINK-01's 5 s rate).
const PollInterval = 5 * time.Second

// WinChanges implements Changes with NotifyIpInterfaceChange,
// NotifyUnicastIpAddressChange and NotifyRouteChange2 (IPv4), falling back
// to polling adapters and the best route when registration fails.
type WinChanges struct {
	Adapters Adapters
	Routes   Routes
	Logger   *slog.Logger // optional; the fallback is logged here
}

// Callbacks made with windows.NewCallback are never freed, so one callback
// is created for the process and broadcasts to every active watcher.
var (
	notifyOnce     sync.Once
	notifyCallback uintptr
	watchersMu     sync.Mutex
	watchers       = map[chan struct{}]bool{}
)

func onNotify(_, _, _ uintptr) uintptr {
	watchersMu.Lock()
	defer watchersMu.Unlock()
	for ch := range watchers {
		signal(ch)
	}
	return 0
}

func (w WinChanges) Watch(ctx context.Context) (<-chan struct{}, error) {
	notifyOnce.Do(func() { notifyCallback = windows.NewCallback(onNotify) })
	ch := make(chan struct{}, 1)
	watchersMu.Lock()
	watchers[ch] = true
	watchersMu.Unlock()

	var handles []windows.Handle
	register := []func(*windows.Handle) error{
		func(h *windows.Handle) error {
			return windows.NotifyIpInterfaceChange(windows.AF_INET, notifyCallback, nil, false, h)
		},
		func(h *windows.Handle) error {
			return windows.NotifyUnicastIpAddressChange(windows.AF_INET, notifyCallback, nil, false, h)
		},
		func(h *windows.Handle) error {
			return windows.NotifyRouteChange2(windows.AF_INET, notifyCallback, nil, false, h)
		},
	}
	for _, reg := range register {
		var h windows.Handle
		if err := reg(&h); err != nil {
			cancelNotify(handles, ch)
			if w.Logger != nil {
				w.Logger.Warn("network change notifications unavailable, polling instead",
					"error", err, "interval", PollInterval)
			}
			return w.poll(ctx), nil
		}
		handles = append(handles, h)
	}
	go func() {
		<-ctx.Done()
		cancelNotify(handles, ch)
		close(ch)
	}()
	return ch, nil
}

// cancelNotify unregisters; CancelMibChangeNotify2 waits for running
// callbacks, so no send can happen on ch afterwards.
func cancelNotify(handles []windows.Handle, ch chan struct{}) {
	for _, h := range handles {
		_ = windows.CancelMibChangeNotify2(h)
	}
	watchersMu.Lock()
	delete(watchers, ch)
	watchersMu.Unlock()
}

func (w WinChanges) poll(ctx context.Context) <-chan struct{} {
	t := time.NewTicker(PollInterval)
	go func() {
		<-ctx.Done()
		t.Stop()
	}()
	return PollChanges(ctx, w.Adapters, w.Routes, t.C)
}

// NewSystem returns the real Windows implementations of every boundary.
// logger may be nil.
func NewSystem(logger *slog.Logger) System {
	ad, rt := WinAdapters{}, WinRoutes{}
	return System{
		Adapters: ad,
		Routes:   rt,
		ARP:      NewARP(),
		WiFi:     NewWiFi(),
		CPU:      WinCPU{},
		Changes:  WinChanges{Adapters: ad, Routes: rt, Logger: logger},
		Console:  NewConsole(),
	}
}
