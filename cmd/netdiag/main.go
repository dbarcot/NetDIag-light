// Command netdiag is a Windows network diagnostic tool (see docs/SPEC.md).
package main

//go:generate windres -i netdiag.rc -O coff -o rsrc_windows_amd64.syso

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"netdiag/internal/clock"
	"netdiag/internal/config"
	"netdiag/internal/sysapi"
)

// Set at build time with -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	var exeDir string
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	// Ctrl+C, and console close/logoff/shutdown (SIGTERM on Windows)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	a := &app{
		ctx:       ctx,
		clock:     clock.Real{},
		stdin:     os.Stdin,
		stdout:    os.Stdout,
		stderr:    os.Stderr,
		console:   sysapi.NewConsole(),
		lookupEnv: os.LookupEnv,
		fs:        config.OSFS{},
		paths:     config.Paths{ExeDir: exeDir, AppData: os.Getenv("APPDATA")},
		version:   version,
		commit:    commit,
		live:      realLive,
		openLog:   openLogFile,
	}
	code := a.main(os.Args[1:])
	stop()
	os.Exit(code)
}
