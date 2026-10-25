# netdiag — Implementation phases

Rules (from `CLAUDE.md`): finish phases in order; tests are written during the phase, not after; run `go vet ./...` and `go test ./...` after every meaningful change; a phase is done only when every checkbox below is ticked. Stop after each phase and give a summary.

---

## Phase 0 — Skeleton, CLI, config

**Build**
- Go module `netdiag`, package layout from `CLAUDE.md`.
- `cmd/netdiag`: subcommands `run`, `save-config`, `help`, `version`; no args → help, exit 0.
- Explorer double-click detection (`GetConsoleProcessList`) → "Press Enter to exit" after help or fatal error. Put the check behind a `sysapi.Console` interface.
- `internal/config`: embedded defaults (JSON), load order, deep merge, validation with key-path errors, `save-config` (atomic write, no overwrite, `%APPDATA%` fallback).
- Exe manifest `asInvoker` (plus `supportedOS` for Windows 10): `.rc` + `.manifest` compiled with mingw `windres` into a committed `.syso` (see `docs/TESTING.md` §6a).

**Tests**
- CLI: no args → help text + exit 0; unknown command → error + exit 1; `version` output.
- Double-click: fake console with process count 1 → waits for input; count 2 → does not.
- Config: defaults valid; partial override merges; every validation rule has a failing case with the exact key path in the error (`targets.ping[1].rate`); unknown key rejected; duplicate custom names rejected; rate < 1 rejected; custom target cap enforced; `depends_on` inference (private IP → lan, public → internet, hostname → internet).
- `save-config`: writes valid JSON that round-trips to the defaults; refuses to overwrite; falls back when the primary dir is not writable (inject the filesystem); temp file removed on failure.

**Acceptance**
- [x] `netdiag.exe` with no args prints help
- [x] `netdiag save-config` produces a file that `netdiag run --config` accepts
- [x] All tests pass, `go vet` clean

---

## Phase 1 — System information (sysapi)

**Build**
- Interfaces + Windows implementations + fakes for: adapters (`GetAdaptersAddresses`: name, type, OperStatus, IPv4, DNS servers), best route (`GetBestRoute2` → adapter + gateway), ARP (`ResolveIpNetEntry2`), Wi-Fi (`WlanOpenHandle`/`WlanQueryInterface`: SSID, signal %, RSSI, channel, band; `ERROR_ACCESS_DENIED` from location privacy → partial result with `SSIDHidden=true`, not an error), address-change notification (`NotifyIpInterfaceChange` or polling fallback).
- CPU usage via `GetSystemTimes` (delta of idle vs kernel+user between calls) behind `sysapi.CPU`, with fake.
- `sysapi.Console`: process count (double-click), get/set stdout and stdin modes, enable VT output, raw key input via `ReadConsoleInputW`, restore of saved modes. Fake records every mode change.
- APIPA detection, private-range classification helpers (pure functions).

**Tests**
- Unit: APIPA and private-range helpers (table tests); band from channel/frequency; CPU % from fake `GetSystemTimes` deltas (incl. counter wraparound, zero delta) and level mapping at boundaries; Wi-Fi fake returning access denied for SSID → partial result, signal still reported.
- Integration (`-tags integration`, real Windows): best route returns an adapter and gateway; adapters list non-empty; Wi-Fi query either returns data or a clean "no Wi-Fi" result; no panics without admin.

**Acceptance**
- [ ] A debug subcommand-free test program or integration test prints real sysinfo on a dev machine
- [ ] Unit + integration tests pass

---

## Phase 2 — Probe core: ICMP, scheduler, store, stats

**Build**
- `probe.Probe` interface (`Run(ctx) Sample`), `Sample` type (ok / fail / skipped, RTT, error class, timestamp).
- ICMP prober via `IcmpSendEcho2` (+ TTL and DF options needed later); RTT from monotonic clock; reply buffer sized per SPEC §5 note; startup loopback baseline ("timer overhead").
- `internal/stats`: loss, avg/best/worst, stdev, RFC 3550 jitter, E-model MOS (formula in SPEC §7).
- `internal/store`: per-series ring buffer (history) + rolling window stats; reset.
- `internal/sched`: per-target interval, random start offset, max one in flight per target, pause/resume hook for gates, injected clock.

**Tests**
- Stats: table tests with hand-computed expected values for loss, stdev, jitter sequence, MOS at known points (e.g. 20 ms/0 jitter/0 loss → MOS ≈ 4.4; clamp at bounds); skipped samples excluded from loss.
- Store: ring wraparound, window expiry with fake clock, reset.
- Scheduler (fake clock): correct number of runs over N seconds; no overlap when a probe is slow; pause produces skipped samples with reason; cancel stops all goroutines (leak check).
- Integration: ICMP to `127.0.0.1` and to gateway succeed; RTT > 0; unreachable TEST-NET address (`192.0.2.1`) reports fail within timeout.

**Acceptance**
- [ ] Stats math verified by tests, not by eye
- [ ] Scheduler deterministic under fake clock

---

## Phase 3 — Gates and verdicts, simulation mode

**Build**
- `internal/gate`: layers L1–L5 per SPEC §6, hysteresis, gateway rule (ping OR ARP), internet rule (any of NET-01/NET-04/RT-01), DNS placement (on-link vs remote resolver), skipped reasons, adapter-change reset.
- `internal/verdict`: primary verdict by priority, info flags, exit codes per SPEC §8.
- `internal/sim`: scenario file format (see `docs/TESTING.md`), fake sysapi + fake probes driven by the scenario timeline, `run --sim` hidden flag wiring.
- Scenario files in `testdata/scenarios/` for every verdict and flag.

**Tests**
- Gate table tests: each layer opens/closes correctly; hysteresis (2 fails → still open, 3 → closed); recovery reopens dependents; ICMP-blocked network still opens L4 via TCP; on-link resolver doesn't depend on L4.
- Verdict tests: every verdict and info flag has at least one scenario producing it; priority ordering when multiple apply; exit-code mapping.
- Scenario runner test: each file in `testdata/scenarios/` runs to completion under fake clock and asserts its own `expect` block.

**Acceptance**
- [ ] One scenario per verdict and per flag, all green
- [ ] `netdiag run --sim testdata/scenarios/gw_down.json` runs (plain output is fine at this phase)

---

## Phase 4 — Remaining probes

**Build**
- TCP connect; DNS-01 (`DnsQuery_W`, bypass cache); DNS-02 (miekg/dns UDP 53); STUN (pion/stun) incl. public IP and same-socket NAT-01; HTTP-01 (no redirect follow, body/status check); SVC-01 (resolve + TLS handshake timing); MTU-01 (DF binary search; buffer-size rule and status handling per SPEC §5 note); TRC-01 (TTL cycling every `trace.interval_ms`, per-hop series, multiple IPs per hop, async cached reverse DNS); custom ICMP/TCP targets with hostname re-resolution.
- Wire all probes into scheduler + gates.

**Tests**
- Unit with local fakes, no internet: TCP probe against an `httptest`/`net.Listen` on loopback (ok) and a closed port (fail); HTTP-01 against `httptest` servers returning the right body, wrong body, 302 → captive classification; STUN against a minimal in-test UDP STUN responder (pion/stun can build responses) incl. mapped-address parsing and NAT-01 comparison; DNS-02 against an in-test miekg/dns server (ok, SERVFAIL, timeout); MTU binary search against a fake ICMP function with a fixed max size; trace hop assembly from fake TTL responses incl. ECMP and non-final-hop loss.
- Integration: each real probe once against its default public target; DNS-01 against the system resolver; MTU to gateway returns 1500 or a plausible value; trace to `1.1.1.1` ends at the target.

**Acceptance**
- [ ] Every probe ID from SPEC §5 except TEAMS/MEET/MEDIA/SRC exists with unit tests
- [ ] Integration suite passes on a normal home/office connection

---

## Phase 5 — Console view

**Build**
- `internal/console`: VT enable, mtr-style layout per SPEC §9, width-aware truncation, greyed skipped rows, dimmed non-final hop loss, 1 Hz redraw (cursor home, no clear), keys `q`/`r`, plain mode when not a TTY, final summary.

**Tests**
- Golden-file tests: render fixed snapshots (from scenarios) at widths 80, 120, 200 → compare to `testdata/golden/*.txt` (with ANSI stripped and a second set with ANSI). `-update` flag regenerates goldens.
- Plain mode: only changes + periodic summary are printed (fake clock).
- Keys: injected key stream → `r` resets store, `q` cancels context.
- Console mode restore: with the fake console, original stdin/stdout modes are restored after `q`, after context cancel (Ctrl+C), after a returned fatal error, and after a panic inside the render loop; `ENABLE_PROCESSED_INPUT` is never cleared.
- Truncation: long hostnames never exceed width.
- Sparkline: table tests for scaling (flat series, single spike), `×` for lost, space for skipped, threshold colouring, newest on the right; column dropped at narrow widths; absent in plain mode.
- Call quality line uses MEDIA-01 when present and falls back to NET-01 `1.1.1.1` (label changes).

**Acceptance**
- [ ] `netdiag run --sim …` shows the full console view
- [ ] `netdiag run > out.txt` produces plain lines
- [ ] Goldens reviewed and committed

---

## Phase 6 — Report, CSV logging, web server (backend)

**Build (report + CSV)**
- `internal/report`: report model per SPEC §11, including whole-run running aggregates per target (no stored samples) and `csv_files`.
- `internal/csvlog`: samples, summary and events writers per SPEC §11.1; separator/decimal/BOM options; quoting; 1 s buffered flush + flush on exit; daily rotation; `csv.max_mb` numbered files; write-failure warning + 30 s reopen retry without stopping probes.
- Wire `--csv <dir>` / `csv.dir`; console header shows `CSV: <dir>`.

**Tests (report + CSV)**
- Whole-run aggregates equal stats computed over the full scenario sample list (scenario runner collects samples only in the test).
- Golden CSV files from a short scenario (`testdata/golden/csv/`) for `;` + `,` + BOM and for `,` + `.` without BOM.
- Quoting of values containing the separator or quotes; skipped rows carry the reason; empty `rtt_ms` on fail.
- Rotation at midnight with fake clock; size cap starts `_2` file; append to existing file keeps one header.
- Failure path: injected writer error → warning flag set, probing continues, reopen retried after 30 s (fake clock).
- Config validation: separator and decimal mark equal → error.
- Report size does not grow with run length (long scenario vs short scenario, same target set → similar size).

**Build (web backend)**
- `internal/web`: listener on `127.0.0.1` with port fallback, Host check middleware, `/api/state`, `/api/history`, `/api/events` (SSE, 1 Hz, flush, heartbeat comment, client disconnect handling), `/api/report`, embedded assets placeholder.
- Web banner for CSV write warnings.

**Tests (web backend, `httptest`, no browser)**
- Host check: `127.0.0.1:<port>` and `localhost:<port>` → 200; `evil.example`, wrong port, missing Host → 403.
- Bind: listener address is loopback; busy port → fallback port reported.
- `/api/state` JSON matches schema for a sim snapshot; `/api/history` returns ordered points with gaps for skipped.
- SSE: correct headers (`text/event-stream`, no-cache), receives ≥ 2 events in 2.5 s (fake or real ticker), stops writing after client disconnect (no goroutine leak).
- `/api/report`: attachment header, valid JSON, same content as report model.
- Assets served with correct content types; no response contains an absolute `http(s)://` URL to another host.

**Acceptance**
- [ ] All handler, report and CSV tests pass
- [ ] `curl http://127.0.0.1:<port>/api/state` works while `run --sim` is running
- [ ] `run --sim … --csv out\` produces three CSV files that open correctly in Excel with Czech regional settings

---

## Phase 7 — Web UI (frontend) + browser tests

**Build**
- Page per SPEC §10, vanilla JS + vendored uPlot, embedded with `go:embed`. Stable `data-testid` attributes on every element tests touch (verdict banner, flags, gate chain items, target rows, graph containers, hops rows, export button, connection indicator, call-quality status and graph, threshold lines, help buttons/popups, CPU level and graph).
- Help texts in one embedded file (e.g. `internal/web/assets/help.json`); threshold values injected from config.
- Expose per-graph data and threshold values as `data-` attributes (or a test-only JS accessor) so tests check them without reading pixels.

**Tests**
- Browser e2e (`-tags e2e`, chromedp, real Edge/Chrome, headless) against `run --sim` started in-process on a random port — full list in `docs/TESTING.md` §5. At minimum: loads with no console errors; zero requests to non-loopback hosts; verdict banner matches scenario and changes when scenario changes; gate chain states; graphs draw (canvas present, non-empty) and show gaps for skipped; tables update; export downloads valid JSON; SSE reconnect after server restart; layout at 360 px and 1280 px has no horizontal scroll; dark mode renders; threshold lines at configured values; every graph has subtitle and working "?" help; call-quality graph source label and fallback; CPU section shows level changes from a `cpu_high` scenario.
- Screenshots of each scenario saved to `testdata/out/ui/` (git-ignored) for review.

**Acceptance**
- [ ] e2e suite green on a Windows dev machine
- [ ] Screenshots reviewed for gw_down, isp_loss, teams_partial, ok

---

## Phase 8 — Teams and Meet diagnostics

**Build**
- TEAMS-01 (per-port STUN over `teams.udp_ports`, default 3478), TEAMS-02 (TCP 443), MEET-01, MEDIA-01 (UDP STUN quality series + MOS source), SRC-01 (source port 50000–50059, fall back to next free port in range, report if none bindable).
- Teams tests stay disabled with a clear console/UI note (`TEAMS_DISABLED`) when `teams.relays` is set to `[]`.
- Console Teams/Meet line and web panel.

**Tests**
- Unit with in-test STUN responders listening on multiple ports: all ports open → no verdict; one port closed → `TEAMS_PARTIAL_PORTS` naming the feature; all closed + TCP ok → `TEAMS_UDP_BLOCKED`.
- Relay unreachable (SPEC §8.2): relay DNS fails, or all UDP ports and TCP 443 fail while MEET-01 or RT-01 works → `TEAMS_RELAY_UNREACHABLE`, no `TEAMS_UDP_BLOCKED`/`TEAMS_PARTIAL_PORTS`, status text "Teams test relay not responding - result unknown"; scenario `teams_relay_unreachable`.
- SRC-01: occupied ports in range handled; range exhausted → flag.
- Scenarios + golden + e2e updated for Teams/Meet states.
- Integration: MEET-01 against Google STUN; TEAMS-01 port 3478 and TEAMS-02 against `worldaz.tr.teams.microsoft.com`.

**Acceptance**
- [ ] Teams/Meet verdicts covered by scenarios
- [ ] Disabled-state message shown when relays not configured

---

## Phase 9 — Headless mode, hardening, release

**Build**
- `run --headless --duration --json`: no UI, no listener, report written, exit code = verdict.
- Adapter-change handling end-to-end (series restart, gate re-evaluation).
- `--log` file logging.
- Build script producing `netdiag.exe` with version info from git tag.

**Tests**
- Headless with `--sim`: correct exit code per scenario; report file valid and complete; with `--csv`, report `csv_files` lists the files written.
- Adapter change scenario: Wi-Fi → Ethernet mid-run resets series and re-evaluates gates.
- Shutdown: all goroutines gone within 2 s (leak test).
- Full suite: `go test ./...`, `-tags integration`, `-tags e2e` all green.

**Manual field checklist (record results in `docs/FIELD_TESTS.md`)**
- [ ] Normal home Wi-Fi
- [ ] Ethernet
- [ ] Cable unplugged → `NO_LINK`
- [ ] Wi-Fi with no DHCP → `APIPA`
- [ ] Hotel/guest Wi-Fi with portal → `CAPTIVE_PORTAL`
- [ ] Phone hotspot (often symmetric NAT)
- [ ] Network with UDP blocked (firewall rule) → `UDP_BLOCKED` / `TEAMS_UDP_BLOCKED`
- [ ] Run as standard (non-admin) user
- [ ] Double-click from Explorer
- [ ] After quitting with `q` and with Ctrl+C, typing in the same console window echoes normally
- [ ] Windows 11 with Location Services off → SSID shown as hidden, no errors
