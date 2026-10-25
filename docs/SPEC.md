# netdiag — Specification

## 1. Purpose

A single Windows exe that quickly locates common network problems: link vs LAN vs ISP vs DNS vs captive portal vs blocked UDP vs poor call quality, with specific checks for Microsoft Teams and Google Meet. Runs from the command line, no admin, no installer.

## 2. Scope

**In scope:** Windows 10/11 and Server 2016+; IPv4; console mtr-style live view; loopback read-only web UI with graphs; JSON config; dependency-gated tests; headless report mode.

**Out of scope (v1):** proxy handling; auto-opening a browser; editing targets at runtime (CLI or UI); throughput/bufferbloat tests; own server infrastructure; IPv6; other operating systems; **VPN detection and any VPN-specific logic** (if a VPN is on, tests run over whatever adapter Windows routes through, without special handling).

## 3. Command line

Running with **no arguments prints help and exits 0**.

```
netdiag run [--config <path>] [--port <n>] [--log <path>] [--csv <dir>]
netdiag run --headless --duration <d> --json <path> [--csv <dir>] [--config <path>]
netdiag save-config [<path>]
netdiag help | -h | --help
netdiag version
```

- `run` — start probing, console view and web UI.
- `--port <n>` — overrides `ui.port` (1–65535).
- `--headless` — no console view, no web UI; run for `--duration`, write the report to `--json`, exit with the exit code of the **worst** verdict of the run (§8.1).
- `--duration <d>` — Go duration syntax (`90s`, `5m`, `1h30m`); required with `--headless`, minimum `10s`. `--json` is required with `--headless`; an existing file is replaced (atomic write).
- Unknown command or flag, or a missing required flag → error message + short usage on stderr, exit 1.
- `version` prints `netdiag <version> (<commit>, <go version>)`; version and commit come from the git tag at build time (`dev` when unset).
- `--csv <dir>` — log every measurement to CSV files in `<dir>` (see §11.1). Overrides `csv.dir` from config.
- `save-config` — write the full default config (every key expanded) to `<path>` (default: `netdiag.json` next to the exe; if not writable, `%APPDATA%\netdiag\netdiag.json`). Refuses to overwrite an existing file. Writes atomically (temp file + rename). Prints the path written. This is the **only** way a config file is created.
- **Explorer double-click:** if the process is the only one attached to its console (`GetConsoleProcessList` returns 1), after printing help or after any non-zero exit (fatal, config or usage error) wait for Enter before exiting. `version` and successful commands do not wait.
- Hidden developer flag `run --sim <scenario.json>` replaces real sysapi and probes with a scripted scenario (see `docs/TESTING.md`). Not shown in help.

## 4. Configuration

- Defaults are embedded in the exe. A user file only overrides keys; partial files are valid.
- Lookup order: `--config <path>` → `netdiag.json` next to the exe → `%APPDATA%\netdiag\netdiag.json` → built-in defaults.
- Unknown keys, wrong types or invalid values: print an error naming the exact key path (e.g. `targets.ping[2].rate`) and exit with code 2.
- Ping targets are defined **only** in JSON. Changes need a restart.

### 4.1 Keys

| Key | Meaning | Default |
|---|---|---|
| `version` | config schema version | `1` |
| `ui.port` | web UI port | `48765` |
| `window.seconds` | rolling stats window | `60` |
| `window.min_samples` | samples before loss/quality verdicts | `10` |
| `history.minutes` | graph history kept in memory | `15` |
| `gate.fail_count` / `gate.ok_count` | hysteresis to close / reopen a gate | `3` / `3` |
| `targets.ping[]` | extra ICMP targets (see 4.2) | `[]` |
| `targets.tcp[]` | extra TCP connect targets `host:port` (see 4.2) | `[]` |
| `targets.internet_icmp` | core internet ICMP targets | `["1.1.1.1", "8.8.8.8"]` |
| `targets.internet_tcp` | core TCP check | `"1.1.1.1:443"` |
| `targets.public_dns` | public resolvers | `["1.1.1.1", "8.8.8.8"]` |
| `targets.dns_test_name` | name resolved by DNS tests | `"www.microsoft.com"` |
| `targets.stun` | generic STUN servers | `["stun.cloudflare.com:3478", "stun.l.google.com:19302"]` |
| `targets.http_check` | captive-portal checks | see 4.3 |
| `targets.services` | service DNS+TLS checks | `["teams.microsoft.com", "outlook.office365.com", "meet.google.com", "zoom.us"]` |
| `targets.trace` | traceroute / mtr hop target | `"1.1.1.1"` |
| `teams.relays` | Teams transport relay hosts/IPs | `["worldaz.tr.teams.microsoft.com"]` (relay used by Microsoft's Teams Network Assessment Tool); Teams tests disabled when set to `[]` |
| `teams.udp_ports` | UDP ports tested by TEAMS-01 | `[3478]` (see §5 note on Teams ports) |
| `meet.stun` | Google STUN targets for Meet checks | `["stun.l.google.com:19302"]` |
| `rates.*` | per probe type interval in seconds (min 1) | see 4.4 |
| `timeouts.*` | per probe type timeout in ms | see 4.4 |
| `thresholds.loss_pct` | loss % for `LAN_LOSS` / `ISP_LOSS` | `2` |
| `thresholds.rtt_ms` | RTT limit (quality verdict, threshold lines, sparkline colour) | `150` |
| `thresholds.jitter_ms` | jitter limit | `30` |
| `thresholds.mos_min` | MOS below this → `QUALITY_POOR_VOIP` | `3.6` |
| `thresholds.mos_good` | MOS at or above this → call quality "Good" | `4.0` |
| `thresholds.mtu_min` | path MTU below this → `MTU_ISSUE` | `1400` |
| `thresholds.wifi_signal_pct` | signal below this → `WIFI_WEAK` | `50` |
| `thresholds.cpu_moderate_pct` / `cpu_high_pct` / `cpu_heavy_pct` | CPU load level boundaries | `50` / `75` / `90` |
| `console.sparkline` | show sparkline column in console | `true` |
| `trace.max_hops` | | `30` |
| `trace.reverse_dns` | async PTR lookups for hops | `true` |
| `trace.interval_ms` | gap between trace probes (TTL cycling) | `200` (min `100`) |
| `limits.max_custom_targets` | cap on `targets.ping` + `targets.tcp` | `20` |
| `csv.dir` | directory for CSV logs; empty = CSV off | `""` |
| `csv.separator` | field separator, `;` or `,` | `";"` |
| `csv.decimal` | decimal mark, `,` or `.` (must differ from separator) | `","` |
| `csv.bom` | write UTF-8 BOM (for Excel) | `true` |
| `csv.summary_seconds` | interval of summary rows | `60` |
| `csv.rotate` | `daily` or `none` | `"daily"` |
| `csv.max_mb` | per-file size cap; when reached, a new numbered file starts | `100` |

### 4.2 Custom targets

```json
{
  "targets": {
    "ping": [
      { "name": "fileserver", "host": "10.0.0.5" },
      { "name": "intranet", "host": "intranet.corp.local", "rate": 2, "group": "internal" }
    ],
    "tcp": [
      { "name": "erp", "host": "erp.corp.local:443", "depends_on": "lan" }
    ]
  }
}
```

- `name` unique and required; `host` IP or hostname (TCP: `host:port`); `rate` seconds ≥ 1 (default 1); `group` free text for display.
- `depends_on`: `"lan"` or `"internet"`. If omitted: private IP → `lan`, public IP → `internet`, hostname → `internet` (and also DNS).
- Hostnames are re-resolved every 60 s; the resolved IP is shown.
- Custom targets are displayed and graphed but do not drive core verdicts.

### 4.3 HTTP checks

| URL | Expect |
|---|---|
| `http://www.msftconnecttest.com/connecttest.txt` | status 200, body `Microsoft Connect Test` |
| `http://connectivitycheck.gstatic.com/generate_204` | status 204, empty body |

Config shape: `targets.http_check` is an array of `{ "url": ..., "expect_status": <100–599>, "expect_body": "..." }` (`expect_body` optional, default empty).

Redirects are **not** followed; a redirect (3xx) or a status/body mismatch from at least one check means captive portal. A network error or timeout is a plain probe failure, not a captive portal. Body is read up to 4 KiB and compared after trimming whitespace.

### 4.4 Rates and timeouts

| Probe | `rates.<key>` (s) | default | `timeouts.<key>` (ms) | default |
|---|---|---|---|---|
| LINK-01 | `link` | 5 | — | — |
| LAN-01 | `lan_ping` | 1 | `icmp` | 1000 |
| LAN-02 | `lan_arp` | 5 | `arp` | 1000 |
| WIFI-01 | `wifi` | 2 | — | — |
| SYS-01 | `cpu` | 1 | — | — |
| NET-01 | `internet_icmp` | 1 | `icmp` | 1000 |
| NET-04 | `internet_tcp` | 1 | `tcp` | 2000 |
| DNS-01 | `dns_system` | 5 | `dns` | 2000 |
| DNS-02 | `dns_public` | 5 | `dns` | 2000 |
| RT-01 | `stun` | 5 | `stun` | 2000 |
| NAT-01 | `nat` | 60 | `stun` | 2000 |
| HTTP-01 | `http` | 15 | `http` | 5000 |
| SVC-01 | `services` | 30 | `tls` | 5000 |
| MTU-01 | `mtu` | 600 | `icmp` (per step) | 1000 |
| TEAMS-01 | `teams_udp` | 10 | `stun` | 2000 |
| TEAMS-02 | `teams_tcp` | 10 | `tcp` | 2000 |
| MEET-01 | `meet` | 10 | `stun` | 2000 |
| MEDIA-01 | `media` | 1 | `stun` | 2000 |
| SRC-01 | `src_ports` | 60 | `stun` | 2000 |
| TRC-01 | `trace.interval_ms` | 200 ms | `icmp` | 1000 |
| CUST-* | per target `rate` | 1 | `icmp` / `tcp` | — |

- A timeout longer than the interval only delays the next run of that series; missed ticks are dropped, never caught up in a burst.
- Timeouts: min 100 ms, max 10000 ms.

### 4.5 Validation rules

- Merge: objects merge key by key; an array in the user file replaces the default array as a whole (its entries never inherit fields from default entries). A UTF-8 BOM is accepted.
- All errors are reported at once, one line per key path. A check relative to another key (e.g. `window.min_samples` ≤ `window.seconds`, `mos_good` ≥ `mos_min`, CPU boundaries ascending, custom target cap) runs only when that key itself is valid.
- `targets.internet_icmp`, `targets.internet_tcp` and `targets.public_dns` must be **IPv4 literals** (non-empty lists): they open L4 and must not depend on DNS.
- Hosts are IPv4 or RFC 1123 hostnames; IPv6 literals are rejected with an explicit "IPv6 is not supported" message. A purely numeric last label (`1.1.1`) is invalid.
- Custom target `name`: 1–40 characters, no control characters, unique across `ping` and `tcp`. `rate` 1–3600.
- Ranges: `ui.port` 1–65535; `window.seconds` 10–3600; `history.minutes` 1–120; `gate.*` 1–100; `rates.*` 1–3600; `trace.max_hops` 1–64; `trace.interval_ms` 100–10000; `limits.max_custom_targets` 0–100; `thresholds.loss_pct` 0–100; `rtt_ms`/`jitter_ms` > 0; `mos_min` 1–4.5; `mtu_min` 576–1500; `csv.summary_seconds` 1–86400; `csv.max_mb` ≥ 1.
- `teams.udp_ports` must not be empty while `teams.relays` is set.

## 5. Tests (probes)

| ID | Test | Target | Method | Default interval |
|---|---|---|---|---|
| LINK-01 | adapter up, IP, APIPA | best-route adapter | `GetAdaptersAddresses` | 5 s + change notification |
| LAN-01 | gateway ping | gateway from `GetBestRoute2` toward `1.1.1.1` | `IcmpSendEcho2` | 1 s |
| LAN-02 | gateway MAC | gateway | `ResolveIpNetEntry2` | 5 s |
| WIFI-01 | SSID, signal %, RSSI, channel, band | Wi-Fi adapter | `WlanQueryInterface` (see note) | 2 s |
| SYS-01 | total CPU usage % | local machine | `GetSystemTimes` delta (no admin) | 1 s, always runs (no gate) |
| NET-01 | internet ping | `targets.internet_icmp` | `IcmpSendEcho2` | 1 s each |
| NET-04 | TCP connect time | `targets.internet_tcp` | TCP handshake | 1 s |
| DNS-01 | system resolver | adapter DNS servers | `DnsQuery_W` with `DNS_QUERY_BYPASS_CACHE` | 5 s |
| DNS-02 | public resolvers | `targets.public_dns` | UDP 53 direct (miekg/dns) | 5 s |
| RT-01 | UDP reachability, public IP | `targets.stun` | STUN binding | 5 s |
| NAT-01 | symmetric NAT | two STUN servers, same local socket | compare mapped ports | 60 s |
| HTTP-01 | HTTP + captive portal | `targets.http_check` | GET, no redirects | 15 s |
| SVC-01 | service DNS + TLS handshake time | `targets.services` | resolve + TLS 443 | 30 s |
| TRC-01 | mtr hop path | `targets.trace` | `IcmpSendEcho2` with TTL | one TTL every 200 ms, cycling (see note) |
| MTU-01 | path MTU | gateway, `targets.trace` | `IcmpSendEcho2` with DF, binary search 576–1500 | at start + every 10 min |
| TEAMS-01 | UDP per port in `teams.udp_ports` (default 3478) | `teams.relays` | STUN binding per port | 10 s |
| TEAMS-02 | TCP 443 fallback | `teams.relays` | TCP connect | 10 s |
| MEET-01 | Meet UDP | `meet.stun` | STUN binding | 10 s |
| MEDIA-01 | UDP media quality | one Teams relay + one Meet STUN | STUN at 1/s → RTT, jitter, loss | 1 s |
| SRC-01 | Teams source-port range | `teams.relays` | STUN from local UDP port in 50000–50059 | 60 s |
| CUST-* | custom ping/TCP | `targets.ping`, `targets.tcp` | ICMP / TCP | per target |

Notes:
- **RTT:** measured with Go's monotonic clock around each `IcmpSendEcho2` call. The API's `RoundTripTime` is whole milliseconds and is `0` for sub-millisecond replies (typical for the gateway), so it is not used as the value; it is only a sanity check (API value > 0 and Go value < API value − 1 ms → log a warning). The Go measurement includes Windows call and scheduling overhead (roughly up to 0.5 ms). At startup, measure a loopback baseline (10 pings to `127.0.0.1`, take the minimum) and show it as "timer overhead" in the console and web system info. Do not subtract it from RTTs; it only helps interpret sub-ms LAN values. Display RTT with one decimal place.
- **ICMP reply buffer:** allocate at least `sizeof(ICMP_ECHO_REPLY) + RequestSize + 8` bytes plus room for an ICMP error message (use `RequestSize + 8 + sizeof(ICMP_ECHO_REPLY) + 32`, minimum 256 bytes). A too-small buffer makes the API return a local buffer error (`ERROR_INSUFFICIENT_BUFFER` / `IP_BUF_TOO_SMALL`) instead of the real status, which breaks MTU-01. MTU-01 treats `IP_PACKET_TOO_BIG` and a DF timeout as "too big" and any local buffer error as a bug (logged, probe fails, never interpreted as an MTU result).
- **TRC-01 pacing:** send one probe every 200 ms, cycling TTL 1 → N, where N is the TTL at which the destination answered (initially `trace.max_hops`, then the discovered length + 1). A 15-hop path is therefore refreshed every ~3 s, and the total trace load never exceeds 5 packets/s. Each hop is its own series. Interval key: `trace.interval_ms` (default 200, minimum 100).
- **WIFI-01 and Windows location privacy:** on Windows 11 (24H2 and later), `WlanQueryInterface`/WLAN scan data may return `ERROR_ACCESS_DENIED` for SSID/BSSID when Location Services for desktop apps are off. This is not an error: show `Wi-Fi: SSID hidden (Windows location privacy)` and display whatever fields are still available (signal %, RSSI, band, channel). Never crash, never count it as a probe failure. If no Wi-Fi field is available at all, show `Wi-Fi: details unavailable` and skip `WIFI_WEAK`/`WIFI_24GHZ`.
- **Teams ports:** the public Teams relay (`worldaz.tr.teams.microsoft.com`) answers an anonymous STUN binding request only on UDP 3478; 3479–3481 do not answer (verified 2026-10-25 from three resolved relay IPs), so testing them by default would always report a false partial block. 3479–3481 can be added to `teams.udp_ports` for relays that answer there. The relay hostname resolves to changing IPs; resolve it at each run, show the IP used.
- **MEDIA-01 sources:** one series to the first `teams.relays` entry (UDP 3478) when Teams is enabled, and one to the first `meet.stun` entry. MOS/call quality uses the Teams series when it has ≥ `window.min_samples` samples, else the Meet series, else NET-01 to `1.1.1.1`.
- **ICMP defaults:** 32-byte payload, TTL 128 (except TRC-01), DF off (except MTU-01).
- **Series and rate limit:** a *series* is one probe ID to one endpoint (e.g. NET-01 → 1.1.1.1, NET-04 → 1.1.1.1:443, TRC-01 hop 3). The "1 probe/s" rule applies per series; different probe IDs to the same host are separate series.
- Never more than one probe in flight per series.
- Probe start times get a small random offset so probes don't fire in bursts.

## 6. Dependency gates

Layers, each requiring the one below:

| Layer | Opens when | Probes depending on it |
|---|---|---|
| L1 Link | best-route adapter is up | everything |
| L2 Address | adapter has a non-APIPA IPv4 | everything below |
| L3 Gateway | gateway ping OK **or** gateway ARP resolves | LAN-01/02 (always run at L2), `lan` custom targets, DNS-01 if resolver is on-link, TRC-01, MTU-01 to gateway |
| L4 Internet | **any** of: NET-01 ok, NET-04 ok, RT-01 ok | NET-*, DNS-02, RT-01, NAT-01, `internet` custom targets, MTU-01 to trace target, DNS-01 if resolver is remote |
| L5 DNS | DNS-01 ok | HTTP-01, SVC-01, hostname targets, TEAMS-*/MEET-*/MEDIA-01 when target is a hostname |

Rules:
- The probes that **define** a layer keep running while that layer is closed, so recovery is detected. Only the layers above pause.
- Hysteresis: a gate closes after `gate.fail_count` consecutive failures and reopens after `gate.ok_count` consecutive successes.
- **Start-up:** every gate starts `unknown`. From `unknown` it opens on the first success and closes after `gate.fail_count` consecutive failures (so a healthy network is green within ~1 s). Probes above an `unknown` gate wait (no samples) rather than recording skipped.
- **Multiple defining probes:** a layer fed by several series (DNS-01 per adapter DNS server, NET-01 per target) counts a success when **any** of them succeeded in the latest round and a failure when **all** failed. DNS-01 placement (L3 vs L4) is decided per server: on-link if inside the adapter's IPv4 subnet.
- Paused probes record **skipped** samples with the reason (`"skipped: gateway down"`). Skipped never counts as loss; graphs show gaps.
- Gateway ping failing while ARP resolves → info flag `GW_ICMP_BLOCKED`, gate stays open.
- Adapter change (Wi-Fi roam, cable, new IP, change of best-route adapter): re-evaluate from L1 upward, start new series for affected targets.
- TRC-01 runs whenever L3 is open; it is most useful when L4 is closed.

## 7. Measurements

- **Loss %:** failed / (ok + failed) in the rolling window. Skipped excluded.
- **Jitter:** RFC 3550 smoothed interarrival jitter over consecutive RTTs (`J += (|D| - J) / 16`). Labelled "jitter (RFC 3550)" in the UI.
- **Per-target stats:** Loss%, Sent, Last, Avg, Best, Worst, StDev, Jitter — same window in console, web and report.
- **MOS:** simplified E-model from the MEDIA-01 series (fallback: NET-01 to `1.1.1.1`): effective latency = avg RTT + 2·jitter + 10; R = 93.2 − (eff/40 if eff < 160 else (eff − 120)/10) − 2.5·loss%; MOS = 1 + 0.035R + 7·10⁻⁶·R(R − 60)(100 − R), clamped 1–4.5.
- **Hop loss:** loss at an intermediate hop that does not continue to the final hop is displayed dimmed and ignored by verdicts.

## 8. Verdicts

Root cause = lowest closed gate. Quality verdicts are evaluated only when all lower gates are open. One primary verdict at a time, plus any number of info flags.

| Verdict | Condition | Exit code |
|---|---|---|
| `OK` | all gates open, no quality verdict | 0 |
| `NO_LINK` | L1 closed | 10 |
| `APIPA` | L2 closed due to 169.254.x.x | 11 |
| `NO_ADDRESS` | L2 closed, no IPv4 at all | 12 |
| `GW_UNREACHABLE` | L3 closed | 13 |
| `CAPTIVE_PORTAL` | L4 open, HTTP-01 redirect/mismatch | 14 |
| `ISP_DOWN` | L4 closed | 15 |
| `DNS_LOCAL_FAIL` | L5 closed, DNS-02 ok | 16 |
| `DNS_FAIL` | L5 closed, DNS-02 fails | 17 |
| `UDP_BLOCKED` | all generic STUN fail, NET-04 ok | 18 |
| `TEAMS_UDP_BLOCKED` | all TEAMS-01 ports fail, TEAMS-02 ok | 19 |
| `TEAMS_PARTIAL_PORTS` | some (not all) ports in `teams.udp_ports` fail (report which; labels: 3478 general, 3479 audio, 3480 video, 3481 sharing) | 20 |
| `MEET_UDP_BLOCKED` | MEET-01 fails, while SVC-01 TLS to `meet.google.com` or NET-04 is ok | 21 |
| `MTU_ISSUE` | path MTU < `thresholds.mtu_min` (default 1400) | 22 |
| `LAN_LOSS` | LAN-01 loss > `thresholds.loss_pct` (default 2) | 23 |
| `ISP_LOSS` | LAN-01 loss ≤ threshold, loss on **every** NET-01 target > threshold | 24 |
| `QUALITY_POOR_VOIP` | on the call-quality source (§5 MEDIA-01 note): MOS < `thresholds.mos_min` (default 3.6) or jitter > `thresholds.jitter_ms` (default 30) or avg RTT > `thresholds.rtt_ms` (default 150) | 25 |

Priority when several apply: table order top to bottom.

- Loss and quality verdicts (`LAN_LOSS`, `ISP_LOSS`, `QUALITY_POOR_VOIP`, `UDP_BLOCKED`, Teams/Meet verdicts) need at least `window.min_samples` non-skipped samples in the window (STUN checks at 10 s: at least 3 results).
- While any of L1–L4 is still `unknown`, the primary verdict is `CHECKING` (shown in console/web, never an exit code by itself).
- **Call quality status** (console + web): `Measuring…` before `window.min_samples`; `Good` when MOS ≥ `thresholds.mos_good` and no quality threshold is exceeded; `Poor` when `QUALITY_POOR_VOIP` conditions hold; otherwise `Fair`.

Info flags (no exit code): `GW_ICMP_BLOCKED`, `ICMP_BLOCKED` (NET-01 fails, NET-04 ok), `SYMMETRIC_NAT`, `WIFI_WEAK` (signal < `thresholds.wifi_signal_pct`, default 50), `WIFI_24GHZ` (connected on the 2.4 GHz band), `CPU_HIGH` (10 s average CPU ≥ `thresholds.cpu_high_pct`; shown alongside network verdicts because a busy CPU also degrades calls), `TEAMS_SRC_PORTS_BLOCKED`, `TEAMS_DISABLED` (`teams.relays` empty), `SERVICE_UNREACHABLE:<host>` (SVC-01 DNS or TLS fails for that service while L5 is open), `CUSTOM_TARGET_DOWN:<name>`, `CSV_WRITE_ERROR`.

Exit codes 1 = runtime error, 2 = config error.

### 8.1 Headless verdict

The exit code is the highest-priority primary verdict (table order) that was in effect at any time during the run, ignoring `CHECKING`. If the run never left `CHECKING` (e.g. too short), exit 1. The report carries both `worst_verdict` and `final_verdict`.

## 9. Console view (`run`)

- **Console modes (via `sysapi.Console`):** save the original stdout **and** stdin modes at startup. Stdout: add `ENABLE_VIRTUAL_TERMINAL_PROCESSING`. Stdin: clear `ENABLE_LINE_INPUT`, `ENABLE_ECHO_INPUT` and `ENABLE_QUICK_EDIT_MODE` (keep `ENABLE_EXTENDED_FLAGS` set so the quick-edit change applies), **keep `ENABLE_PROCESSED_INPUT`** so Ctrl+C still raises a signal. Read keys with `ReadConsoleInputW` in its own goroutine (ignore non-key and key-up events), without waiting for Enter.
- **Always restore** both original modes on every exit path: `q`, Ctrl+C/console close (`SetConsoleCtrlHandler`/signal), fatal error, and panic (deferred restore in `main` plus a recover that restores before re-panicking). Also show the cursor again and move it below the drawn area, so the user's terminal is never left without echo or line editing.
- Redraw details: Redraw at 1 Hz with cursor-home + overwrite (no screen clear). Read console width every frame; truncate names to fit.
- **Header (always visible):** version, config source, `Web UI: http://127.0.0.1:<port>/`, adapter name, local IP, gateway IP + MAC, DNS servers, public IP, Wi-Fi SSID/signal/band, CPU load level (Light / Moderate / High / Heavy with %), primary verdict, info flags.
- **Call quality line:** source (MEDIA-01, or NET-01 to `1.1.1.1` when MEDIA-01 is unavailable), current RTT, jitter, loss, MOS, plus a sparkline.
- **Targets table:** Name, Host, Loss%, Snt, Last, Avg, Best, Wrst, StDev, Jitter, and (when `console.sparkline` is on and width allows) a **sparkline** of the last 60 s.
- **Sparkline:** one character per second, newest on the right, built from `▁▂▃▄▅▆▇█` scaled to that row's own range (min to max within the 60 s); a lost sample is `×` (in red when colour is on), a skipped sample is a space. If the row's max exceeds `thresholds.rtt_ms`, characters above the threshold are drawn in yellow. Column is dropped first when the console is too narrow. Not shown in plain (non-TTY) mode.
- Skipped rows greyed with reason.
- **Teams/Meet line:** per-port status (3478–3481, 443), Meet status, MOS.
- **Hops table:** mtr-style, all IPs seen per hop, reverse DNS if available, dimmed non-final loss.
- **Keys:** `q` quit, `r` reset counters (console and web share the reset). Key handling is active only when stdin is a console; otherwise only Ctrl+C.
- **Reset (`r`)** clears rolling-window stats, Sent counters and in-memory graph history. It does **not** reset whole-run report aggregates, gate/verdict history or CSV files; it writes a `reset` row to the events CSV.
- **Colour** is on when stdout is a console with VT processing enabled and the `NO_COLOR` environment variable is unset.
- **Text encoding:** console output is written with `WriteConsoleW` (UTF-16), so sparkline characters do not depend on the console code page; redirected output is UTF-8 without BOM.
- **Not a TTY (redirected):** no redraw; print timestamped verdict/flag changes and a summary every 30 s.
- **Exit (q / Ctrl+C):** cancel all probes, close listener, print a final summary.

## 10. Web UI

- Listen on `127.0.0.1:<ui.port>`; if busy, fall back to a free random port. The console always prints the actual URL.
- **Host check:** only `Host: 127.0.0.1:<port>` or `localhost:<port>` accepted; anything else → 403. No auth token.
- Read-only. No config editing.
- **Endpoints:**
  - `GET /` — page
  - `GET /assets/*` — embedded JS/CSS (uPlot vendored)
  - `GET /api/state` — current snapshot (sysinfo, gates, verdict, flags, per-target stats, hops)
  - `GET /api/history?target=<series id>` — ring-buffer history for graphs. Series IDs: `<probe>:<endpoint>`, e.g. `LAN-01:gateway`, `NET-01:1.1.1.1`, `NET-04:1.1.1.1:443`, `MEDIA-01:meet`, `SYS-01:cpu`, `TRC-01:hop:3`, `CUST:<name>`; `/api/state` lists all IDs. Columnar response for uPlot: `{"series": id, "t": [unix ms…], "value": [number|null…], "result": ["ok"|"fail"|"skipped"…]}` (`value` = RTT ms, CPU % for SYS-01; `null` when not ok). Unknown ID → 404.
  - `GET /api/events` — SSE, one `state` event per second (same JSON as `/api/state`), plus a `: ping` comment every 15 s
  - `GET /api/report` — report JSON as download (`Content-Disposition: attachment`)
- **Page (top to bottom):** verdict banner + flags; **Call quality** section; **System load** section; system info; gate chain (L1→L5) with open/closed/state; per-target detail graphs; targets table; Teams/Meet panel; hops table; "Export report" button; "connection lost" indicator with automatic SSE reconnect.
- **Call quality section** (the main view, modelled on call-app troubleshooting panels): one plain-language status (Good / Fair / Poor from MOS and thresholds) and one **connection delay** graph from MEDIA-01 (fallback NET-01 to `1.1.1.1`; the source is labelled). Below it a jitter graph and loss markers on the same time axis.
- **Per-target detail graphs:** latency per target (selectable), jitter, loss markers, skipped periods as gaps/shading.
- **Threshold lines:** every latency graph shows a dashed horizontal line at `thresholds.rtt_ms`; every jitter graph at `thresholds.jitter_ms`. Values above the line are visually distinguishable (colour of the series segment or shaded band).
- **Help texts:** every graph and section has a one-line plain-language subtitle (e.g. latency: "High values or large variations may reduce call quality"; CPU: "Very high values may reduce call quality") and a "?" button opening a short explanation (what is measured, how, what the threshold means, what to do). Texts live in one embedded file so they can be edited or translated without touching code. Language: English in v1.
- **System load section:** current level (Light / Moderate / High / Heavy) and a CPU graph with level bands at the configured boundaries, on the same time axis as the call-quality graph so a CPU spike can be matched to a latency spike.
- Time axis shows local clock time (HH:MM).
- Works fully offline; the page makes no requests to any host other than itself.
- Layout usable from 360 px width up; light and dark via `prefers-color-scheme`.

## 11. Outputs: JSON report (summary) + CSV (raw data)

Two complementary outputs. The JSON report is a **summary**; it does **not** contain every sample. All raw measurements go to CSV.

**Report JSON** contains: tool version, start/end time, config (effective), sysinfo, gate history (all transitions for the whole run), verdict and flag history (whole run), per-target stats for the current window **and** for the whole run (sent, ok, failed, skipped, loss %, avg, best, worst, stdev, jitter; whole-run stats kept as running aggregates, not from stored samples), hop table, Teams/Meet results, MTU results, and `csv_files`: list of CSV file names written during the run (empty when CSV is off). Same model used by `/api/report` and `--headless --json`. Raw samples are never included, so the report size does not grow with run length.

### 11.1 CSV logging

Enabled by `--csv <dir>` or `csv.dir`. Off by default. Directory created if missing. Files are appended, never overwritten.

Files (prefix = local start date, suffix `_N` when `csv.max_mb` is reached):

| File | One row per | Columns |
|---|---|---|
| `netdiag_<date>_samples.csv` | every sample, including skipped | `time; probe; target; host; ip; result; rtt_ms; value; ttl_hop; detail` (`value` = CPU % for SYS-01, empty otherwise) |
| `netdiag_<date>_summary.csv` | target every `csv.summary_seconds` | `time; target; window_s; sent; ok; fail; skipped; loss_pct; avg_ms; best_ms; worst_ms; stdev_ms; jitter_ms; mos` |
| `netdiag_<date>_events.csv` | gate/verdict/flag change, adapter change, start/stop | `time; type; name; old; new; detail` |

- `time`: ISO 8601 local time with offset and milliseconds (`2026-10-25T14:03:07.123+01:00`).
- `result`: `ok`, `fail`, `skipped`. `detail`: error class or skip reason (`skipped: gateway down`).
- `rtt_ms` empty when not ok. `ttl_hop` filled only for TRC-01 samples. `mos` empty for non-media targets.
- Header row in every file. Separator, decimal mark and BOM per config; values containing the separator are quoted.
- Buffered writer flushed every second and on exit, so a crash loses at most ~1 s.
- Daily rotation at local midnight (when `csv.rotate = daily`).
- If a CSV write fails (disk full, file locked by Excel): print a warning in the console header and web banner, keep running, retry opening the file every 30 s. Never stop probing because of CSV.
- Console header shows `CSV: <dir>` when active.

## 12. Non-functional

- Memory bounded by ring buffers; steady-state CPU negligible.
- Starts to first console frame in < 1 s.
- All goroutines stop within 2 s of quit.
- Unsigned exe may be flagged by antivirus/SmartScreen; code signing is a distribution concern outside this spec.
