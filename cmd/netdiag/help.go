package main

import (
	"fmt"
	"io"
)

const usageText = `Usage:
  netdiag run [--config <path>] [--port <n>] [--log <path>] [--csv <dir>]
  netdiag run --headless --duration <d> --json <path> [--csv <dir>]
              [--config <path>]
  netdiag save-config [<path>]
  netdiag help | -h | -help | --help
  netdiag version
`

const helpBody = `
Commands:
  run          Start probing with the live console view and the local
               web UI at http://127.0.0.1:<port>/ (keys: q quit, r reset).
  save-config  Write the full default config to <path>. Default:
               netdiag.json next to the exe, or
               %APPDATA%\netdiag\netdiag.json if that folder is not
               writable. Never overwrites an existing file.
  help         Show this help.
  version      Show version information.

Options for run:
  --config <path>  Config file. Default lookup: netdiag.json next to the
                   exe, then %APPDATA%\netdiag\netdiag.json, then
                   built-in defaults.
  --port <n>       Web UI port (overrides ui.port, default 48765).
  --log <path>     Write a debug log to <path>.
  --csv <dir>      Log every measurement to CSV files in <dir>.
  --headless       No console view and no web UI. Needs --duration and
                   --json; exits with the worst verdict's exit code.
  --duration <d>   Run time for --headless, e.g. 90s, 5m, 1h (min 10s).
  --json <path>    Write the report JSON to <path>.

Exit codes:
  0 OK, 1 runtime or usage error, 2 config error,
  10-25 network problem found (see the verdict in the report).
`

const saveConfigUsage = `Usage:
  netdiag save-config [<path>]

Write the full default config (every key) to <path>. Without <path>:
netdiag.json next to the exe, or %APPDATA%\netdiag\netdiag.json if that
folder is not writable. Never overwrites an existing file. Prints the
path written.
`

func (a *app) printUsage(w io.Writer) {
	fmt.Fprint(w, usageText)
}

func (a *app) printHelp(w io.Writer) {
	fmt.Fprintf(w, "netdiag %s - network diagnostics for Windows\n\n", a.version)
	fmt.Fprint(w, usageText)
	fmt.Fprint(w, helpBody)
}
