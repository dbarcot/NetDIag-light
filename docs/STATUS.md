# netdiag — Status (session handover)

Read this first in every session. Detail lives in `docs/SPEC.md`, `docs/PHASES.md`, `docs/TESTING.md`.

## Current state

- **Last completed phase:** 3 — gates, verdicts, simulation mode (`c21a8be`). Phase 3 has not been reviewed yet.
- **Next phase:** 4 — remaining probes (TCP, DNS-01/02, STUN/NAT, HTTP-01, SVC-01, MTU-01, TRC-01, custom targets) and wiring them into scheduler + gates.
- `netdiag run` without `--sim` still only loads the config and prints "Probing is not implemented in this build yet".

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

### Key decisions and SPEC deviations

- **Phase 0:** Teams relay default `worldaz.tr.teams.microsoft.com` (user choice; unofficial), `teams.udp_ports` default `[3478]` — the relay answers anonymous STUN only on 3478 (SPEC §4.1, §5 Teams ports). Explorer double-click: help + wait for Enter (§3). Headless exit code = worst verdict of the run (§8.1). Manifest via mingw `windres`, `.syso` committed; v1 is amd64 only (§2). save-config never overwrites: `MoveFileEx` without REPLACE (§3). Validation rules incl. IPv4-literal L4 targets (§4.5). Rates/timeouts keys (§4.4).
- **Phase 1:** primary adapter = best-route adapter, else hardware ethernet/Wi-Fi fallback (§5 LINK-01). Only ARP Reachable/Permanent count; stale = unconfirmed (§5 LAN-02, §6 L3). L3 also opens from L4 traffic (§6). Wi-Fi `band_source`; `WIFI_24GHZ` only when the band comes from the frequency (§5 WIFI-01). Change notifications with polling fallback that includes the best route (§5).
- **Phase 2:** **RTT is timed with QueryPerformanceCounter**, not Go's monotonic clock (§5 RTT). Scheduler: fixed grid, start offset [0, min(interval, 1 s)), missed ticks dropped, shutdown run emits nothing, panic → `local_error` (§5). Jitter = mean of \|RTT_i − RTT_{i−1}\| over consecutive ok samples, unknown below 2 pairs (§7). StDev population.
- **Phase 3:** gate rounds by a 1 s tick, one round per layer period from fresh evidence (≤ 1.5 × interval) instead of a pacer series (§6). Wait retries after min(interval, 1 s) (§6 start-up). L1/L2 without hysteresis; closing a layer makes the ones above unknown (§6). RT-01 with an unresolved STUN name → skipped (§6). Settled loss for `LAN_LOSS`/`ISP_LOSS`; `GW_ICMP_BLOCKED` needs internet working *now* (§8). Call-quality source rules (§8). `run --sim` uses defaults + scenario config, plain output (§3). New package `internal/engine` (CLAUDE.md layout).

## Open items

- **Review fixes pending:** none (Phase 1 and Phase 2 review lists are done). Phase 3 not reviewed yet.
- **Wiring still missing (Phase 4+):** real LAN-02 / SYS-01 / LINK-01 / WIFI-01 probes into the engine; TCP, DNS, STUN, NAT, HTTP, SVC, MTU, TRC probes; TRC-01 is not in the series catalog yet; RT-01 must record "name not resolved" as skipped; `CSV_WRITE_ERROR` is only set by sim events until Phase 6.
- **Known limitations:**
  - `sim.Live` (`run --sim`) resets gates on an adapter change but does not rebuild series (series restart is Phase 9).
  - After a gateway recovery the verdict shows `CHECKING` for about a second while L4/L5 are re-decided (by design).
  - Settled loss applies to `LAN_LOSS`/`ISP_LOSS` only, not to MOS.
  - `--sim` cannot be combined with `--headless` yet (Phase 9).
- **Unverified on real hardware (dev VM has no Wi-Fi, no Hyper-V switch):** Wi-Fi queries and the Windows 11 location-privacy path (fake-tested only); hardware-only fallback with a real `vEthernet` adapter. Covered by Phase 9 field tests.
- **"To confirm":** nothing left in SPEC. The Teams relay is unofficial and may stop answering (handled: `TEAMS_RELAY_UNREACHABLE`, SPEC §8.2).

## Environment

- Windows 10 Enterprise LTSC 2019 (build 17763) VM, standard user (no admin), Ethernet only (VirtIO NIC, reports hardware=true), gateway `10.102.200.1`, no WLAN interface (wlansvc running). Path MTU to the internet only ~1400–1420.
- Go 1.27.2 (`go.mod`: `go 1.27`). `go env` has `CGO_ENABLED=1` (mingw installed) → **always set `CGO_ENABLED=0`** except for `-race`.
- Tools: golangci-lint v2.15 (`%USERPROFILE%\go\bin`), mingw `windres` + `gcc` (`C:\mingw64\bin`), git 2.55, Chrome (no Edge) for future e2e.
- Allowed deps fetched fine from proxy.golang.org; only `golang.org/x/sys` is in `go.mod` so far.

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

e2e (`-tags e2e`, chromedp) does not exist yet (Phase 7).

### Lessons learned (non-obvious)

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
