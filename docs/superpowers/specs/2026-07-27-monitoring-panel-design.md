# feast-watch Admin Panel Integration — Design

Date: 2026-07-27
Repos touched: `feast-mobile-backend-control` (panel), `feast-watch` (mother)

## Purpose

feast-watch's mother exposes a complete admin API, and the feast backend already
proxies it at `/admin/monitoring/*` (branch `feat/admin-monitoring-proxy`). Nothing
consumes it. This design covers the panel UI that closes the loop, plus the one
mother-side gap that blocks a real agent install.

## Non-Goals

- No direct panel→mother calls. Every request goes through the backend proxy.
- No new backend endpoints. The panel consumes the existing eight verbatim.
- No `CA_FILE` propagation through the install script — that is a path on the
  agent host and cannot be transferred from the mother.
- No alerting, no notifications, no dashboards beyond the per-server charts.

## Contract

The panel consumes the backend proxy, which forwards mother's payloads verbatim.

| Endpoint | Shape |
| --- | --- |
| `GET /admin/monitoring/servers` | `[{id, name, status, collectors[], hostname, ip, os, agent_version, last_push}]` |
| `POST /admin/monitoring/servers` | body `{name}` → `{server:{id,name,token,collectors}, install_command}` |
| `DELETE /admin/monitoring/servers/{id}` | → `null` |
| `PUT /admin/monitoring/servers/{id}/collectors` | body `{collectors[]}` → `null`; empty list → 400 |
| `GET /admin/monitoring/settings` | `{interval, heartbeat_miss_threshold, retention_raw_hours, retention_1m_days, retention_1h_days, desired_version}` |
| `PUT /admin/monitoring/settings` | same shape; `interval >= 2`, `heartbeat_miss_threshold >= 1` |
| `GET /admin/monitoring/chart` | query `server_id, metric, from, to, interval` → `[{ts,min,max,avg}]` |
| `DELETE /admin/monitoring/history` | query `server_id, from, to` → `null`; `server_id` required, `0` = all |

`status` is one of `pending` (never pushed), `online`, `down`.

Proxy-specific statuses: `503` when `MONITORING_API_URL`/`MONITORING_API_KEY` are
unset, `502` when mother is unreachable. Mother's own 400/409 pass through with
their message intact.

Chart constraints enforced by mother: `interval` is floored to 60 s, and
`(to - from) / interval` must not exceed 500 points.

## Architecture

Panel files follow the existing `src/api/<domain>.js` + `src/features/<domain>/`
convention. No file exceeds ~300 lines.

```
src/api/monitoring.js                            eight endpoint wrappers
src/lib/monitoringMetrics.js                     metric catalog + range presets (pure)
src/features/monitoring/MonitoringPage.jsx       server list
src/features/monitoring/ServerDetailPage.jsx     detail + chart grid
src/features/monitoring/MetricChart.jsx          one metric chart (Recharts)
src/features/monitoring/AddServerDialog.jsx      add + install command
src/features/monitoring/CollectorsDialog.jsx     collector selection
src/features/monitoring/SettingsDialog.jsx       global settings
src/features/monitoring/DeleteHistoryDialog.jsx  history deletion
```

Plus two routes in `App.jsx` and one entry in `navItems.js`.

New dependency: `recharts`. The panel has no charting library, and hand-rolling
axes, tooltips and responsive sizing would cost more to write and test than it
saves in bundle size for an internal admin tool.

### Permission

Both routes sit under `RequirePermission perm="system:health"`, matching the
backend, which mounts every `/admin/monitoring/*` route under
`RequirePermission(role.PermSystemHealth)`. No new permission is introduced.

## Pure logic — `lib/monitoringMetrics.js`

Two data tables and the functions over them. Everything here is pure, so the
range arithmetic and metric derivation are unit-testable without rendering.

**Metric catalog** — `key → {label, unit, collector, format}`:

| Collector | Metrics |
| --- | --- |
| `cpu` | `cpu.usage` (%) |
| `memory` | `mem.used_pct`, `mem.swap_used_pct` (%) |
| `disk` | `disk.used_pct` (%) |
| `uptime` | `uptime_s` (duration) |
| `centrifugo` | `centrifugo.conns`, `centrifugo.conns_max` (count) |
| `dragonfly` | `dragonfly.mem_used`, `dragonfly.mem_max` (bytes), `dragonfly.clients` (count) |
| `postgres` | `postgres.conns`, `postgres.conns_max` (count) |
| `k8s` | `k8s.nodes_ready`, `k8s.nodes_total`, `k8s.pods_running`, `k8s.pods_failed`, `k8s.restarts` (count) |

`metricsForCollectors(collectors)` derives which charts a server gets from its
own `collectors[]` — a server with only `cpu, memory` never renders a disk chart.

Capacity pairs (`centrifugo.conns`/`conns_max`, `dragonfly.mem_used`/`mem_max`,
`postgres.conns`/`conns_max`) render as a single chart: the usage metric as the
series, its maximum as a reference line. This matches the original feast-watch
design, whose connection metric is defined as "each vs. its max", and avoids a
chart whose only content is a flat capacity line.

**Range presets** — chosen so mother's `interval >= 60` and `<= 500 points`
constraints hold by construction rather than by hitting a 400 at runtime:

| Preset | Span | interval | Points | Mother tier |
| --- | --- | --- | --- | --- |
| 1 saat | 3600 s | 60 s | 60 | `rollup_1m` |
| 6 saat | 21600 s | 60 s | 360 | `rollup_1m` |
| 24 saat | 86400 s | 300 s | 288 | `rollup_1m` |
| 7 gün | 604800 s | 3600 s | 168 | `rollup_1h` |
| 30 gün | 2592000 s | 21600 s | 120 | `rollup_1h` |

The 30-day preset deliberately uses an interval `>= 3600` so mother reads
`rollup_1h`, whose 75-day default retention covers the range; `rollup_1m` only
retains 15 days.

`rangeWindow(rangeKey, nowSeconds)` returns `{from, to, interval}`.

## Server list — `/monitoring`

Table columns: name, status badge, hostname, IP, OS, agent version, last push
(relative, e.g. "2 dk önce"). Status maps to badge variants:
`online → success`, `down → danger`, `pending → warning`. Rows link to the
detail route. `refetchInterval: 10_000` matches the agent's default push cadence.

Header actions: `[Ayarlar]` and `[+ Sunucu Ekle]`.

**Add flow:** enter a name → `POST` → the dialog switches to a result view showing
the returned `install_command` and token in a copyable code block. The dialog does
not auto-close: the token is never shown again by any endpoint, so the operator
must dismiss it deliberately.

## Server detail — `/monitoring/:id`

Header shows the server's identity and three actions: `[Collector'lar]`,
`[Geçmişi Temizle]`, `[Sil]`. Below, a single range selector and a chart grid
(2 columns, 1 on mobile) derived from the server's active collectors.

Each chart renders a min–max band plus an average line over a shared time axis,
with a hover tooltip. One `useQuery` per chart, keyed
`['monitoring','chart',id,metric,rangeKey]`. The `from`/`to` window is computed
inside the query function rather than baked into the key, so a refetch advances
the window instead of permanently caching a stale one. Auto-refresh runs at 60 s
for the 1-hour and 6-hour presets and is off for longer ranges, where a
sub-minute refresh buys nothing.

Chart colors bind to the panel's existing theme tokens; no hardcoded hex values.

**Collectors dialog:** checkbox list of all known collectors, current selection
pre-checked. At least one must remain selected — mother rejects an empty list
with 400, so the save button disables rather than round-tripping to an error.

**History deletion:** a date range scoped to this server's id, confirmed through
the panel's existing `ConfirmDialog` with the `danger` variant. The all-servers
form (`server_id=0`) is deliberately not exposed in the UI — it is a destructive
operation with no undo and belongs on the CLI.

## Settings dialog

Six fields. Client-side validation mirrors mother's rules (`interval >= 2`,
`heartbeat_miss_threshold >= 1`) to give immediate feedback, but mother remains
the authority: a 400 response renders its message on the form. Saving invalidates
both the settings query and the server list, since `status` is derived from
`interval` and `heartbeat_miss_threshold`.

## Error handling

| Condition | Panel behavior |
| --- | --- |
| 503 `monitoring is not configured` | Empty-state card naming the missing `MONITORING_API_URL` / `MONITORING_API_KEY` |
| 502 `monitoring backend unreachable` | Error card with a retry action |
| Mother 400/409 (e.g. duplicate name) | Message rendered on the originating form via `apiErrorMessage` |
| Empty server list | `EmptyState` prompting the first add |
| Chart with no points | Per-chart "veri yok" placeholder, not a page-level error |

## Testing

Vitest + Testing Library, matching the panel's existing test layout.

- `monitoringMetrics.test.js` — range arithmetic, the ≤500-point and ≥60 s
  invariants across every preset, metric derivation from collectors, formatters.
- `monitoring.test.js` — request shape per endpoint (method, path, query, body).
- `MonitoringPage.test.jsx` — list rendering, status badges, empty state, 503 state.
- `AddServerDialog.test.jsx` — install command displayed and copyable after add.
- `SettingsDialog.test.jsx` — validation blocks submit; mother's 400 surfaces.
- `MetricChart.test.jsx` — empty, loading and populated states.

## Mother-side change: install script TLS propagation

`install.sh.tmpl` writes only `MOTHER_URL`, `TOKEN` and `SERVER_NAME` into
`agent.conf`, and `install.go` hardcodes the `https://` scheme. Two consequences:
a mother served over plain HTTP renders an install script the agent cannot use,
and a mother with a self-signed certificate produces an agent that fails TLS
verification with no way to opt out. The install path only works today against a
publicly-trusted certificate.

Change:

- `mother/cmd/feast-watch/main.go` — derive the scheme from `FW_TLS_CERT`
  (`https` when set, `http` otherwise); read a new `FW_AGENT_TLS_SKIP_VERIFY`.
- `mother/api/install.go` — carry scheme and the agent TLS flag on `API`; render
  `MOTHER_URL` from the scheme instead of a hardcoded literal.
- `mother/api/admin.go` — `InstallCommand` takes the scheme, so the panel's
  displayed one-liner and the `feast-watch generate` CLI agree with what the
  mother actually serves.
- `mother/generate.go` — pass the scheme through from the CLI entry point.
- `mother/api/install.sh.tmpl` — conditionally emit `TLS_SKIP_VERIFY=true` into
  the generated `agent.conf`.

Tests cover both schemes and the presence/absence of the skip-verify line.

## LAN verification

Production targets are Linux servers, so the test exercises the real
`curl | sudo bash` + systemd path rather than a native Windows binary. The agent
host is WSL2 on the second machine, reached over wifi.

| Component | Host | Configuration |
| --- | --- | --- |
| certificate | Mac | self-signed, `subjectAltName=IP:<mac-ip>` |
| agent binary | Mac | `GOOS=linux GOARCH=amd64` build staged as `downloads/feast-watch-agent-latest-amd64` |
| mother | Mac | `FW_TLS_CERT`/`FW_TLS_KEY`, `FW_PUBLIC_ADDR=<mac-ip>:8443`, `FW_AGENT_TLS_SKIP_VERIFY=true` |
| backend | Mac, `fmb-monitoring-proxy` worktree | `MONITORING_API_URL=https://127.0.0.1:8443`, `MONITORING_TLS_SKIP_VERIFY=true`, `MONITORING_API_KEY` |
| panel | Mac | `npm run dev -- --host`, reached at `http://<mac-ip>:5173` |
| agent | Windows/WSL2 | systemd enabled in `/etc/wsl.conf`, then the panel's one-liner under `sudo` |

The panel's Vite dev proxy runs on the Mac, so `/api` stays same-origin and no
CORS configuration is required.

Success criterion: add a server in the panel, run the generated one-liner in
WSL2, and watch the row move from `pending` to `online` with charts filling in.

The backend proxy branch stays unmerged during verification and is merged only
after the flow is confirmed end to end.

## Decisions Log

| Decision | Choice | Alternatives considered |
| --- | --- | --- |
| Chart rendering | Recharts | Hand-rolled SVG; uPlot |
| Page structure | List route + detail route | Single page with tabs; expanding rows |
| Range presets | Fixed five, constraint-safe by construction | Free-form date picker with client-side clamping |
| History deletion scope | Per-server only in the UI | Expose `server_id=0` all-servers form |
| Permission | Reuse `system:health` | New `monitoring:*` permission |
| Agent test target | WSL2 on the second machine | Native Windows binary; Docker container; remote Linux server |
| Agent TLS trust over the wire | `FW_AGENT_TLS_SKIP_VERIFY` → `TLS_SKIP_VERIFY` | Ship a CA file reference; require a publicly-trusted cert |
