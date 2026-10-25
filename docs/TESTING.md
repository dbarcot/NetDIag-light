# netdiag — Testing guide

Testing is part of every phase, not a final step. Write tests while writing code; run `go vet ./...` and `go test ./...` after every meaningful change.

## 1. Test levels

| Level | Command | Touches network / OS? | When |
|---|---|---|---|
| Unit | `go test ./...` | No. Fakes only, loopback sockets allowed | every change |
| Handler (web backend) | `go test ./...` (part of unit) | Loopback only via `httptest` | every change in `internal/web` |
| Scenario | `go test ./...` (part of unit) | No. Sim mode + fake clock | every change in gate/verdict/console/web |
| Integration | `go test -tags integration ./...` | Yes: real Windows APIs + internet | end of each phase that adds OS/probe code |
| Browser e2e | `go test -tags e2e ./internal/web/...` | Loopback only + local Edge/Chrome | every change in web UI, end of phases 7–9 |
| Field | manual, `docs/FIELD_TESTS.md` | Real networks | phase 9 |

Unit and scenario tests must be deterministic: injected clock, no sleeps longer than needed, no reliance on timing of the real network.

## 2. Fakes

Every OS/network boundary has an interface and a fake in the same package (`fake.go`, no build tag):

- `sysapi.Adapters`, `sysapi.Routes`, `sysapi.ARP`, `sysapi.WiFi`, `sysapi.Console`
- `probe.ICMP` (send echo with TTL/DF/size), `probe.Dialer` (TCP), `probe.Resolver` (system DNS)
- `clock.Clock` (`Now`, `NewTimer`, `NewTicker`) with a manual fake that advances explicitly

Prefer real loopback servers over fakes where cheap: `httptest.Server` for HTTP, an in-test UDP STUN responder (built with pion/stun/v3), an in-test miekg/dns server, `net.Listen("tcp", "127.0.0.1:0")` for TCP.

## 3. Scenarios (simulation mode)

Scenario files drive fake sysapi and fake probes over a timeline. They are used by: scenario tests, console golden tests, web e2e tests, headless exit-code tests, and demos (`netdiag run --sim <file>`).

Location: `testdata/scenarios/*.json`. Format:

```json
{
  "name": "gw_down",
  "description": "Gateway stops answering ping and ARP at t=20s, recovers at t=60s",
  "duration_s": 90,
  "sysinfo": {
    "adapter": { "name": "Wi-Fi", "type": "wifi", "up": true, "ipv4": "192.168.1.23/24" },
    "gateway": { "ip": "192.168.1.1", "mac": "aa:bb:cc:dd:ee:ff" },
    "dns_servers": ["192.168.1.1"],
    "wifi": { "ssid": "Home", "signal_pct": 78, "band": "5GHz", "channel": 36 }
  },
  "default": { "result": "ok", "rtt_ms": [12, 3] },
  "rules": [
    { "probe": "LAN-01", "from_s": 0,  "to_s": 90, "rtt_ms": [2, 0.5] },
    { "probe": "LAN-01", "from_s": 20, "to_s": 60, "result": "fail" },
    { "probe": "LAN-02", "from_s": 20, "to_s": 60, "result": "fail" },
    { "probe": "NET-01", "target": "8.8.8.8", "loss_pct": 5 }
  ],
  "events": [
    { "at_s": 45, "sysinfo": { "wifi": { "signal_pct": 30 } } }
  ],
  "expect": [
    { "at_s": 10, "verdict": "OK" },
    { "at_s": 30, "verdict": "GW_UNREACHABLE", "skipped": ["NET-01", "DNS-01"] },
    { "at_s": 75, "verdict": "OK", "flags_include": ["WIFI_WEAK"] }
  ]
}
```

- `rtt_ms: [mean, stddev]` gives deterministic pseudo-random RTTs (seeded by scenario name).
- `loss_pct` drops a deterministic fraction of samples.
- Rules are matched last-wins by `probe` + optional `target` + time range.
- `expect` entries are asserted by the scenario runner test.

**Required scenario set (at least):** `ok`, `no_link`, `apipa`, `no_address`, `gw_down`, `gw_icmp_blocked`, `captive_portal`, `isp_down`, `icmp_blocked`, `dns_local_fail`, `dns_fail`, `udp_blocked`, `teams_udp_blocked`, `teams_partial_3480` (sets `teams.udp_ports` to 3478–3481), `teams_disabled_no_relays`, `service_unreachable`, `checking_startup` (`CHECKING` before gates decide), `csv_write_error`, `meet_udp_blocked`, `mtu_issue`, `lan_loss`, `isp_loss`, `poor_voip`, `symmetric_nat`, `wifi_weak_24ghz`, `wifi_ssid_hidden`, `cpu_high`, `no_media_fallback` (MEDIA-01 unavailable → call quality from NET-01), `custom_target_down`, `adapter_change`, `flapping_gateway` (checks hysteresis), `recovery` (all gates reopen).

## 4. Console tests

- Golden files in `testdata/golden/console/<scenario>_<width>.txt` (ANSI stripped) and `..._ansi.txt`. Goldens include the sparkline column and the call quality line (sparkline characters are UTF-8; goldens are compared as UTF-8).
- Render a snapshot taken at a fixed scenario time; widths 80, 120, 200.
- `go test ./internal/console -update` regenerates goldens; diffs must be reviewed before committing.
- Also test: plain (non-TTY) output sequence for a scenario; key handling with an injected key stream.

## 4a. Report and CSV tests

- Report: whole-run aggregates are compared against stats computed from the full sample list collected by the test harness (the app itself never stores all samples).
- CSV goldens in `testdata/golden/csv/<scenario>_<variant>/` (`semicolon_bom`, `comma_nobom`); `-update` regenerates.
- Fake filesystem/writer for: rotation at midnight, size-cap numbering, append with single header, write error → warning + retry.
- Timestamps in goldens come from the fake clock with a fixed time zone offset so files are reproducible.

## 5. Web UI tests

### 5.1 Handler tests (`httptest`, always run)

- Host header allowlist (accept `127.0.0.1:<port>`, `localhost:<port>`; reject others with 403).
- Listener bound to loopback; port fallback.
- `/api/state`, `/api/history`, `/api/report` shapes and content for sim snapshots.
- SSE headers, event cadence, disconnect cleanup, heartbeat.
- Assets: content types; no absolute URLs to other hosts in any served file.

### 5.2 Browser e2e (`-tags e2e`, chromedp)

Setup per test: start the full app in-process with `--sim <scenario>` on a random port and a fake clock that the test can advance (expose a test-only hook, not an HTTP endpoint). Launch headless Edge or Chrome, found in this order: env `NETDIAG_BROWSER` (full path), `%ProgramFiles(x86)%\Microsoft\Edge\Application\msedge.exe`, `%ProgramFiles%\Microsoft\Edge\Application\msedge.exe`, `%ProgramFiles%\Google\Chrome\Application\chrome.exe`, `%ProgramFiles(x86)%\Google\Chrome\Application\chrome.exe`, `%LOCALAPPDATA%\Google\Chrome\Application\chrome.exe`. Skip with a clear message if none is found. Use a temporary user-data dir per test run. Fail the test on any JS console error or uncaught exception.

Required cases:

1. **Offline/self-contained:** record all network requests; every URL host is `127.0.0.1:<port>`.
2. **Initial render:** verdict banner text = scenario verdict; system info fields populated; gate chain shows correct states.
3. **Live update:** advance the scenario past a transition → banner and gate chain change within 2 s without reload.
4. **Graphs:** each graph container holds a canvas with non-zero size; after advancing time the series point count grows; a skipped period renders as a gap (check via exposed chart data or `data-` attributes, not pixels).
5. **Tables:** target rows count matches config; values match `/api/state`; skipped rows have the skipped style and reason.
6. **Teams/Meet panel:** per-port states for `teams_partial_3480`; disabled note for `teams_disabled_no_relays`.
7. **Hops table:** hops present; non-final loss shown dimmed.
8. **Export:** clicking export downloads a file; it parses as JSON, its verdict matches the banner, it contains whole-run stats and `csv_files` (when the test runs with CSV on), and no raw sample arrays.
8a. **CSV warning:** with an injected CSV write error, the web banner shows the CSV warning.
9. **SSE reconnect:** stop and restart the server on the same port → "connection lost" indicator appears, then clears and updates resume.
10. **Responsive:** viewport 360×740 and 1280×800 → `document.documentElement.scrollWidth <= innerWidth`.
11. **Dark mode:** emulate `prefers-color-scheme: dark` → body background differs from light; text remains visible (contrast check on banner).
12. **Host rejection from browser:** navigate to `http://localhost:<port>/` works; a request with a forged Host via chromedp fetch override gets 403.
13. **Threshold lines:** latency graphs expose a threshold of `thresholds.rtt_ms`, jitter graphs `thresholds.jitter_ms`; changing the config value in the test changes the line.
14. **Help:** every graph/section has a non-empty subtitle; clicking each "?" opens a popup with non-empty text and it closes with Esc and with the close button; help texts file has an entry for every `data-help-id` on the page.
15. **Call quality:** status text matches MOS from `/api/state` (Good/Fair/Poor); source label is MEDIA-01 normally and NET-01 in `no_media_fallback`.
16. **System load:** `cpu_high` scenario → level changes Light → High within 2 s of the transition, CPU graph point count grows, `CPU_HIGH` flag shown; call-quality and CPU graphs share the same visible time range.

Screenshots: full-page PNG for each scenario × viewport into `testdata/out/ui/` (git-ignored) for human review at the end of phases 7–9.

Selectors: use only `data-testid` attributes; never CSS classes or text positions.

## 6. Integration tests (`-tags integration`)

Run on a real Windows machine with internet. They verify the OS bindings and real probes, not logic. Each test must:
- Skip (not fail) when a precondition is missing (no Wi-Fi adapter, no internet), with a clear reason.
- Use only targets from the default config or loopback.
- Run each probe a few times at most; respect the 1/s limit.
- Not assume a 1500-byte path MTU: virtualised and tunnelled networks are often lower (the dev VM measures ~1400–1420). MTU tests assert a value in 576–1500, not 1500.
- Teams relay tests assert only port 3478 (see SPEC §5 note on Teams ports).

## 6a. Build environment

- All builds and tests run with `CGO_ENABLED=0` (the dev machine has `CGO_ENABLED=1` in `go env` because mingw is installed). `-race` needs cgo and is therefore not used.
- The `asInvoker` manifest is compiled from `cmd/netdiag/netdiag.rc` + `netdiag.manifest` with mingw `windres` into `cmd/netdiag/rsrc_windows_amd64.syso`, which is committed; regenerate only when the manifest or version resource changes.

## 7. Conventions

- Table-driven tests; test names describe behavior (`TestGate_ClosesAfterThreeFailures`).
- `t.Parallel()` where there is no shared state.
- Goroutine-leak check in scheduler, web and shutdown tests (compare `runtime.NumGoroutine` before/after with a short settle, or use a small helper).
- No test sleeps over 100 ms in unit tests; use the fake clock.
- A bug fix always comes with a test that failed before the fix.
