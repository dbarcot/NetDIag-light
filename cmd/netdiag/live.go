package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"netdiag/internal/clock"
	"netdiag/internal/config"
	"netdiag/internal/engine"
	"netdiag/internal/probe"
	"netdiag/internal/sysapi"
)

// realLive runs the real probes against the real network on eng until ctx
// ends. With logPath set, warnings and probe panics go to that file
// (slog); the console belongs to the renderer.
func realLive(ctx context.Context, cfg *config.Config, eng *engine.Engine, logPath string) error {
	var logger *slog.Logger
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("--log: %w", err)
		}
		defer func() { _ = f.Close() }()
		logger = slog.New(slog.NewTextHandler(f, nil))
	}
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
