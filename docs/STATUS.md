# netdiag — Status (session handover)

Read this first in every session. Detail lives in `docs/SPEC.md`, `docs/PHASES.md`, `docs/TESTING.md`.

## Current state

- **Last completed phase:** 5 — console view (`internal/console`), awaiting review.
- **Next:** review fixes for Phase 5 if any, else Phase 6 — report, CSV logging, web server (backend).
- `netdiag run` and `run --sim` show the mtr-style live view on a console (plain lines when redirected) until q / Ctrl+C / end of scenario, then a final summary. `--headless` is still a stub (Phase 9).

## Completed phases

| Phase | Commit | Summary |
|---|---|---|
| 0 | `dfd3d3e` | Module, package layout, CLI (`run`, `save-config`, `help`, `version`), config load/merge/validate, asInvoker manifest |
| 0 fixes | `6933a8d` | LF line endings, `go 1.27`, non-replacing save, help flags, "required" errors, Teams relay-unreachable spec |
| 1 | `0c6fd1c` | sysapi: adapters, best route, ARP, Wi-Fi, change notifications, CPU, console modes/keys, fakes |
| 1 fixes | `d24e6fc` | Primary adapter selection, ARP neighbor states, one ARP in flight, Wi-Fi band certainty, route-aware polling |
| 2 | `1c1a33b` | clock (fake), probe Sample + ICMP (`IcmpSendEcho2`), stats, store, scheduler |
| 2 fixes | `48c5645` | Window jitter = mean \|ΔRTT\|, series independence + panic recovery, hardware-only fallback, ICMP min timeout |
| 3 | `c21a8be` | gate L1–L5, verdict + flags + exit codes, engine (series catalog), sim (35 scenarios), `run --sim` |
| 3 fixes | `b68c35d` | HTTP-01 at L3 + `CAPTIVE_PORTAL` over ISP/DNS verdicts, `GW_ICMP_RATE_LIMITED`, NET-04 L3 recovery (`Sample.Recovery`), 4 new scenarios (39) |
| 4 | `f766bc7` | probes: TCP, HTTP-01, SVC-01 (TLS), DNS-01 (`DnsQuery_W`) / DNS-02 (miekg), STUN + NAT-01 (pion), MTU-01, TRC-01 tracer, LAN-02, SYS-01, custom targets; `HTTP_CHECK_FILTERED`; `engine.RunLive` + `run` wiring |
| 4 fixes | `b55db9c` | MTU-01 paced/confirmed/`Bound`, `MTU_ISSUE` only for a silent black hole + `MTU_REDUCED:<mtu>`, `Sample.RerunAfter`; adapter change clears series, new generation, `adapter_change` event; SVC-01 multi-IP; 3 scenarios (43 total) |
| 5 | see `git log` | console: view model (`BuildView`), renderer (width-aware, sparkline, hops, colours), `console.Run` (TTY/plain, keys q/r, mode restore incl. panic), `sysapi.Console.Write` (WriteConsoleW) + `Height`, `Engine.Reset`, `sim.Play`/`LiveOn`, 66 goldens |

### Key decisions and SPEC deviations

- **Phase 0:** Teams relay default `worldaz.tr.teams.microsoft.com` (user choice; unofficial), `teams.udp_ports` default `[3478]` — the relay answers anonymous STUN only on 3478 (SPEC §4.1, §5 Teams ports). Explorer double-click: help + wait for Enter (§3). Headless exit code = worst verdict of the run (§8.1). Manifest via mingw `windres`, `.syso` committed; v1 is amd64 only (§2). save-config never overwrites: `MoveFileEx` without REPLACE (§3). Validation rules incl. IPv4-literal L4 targets (§4.5). Rates/timeouts keys (§4.4).
- **Phase 1:** primary adapter = best-route adapter, else hardware ethernet/Wi-Fi fallback (§5 LINK-01). Only ARP Reachable/Permanent count; stale = unconfirmed (§5 LAN-02, §6 L3). L3 also opens from L4 traffic (§6). Wi-Fi `band_source`; `WIFI_24GHZ` only when the band comes from the frequency (§5 WIFI-01). Change notifications with polling fallback that includes the best route (§5).
- **Phase 2:** **RTT is timed with QueryPerformanceCounter**, not Go's monotonic clock (§5 RTT). Scheduler: fixed grid, start offset [0, min(interval, 1 s)), missed ticks dropped, shutdown run emits nothing, panic → `local_error` (§5). Jitter = mean of \|RTT_i − RTT_{i−1}\| over consecutive ok samples, unknown below 2 pairs (§7). StDev population.
- **Phase 3:** gate rounds by a 1 s tick, one round per layer period from fresh evidence (≤ 1.5 × interval) instead of a pacer series (§6). Wait retries after min(interval, 1 s) (§6 start-up). L1/L2 without hysteresis; closing a layer makes the ones above unknown (§6). RT-01 with an unresolved STUN name → skipped (§6). Settled loss for `LAN_LOSS`/`ISP_LOSS`; `GW_ICMP_BLOCKED` needs internet working *now* (§8). Call-quality source rules (§8). `run --sim` uses defaults + scenario config, plain output (§3). New package `internal/engine` (CLAUDE.md layout).
- **Phase 3 fixes:** HTTP-01 requires L3 only; `CAPTIVE_PORTAL` wins over `ISP_DOWN`/`DNS_*` in any L4/L5 state (also L4 unknown), L1–L3 must be open (§6, §8). `LAN_LOSS` needs every NET-01 target lossy too (≥ half the LAN loss or > threshold), else flag `GW_ICMP_RATE_LIMITED`; undecided while NET-01 lacks `min_samples` (§8). NET-04 runs as an L3 recovery probe whenever L3 is **not open** (closed, and also unknown at start-up/reset — extension of the review request, avoids a false `GW_UNREACHABLE`/exit 13 at start with an ICMP-blocking gateway); its samples carry `Recovery=true` and count as skipped in stats/verdicts (§6). During a full outage behind such a gateway the verdict is `GW_UNREACHABLE` (nothing proves the gateway alive).
- **Phase 4 (rules recorded before the phase, now implemented):** `CAPTIVE_PORTAL` only when ≥ 1 HTTP-01 check is captive **and** none passes; otherwise flag `HTTP_CHECK_FILTERED:<host>` + scenario `http_check_filtered` (§4.3, §8). HTTP-01/SVC-01: own `http.Transport`, `Proxy: nil`, `DisableKeepAlives`, no redirects, probe timeout; test that `HTTP_PROXY`/`HTTPS_PROXY` are ignored (§4.3). DNS-01: per server, `DNS_QUERY_BYPASS_CACHE`, server in `pExtra` (`IP4_ARRAY`) or `DnsQueryEx` (`DNS_ADDR_ARRAY`); never `net.Resolver` for DNS-01; other probes may use the system resolver (§5).
- **Phase 4 decisions/deviations:** empty `expect_body` = body not compared (§4.3). `trace.interval_ms` minimum raised 100 → 200 (validation + tracer floor) so TRC-01 never exceeds 5 packets/s (§4.5, §5). DNS-01 uses `DnsQuery_W` + `IP4_ARRAY` (not `DnsQueryEx`); a timed-out query keeps running and the next one fails as busy (§5). SVC-01 is a bare TLS handshake, not an HTTP request (§5). RT-01/NAT-01 unresolved name → skipped, NAT-01 needs two STUN servers (§5). TRC-01 hops are series `TRC-01:NN` added at run time (`Engine.AddSeries`); `Snapshot.Hops` carries IPs/names/final flag (§5). `netdiag run` runs `engine.RunLive`: polls adapters/route/Wi-Fi every min(link, wifi) s, rebuilds series on a primary adapter change, plain output (§5 live run). `--log <file>` opens an slog file for warnings and probe panics.
- **Phase 4 fixes:** a smaller path MTU is not a problem by itself (PPPoE, VPN); the problem is a PMTUD black hole. MTU-01 echoes are ≥ 200 ms apart, every run records its bound (`Sample.Bound`: signalled = `IP_PACKET_TOO_BIG` seen above the result, silent = only timeouts), a result < 1500 must be confirmed (result answers again, result + 1 fails twice) or the run fails as `MTU result not confirmed`. `MTU_ISSUE` = two consecutive confirmed runs of one target < `mtu_min` with a silent bound; signalled → info flag `MTU_REDUCED:<mtu>`; the first low silent result asks for a re-run after 60 s (`Sample.RerunAfter`, honoured by `sched`) (§5, §8). Adapter change = new generation: all series cleared except SYS-01 (custom targets too), stale-generation samples dropped (`Engine.SinkFor`), `adapter_change` event (`Snapshot.AdapterEvents`, printed in plain output), `SetSeries` removes series not in the new catalog (`Store.ResetSeries`/`Retain`) (§6). SVC-01 tries every resolved IPv4 within the timeout (§5).

- **Phase 5 decisions/deviations (SPEC §9 updated):** the screen is cleared once at start, then cursor-home redraws; frames are cut to the window **height** with `… N more lines: enlarge the window` (a taller frame scrolls and breaks cursor-home redraws, seen in a real terminal); last column left free. MTU-01 rows show `path MTU <n>`, no RTT columns/sparkline; SYS-01 only in the header. Columns shrink: sparkline first, then Host (min 10), then Name (min 6). A second without a sample is a space in the sparkline. Plain mode: changes + adapter events + a summary block on a fixed 30 s grid (indented, table at width 120) and `final:` block; also used when VT cannot be enabled on a console (written via WriteConsoleW). `NO_COLOR` counts when set at all (even empty). Main cancels on SIGTERM too (console close/logoff). Header: gateway MAC from the newest ok LAN-02 Detail, public IP from STUN mapped address, `Web UI: off` until Phase 6. `RunLive`/`sim.LiveOn` take the console's engine; their own plain printing is discarded (`Out` nil). Sim LAN-02 ok samples now carry the scenario gateway MAC.

## Open items

- **Review fixes pending:** none for Phases 1–4.
- **Wiring still missing:** TEAMS-01/02, MEET-01, MEDIA-01, SRC-01 probes (Phase 8; their series exist for sim/verdicts but are not scheduled in a live run; the Teams line shows `-`); `CSV_WRITE_ERROR` is only set by sim events until Phase 6; `--headless` (Phase 9).
- **Phase 6 must add:** web URL in `console.Meta.WebURL`; `r` must also reset the web graphs (shared `Engine.Reset`) and write a `reset` row to the events CSV (SPEC §9).
- **Phase 9 hardening (memory on long headless runs, SPEC §12):** `Gates.history` (all gate transitions) and the engine's adapter events (`Engine.events`) grow without bound. Cap both (e.g. keep the last 1000), count what was dropped and report the count (report JSON, `GateHistory`/`AdapterEvents` callers) so a truncated history is visible. Add tests for the cap and the count.
- **Known limitations:**
  - Simulations (real-time `Live`) can show `QUALITY_POOR_VOIP` for ~1 s when the internet drops, before the gates close (settled loss is not applied to MOS; seen in `isp_down`).
  - Very long hop lists or many custom targets are cut to the window height (no scrolling of the live view).
  - `sim.Live` (`run --sim`) resets gates on an adapter change but does not rebuild series; the real `engine.RunLive` does rebuild them (Phase 4). Old-adapter series are removed from the store by `SetSeries`.
  - After a gateway recovery the verdict shows `CHECKING` for about a second while L4/L5 are re-decided (by design).
  - Settled loss applies to `LAN_LOSS`/`ISP_LOSS` only, not to MOS.
  - `--sim` cannot be combined with `--headless` yet (Phase 9).
- **Phase 4 not verified:** HTTP-01 / SVC-01 on a network with TLS interception or a real captive portal (only httptest + the open internet); DNS-01 against several adapter DNS servers (one server on this VM); MTU-01 bursts (up to ~25 echoes in a run) against a real rate-limiting router; Wi-Fi roam rebuild.
- **Unverified on real hardware (dev VM has no Wi-Fi, no Hyper-V switch):** Wi-Fi queries and the Windows 11 location-privacy path (fake-tested only); hardware-only fallback with a real `vEthernet` adapter. Covered by Phase 9 field tests.
- **"To confirm":** nothing left in SPEC. The Teams relay is unofficial and may stop answering (handled: `TEAMS_RELAY_UNREACHABLE`, SPEC §8.2).

## Environment

- Windows 10 Enterprise LTSC 2019 (build 17763) VM, standard user (no admin), Ethernet only (VirtIO NIC, reports hardware=true), gateway `10.102.200.1`, no WLAN interface (wlansvc running). Path MTU to the internet only ~1400–1420.
- Go 1.27.2 (`go.mod`: `go 1.27`). `go env` has `CGO_ENABLED=1` (mingw installed) → **always set `CGO_ENABLED=0`** except for `-race`.
- Tools: golangci-lint v2.15 (`%USERPROFILE%\go\bin`), mingw `windres` + `gcc` (`C:\mingw64\bin`), git 2.55, Chrome (no Edge) for future e2e.
- Allowed deps (from proxy.golang.org) now in `go.mod`: `golang.org/x/sys`, `github.com/miekg/dns` (DNS-02), `github.com/pion/stun/v3` (STUN). chromedp comes with Phase 7.

### Commands (PowerShell)

```
$env:CGO_ENABLED='0'; go vet ./...; go test ./...           # unit + handler + scenario tests
$env:CGO_ENABLED='0'; go test -tags integration ./...       # real Windows APIs + network
$env:CGO_ENABLED='1'; go test -race ./...                   # race detector (needs mingw gcc)
golangci-lint run --build-tags integration ./...
go generate ./cmd/netdiag                                   # rebuild manifest .syso (windres)
$env:CGO_ENABLED='0'; go build -o netdiag.exe ./cmd/netdiag
.\netdiag.exe run --sim testdata\scenarios\gw_down.json     # real-time demo (130 s)
go test -tags integration -v -run Sysinfo ./internal/sysapi/ # prints real sysinfo
```

e2e (`-tags e2e`, chromedp) does not exist yet (Phase 7). Console goldens: `go test ./internal/console -update` (review the diff in `testdata/golden/console/`).

### Lessons learned (non-obvious)

- Lock order: `Engine.Snapshot` holds the engine lock and calls `Tracer.Hops` (tracer lock); the tracer must never call the engine (`Register`/`Emit`) while holding its lock → `Step` registers new hops after unlocking (found as a deadlock in `RunLive` tests).
- `http.ProxyFromEnvironment` skips localhost and caches the environment once per process, so the no-proxy test dials a fake host name through a `DialFunc` hook and asserts on the Transport itself as well.
- `DnsQuery_W` with an IP4_ARRAY in `pExtra` works against `127.0.0.x:53` (no admin): the integration test runs its own UDP DNS servers there and counts queries (proves server selection and cache bypass). A closed port answers ICMP unreachable = `WSAECONNRESET` (10054), classified `unreachable`.
- Windows echo with TTL < path length returns `IP_TTL_EXPIRED_TRANSIT` from the router with RTT: a hop that answers is an ok sample; only a timeout is hop loss.
- The Bash tool breaks heredocs with apostrophes: write scripts/files with the Write tool (scratchpad) and run them.
- Console view check: run `.\netdiag.exe run --sim testdata\scenarios\lan_loss.json` in a real terminal tab; the terminal panel is short (~30 rows), which is how the frame-height scroll bug showed up.
- Fake-clock render loops: take the time from the ticker value, not `Clock.Now()` (the test may already have advanced the clock).

- Go's monotonic clock on Windows reads interrupt time (0.5–15.6 ms steps): sub-ms RTTs read as 0 → use QPC (`internal/probe/qpc_windows.go`).
- `os.Rename` on Windows replaces an existing file → `MoveFileEx(…, 0)` for "never overwrite".
- `json.Unmarshal` into an existing slice reuses its elements → config merge clears user-set slices first (else entries inherit default fields).
- `IcmpSendEcho2`: reply buffer per SPEC §5; GetLastError carries IP_* statuses (11010 timeout etc.); DF "packet too big" comes back with no reply struct.
- `ResolveIpNetEntry2` returns `Reachable` for a live gateway, takes ~3 s to fail and cannot be cancelled → `SerialARP`.
- `GetConsoleProcessList` is 1 only in a console of our own: tested by re-running the test binary with `CREATE_NEW_CONSOLE` (opens `CONIN$`/`CONOUT$`; `go test` redirects std handles).
- Hand-written Win32 structs (`MIB_IPNET_ROW2`, `INPUT_RECORD`, WLAN, ICMP) have size/offset tests; keep them when adding structs.
- Fake clock: `NewTimer(0)` fires at once without registering; `WaitTimers(n)` before `Advance`; the scheduler takes the tick time from the timer channel.
- Scenario timing: L3 opens at 0 s, L4/L5 at 1 s, other series start at 2 s; blocked checks need 3 consecutive failures; the 60 s loss window keeps outage failures visible after recovery.
- Tooling: PowerShell 5.1 mangles quotes in `git commit -m` → write the message to a file and use `git commit -F`. The Bash tool breaks multi-line scripts containing apostrophes → write the script to the scratchpad and run it.
