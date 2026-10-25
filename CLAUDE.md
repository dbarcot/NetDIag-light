# CLAUDE.md — netdiag

Windows-only command-line network diagnostic tool written in Go. Live mtr-style console stats plus a local read-only web UI with latency/jitter graphs.

Read these before writing code:

- `docs/SPEC.md` — what the tool does (source of truth for behavior)
- `docs/PHASES.md` — the order of work, with tests and acceptance criteria per phase
- `docs/TESTING.md` — how everything is tested, including the web UI

## Golden rules

0. At the start of every session read docs/STATUS.md first. At the end of every phase or fix task, update docs/STATUS.md (state, summary, decisions, open items) in the same commit. Keep it under ~150 lines; move detail into SPEC/PHASES, not STATUS. If the user gives no specific task (e.g. just "continue" or "pokračuj"), do the next item from docs/STATUS.md: open review fixes first, then the next phase. Stop after that one item, update STATUS.md and commit.
1. **Work strictly phase by phase** as listed in `docs/PHASES.md`. Do not start phase N+1 until every test of phase N passes and its acceptance checklist is ticked. At the end of each phase, stop and give a short summary: what was built, test results, any deviations from the spec.
2. **Tests are written together with the code, never after the phase.** Prefer writing the test first for pure logic (stats, gates, verdicts, config). Every exported behavior has a test. A phase with failing, skipped or missing tests is not done.
3. **Run the full check after every meaningful change**, not only at the end:
   ```
   go vet ./...
   go test ./...
   ```
   Also run `golangci-lint run` if it is installed. Fix failures before continuing.
4. **Unit tests never touch the real network or real OS network state.** All OS and network access goes through interfaces with fakes (see Architecture). Tests that need the live network or real adapters use the build tag `integration` and are run explicitly.
5. **Web UI is tested** at two levels: HTTP handler tests (`httptest`) and browser end-to-end tests (chromedp driving installed Edge or Chrome) against simulation mode. See `docs/TESTING.md`.
6. **Do not invent endpoints, IP ranges or hostnames.** Use only what `docs/SPEC.md` lists. If a default is marked "to confirm", leave it empty/disabled and say so in the phase summary.
7. **Ask before adding any dependency** not on the allowlist below.
8. **If you must deviate from the spec**, update `docs/SPEC.md` in the same change and mention it in the phase summary.

## Constraints

- Windows 10/11 and Server 2016+ only. The tool is Windows-only: all code and tests target Windows, and the module does not need to build on other OSes, but pure logic stays free of OS calls so it is testable with fakes.
- No admin rights. The exe manifest is `asInvoker`. Never use raw sockets.
- No cgo. Windows APIs via `golang.org/x/sys/windows` or `windows.NewLazySystemDLL`.
- No proxy handling. Direct connections only.
- No VPN detection or VPN-specific logic. Tests simply run over whatever adapter Windows routes through.
- Web server binds `127.0.0.1` only. Never `0.0.0.0`.
- No external network requests from the web page (no CDN). All assets are embedded.
- Probe rate: never more than 1 probe/s per target by default; config may not go below 1 s. Only exception: TRC-01 hop cycling (SPEC §5), capped at 5 packets/s in total.

## Dependency allowlist

- `golang.org/x/sys` — Windows APIs
- `github.com/miekg/dns` — queries to specific DNS servers
- `github.com/pion/stun/v3` — STUN client (not the old unversioned path)
- `github.com/chromedp/chromedp` — browser tests only
- uPlot (JavaScript, vendored into `internal/web/assets/`) — charts
- Standard library for everything else (CLI parsing with `flag`, logging with `log/slog`, HTTP, JSON, embed)

## Architecture (package layout)

```
cmd/netdiag/          main, subcommands, help, Explorer double-click pause
internal/config/      embedded defaults, load + merge + validate, save-config
internal/sysapi/      interfaces for OS info + Windows implementations + fakes
                      (adapters, gateway/best route, ARP, DNS servers, Wi-Fi, console)
internal/probe/       probe interface + implementations (icmp, tcp, dns, stun, http,
                      tls, mtu, trace) + fakes
internal/clock/       Clock interface (Now, timers, tickers), real clock, manual fake
internal/sched/       scheduler: rates, start jitter, gate-aware pausing
internal/store/       ring buffers per series, rolling-window stats
internal/stats/       pure math: loss, jitter (mean |ΔRTT|), stdev, E-model MOS
internal/gate/        dependency layers, hysteresis, skipped state
internal/verdict/     verdicts and info flags from gates + stats
internal/engine/      series catalog; ties store, gates and verdict together; snapshots
internal/console/     mtr-style TTY renderer, plain renderer, keys (raw stdin, mode restore)
internal/web/         HTTP server, Host check, SSE, JSON API, embedded assets
internal/report/      report JSON model (summary + whole-run aggregates) + export
internal/csvlog/      CSV writers: samples, summary, events; rotation, size cap, flush
internal/sim/         simulation mode: scripted scenarios feeding fake probes/sysapi
testdata/scenarios/   scenario files used by unit, UI and demo runs
```

Design rules:

- **Inject everything that varies:** clock (`Now`, timers), sysapi, probes. No package-level mutable globals.
- **Context cancellation everywhere.** Ctrl+C / `q` cancels one root context; all goroutines exit; tests check for goroutine leaks where practical.
- **Samples are immutable values** with timestamp, target, result (ok / fail / skipped), RTT and error class.
- **Skipped is a distinct state**, never counted as loss.
- **Logging:** `slog` to a file only when `--log <path>` is given. The console is owned by the renderer.

## Commands

Always with `CGO_ENABLED=0` (PowerShell: `$env:CGO_ENABLED='0'`); the machine default is 1. Only the race detector needs cgo: `$env:CGO_ENABLED='1'; go test -race ./...` (run it after changes to concurrent code).

```
go build -o netdiag.exe ./cmd/netdiag
go test ./...                                   # unit + handler tests, no network
go test -tags integration ./...                 # live Windows + network tests
go test -tags e2e ./internal/web/...            # browser UI tests (needs Edge or Chrome)
```

## Workflow

- Commit to `main` once at the end of each phase (tests green, summary given). Never push.
- Communicate with the user in Czech; code, comments and docs in English.

## Definition of done (every phase)

- [ ] Code for the phase matches `docs/SPEC.md`
- [ ] Unit tests written and passing; `go vet` clean
- [ ] Integration / e2e tests for the phase written and passing (where the phase requires them)
- [ ] No new dependencies outside the allowlist
- [ ] Phase summary given; spec updated if anything deviated
