package main

import (
	"context"
	"log/slog"
	"os"

	"netdiag/internal/clock"
	"netdiag/internal/config"
	"netdiag/internal/engine"
	"netdiag/internal/probe"
	"netdiag/internal/sysapi"
)

// openLogFile opens the --log file for slog (append). An empty path
// means no log: a nil logger.
func openLogFile(path string) (*slog.Logger, func(), error) {
	if path == "" {
		return nil, func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return slog.New(slog.NewTextHandler(f, nil)), func() { _ = f.Close() }, nil
}

// realLive runs the real probes against the real network on eng until ctx
// ends. With a logger (--log), warnings and probe panics go to that file;
// the console belongs to the renderer.
func realLive(ctx context.Context, cfg *config.Config, eng *engine.Engine, logger *slog.Logger) error {
	sys := sysapi.NewSystem(logger)
	c := clock.Real{}
	_, err := engine.RunLive(ctx, engine.LiveOptions{
		Cfg: cfg, Clock: c, Sys: sys, Logger: logger, Engine: eng,
		Deps: engine.Deps{
			Clock:     c,
			ICMP:      probe.WinICMP{Logger: logger},
			Dialer:    engine.DefaultDialer(),
			Resolver:  probe.SystemResolver{},
			DNSSystem: &probe.WinDNS{},
			DNSPublic: probe.MiekgDNS{},
			ARP:       sys.ARP,
			CPU:       sysapi.NewCPUMeter(sys.CPU),
			Logger:    logger,
		},
	})
	return err
}
