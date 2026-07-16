# feast-watch — Technical Design

**Date:** 2026-07-16
**Status:** Approved design, pending implementation plan

## Purpose

feast-watch is an internal server-monitoring system. Lightweight agents run on every
server and push metrics to a central collector ("mother"). The mother stores rolled-up
metrics and serves summaries to the existing feast admin panel **through the feast
backend only** — feast-watch is never exposed to the public internet, and the frontend
never talks to the mother directly.

## Non-Goals

- No standalone UI in this repo. Charts and screens live in the existing admin panel.
- No disk I/O read/write metrics, no temperature sensors, no raw net rx/tx metrics.
- No pull-based scraping (Prometheus-style). Everything is push.
- Raw logs/metrics are never shipped to the frontend — only rollup summaries.

## Architecture Overview

```
┌────────────┐  HTTPS push (10s)   ┌────────────┐   API key    ┌───────────────┐
│  agent(s)  │ ──────────────────► │   mother   │ ◄──────────  │ feast backend │
│ (each srv) │ ◄────────────────── │ Go+SQLite  │              └──────┬────────┘
└────────────┘  config in response └────────────┘                     │
                                     ▲    also runs an agent          ▼
                                     └── monitors its own host   admin panel
```

- **Push model:** each agent POSTs metrics to `POST /v1/ingest` every poll interval
  (default 10s). The mother's response carries the agent's current config: enabled
  collectors, poll interval, and desired agent version. Agents open **no inbound
  ports** (firewall-friendly; chosen over a separate control channel or a second
  config-polling endpoint).
- **Isolation:** mother lives on the internal network. The feast backend calls the
  mother's `/api/*` endpoints with an API key; the admin panel gets everything via
  the backend.
- The mother's own host is monitored by a regular agent installed alongside it.

## Repository Layout

```
feast-watch/
├── agent/          # Go — per-server agent
├── mother/         # Go — collector, API, SQLite storage
├── shared/         # shared Go packages: metric types, protocol, version
├── deploy/         # install.sh template, systemd unit, k8s DaemonSet manifest
├── docker-compose.yml   # local development only
├── README.md
└── QUICKSTART.md
```

Both components are Go so protocol/metric types are defined once in `shared/`.

## Agent

Single static binary (no cgo), reads `/proc` via gopsutil.

**Resource budget (acceptance criteria, verified by load test):** < 1% CPU, < 30 MB RSS.
Monitoring must never burden the monitored server.

### Collectors

Only the collectors listed in the mother's response run; everything else stays off.
Every metric on this list answers "which failure would we miss without it" — curated
from the meeting notes plus the essential parts of the earlier (never implemented)
backend monitoring design (`feast-mobile-backend/docs/superpowers/specs/2026-07-03-monitoring-design.md`).

**Base set — enabled on every server:**

| Collector | Data                                                                  | Failure it catches            |
| --------- | --------------------------------------------------------------------- | ----------------------------- |
| `cpu`     | CPU usage %                                                           | Server saturated              |
| `memory`  | RAM used/total **and** swap used/total together (headroom visibility) | Spill into swap = RAM exhausted |
| `uptime`  | System uptime                                                         | Unexpected restarts           |
| `disk`    | Disk usage % (space only — no I/O rates)                              | Disk full stops everything    |

**Per-server extras — the "max connections vs. used" family, enabled only where the
service runs:**

| Collector    | Data                                                            | Source                                   |
| ------------ | ---------------------------------------------------------------- | ---------------------------------------- |
| `centrifugo` | Total client connections vs. configured maximum                  | Centrifugo server API `info` (localhost) |
| `dragonfly`  | `used_memory` vs. `maxmemory` (it stops when RAM runs out) + `connected_clients` | `INFO memory` / `INFO clients` (localhost) |
| `postgres`   | Active connections (`pg_stat_activity`) vs. `max_connections`    | Catalog queries; Supabase is externally hosted, so this collector is enabled on the backend server's agent and queries remotely |
| `k8s`        | Node ready status, pod phase counts, container restart spikes    | kubelet/API server (enabled on k8s nodes/masters) |

**Deliberately excluded:** disk I/O read/write rates, temperature sensors, raw
net rx/tx, DB size (Supabase dashboard covers it), Centrifugo per-node breakdown
(total suffices), and HTTP 5xx tracking — 5xx capture lives in the backend request
path and stays a backend concern, outside feast-watch's scope.

### Registration & Identity

Install writes `TOKEN`, `MOTHER_URL`, `SERVER_NAME` to `/etc/feast-watch/agent.conf`.
On first push the agent reports hostname, IP, OS, and its own version. The panel shows
each agent's version.

### Self-Update

If `desired_version` in the mother's response differs from the running version, the
agent downloads the new binary from the mother (SHA-256 checksum verified), replaces
itself, and restarts via systemd. This gives one-click "update mother → force-update
all agents". On Kubernetes, updates go through the DaemonSet image tag instead.

## Add Server Flow (no Docker)

1. Admin panel → **Add Server** → name entered → backend forwards to mother →
   mother generates a per-server token.
2. Panel shows a one-liner:
   `curl -sSL https://<mother-ip>:8443/install/<token>.sh | sudo bash`
   (`-k` flag included in the generated command when the internal CA is not yet
   trusted by the target host)
   The script downloads the right-arch binary, writes the config (mother IP embedded),
   installs and starts the systemd service.
3. Kubernetes alternative: `deploy/k8s/daemonset.yaml` (hostPID + `/proc` mount),
   token supplied as a Secret.
4. On the agent's first push the server flips from *pending* to *online* in the panel.

## Mother

Go binary with embedded SQLite. Single service, single data file.

### Endpoints

| Endpoint                        | Purpose                                                        |
| ------------------------------- | -------------------------------------------------------------- |
| `POST /v1/ingest`               | Agent push (Bearer token). Response: `{collectors, interval, desired_version}` |
| `GET /install/<token>.sh`       | Install script generation                                       |
| `GET /download/agent/<version>` | Agent binary distribution (with checksums)                      |
| `GET/POST /api/*`               | Backend-facing API (API key): server list + status + versions, chart summaries, settings, history deletion |

### Behavior

- **Down detection:** `now - last_push > heartbeat_miss_threshold × interval` → server
  marked DOWN.
- **Configurable from the panel:** poll interval, heartbeat miss threshold, retention
  days, per-server collector selection, desired agent version.
- **History deletion:** by server and/or by date range.

## Data: Rollup & Retention

- **Per-server rollups** (never a global average). Each rollup row is
  (server, metric, window) → min/max/avg.
- Tiers: raw (10s) → 1-minute → 1-hour rollup tables.
- Default retention (all configurable): raw 48 h, 1-minute 15 days, 1-hour **75 days**.
- **The chart API reads only from rollup tables.** The panel passes the desired
  interval (5m/1h/1d/…) as a parameter; the mother returns the summarized series.
  Raw data never reaches the frontend.

## Security

- Unique per-server bearer token; revoked when the server is deleted (pushes rejected).
- API key between backend and mother; TLS via internal CA or self-signed certs.
- Rate limiting and payload schema validation on ingest.
- No hardcoded secrets; config via files/env, validated at startup.

## Testing

- **Unit:** collectors (mocked `/proc`), rollup/retention logic.
- **Integration:** ingest → rollup → chart API flow.
- **E2E:** docker-compose scenario with mother + 2 fake agents.
- Target: 80%+ coverage.

## Decisions Log

| Decision                | Choice                                   | Alternatives considered            |
| ----------------------- | ---------------------------------------- | ---------------------------------- |
| UI                      | Existing admin panel via feast backend   | Own UI in repo; hybrid             |
| Mother language         | Go (shared types with agent)             | Python/FastAPI, Node/TS            |
| Storage                 | SQLite embedded in mother                | PostgreSQL, VictoriaMetrics        |
| Config/update transport | Piggybacked on push response             | Separate control channel; separate config polling |
| Connection metric       | Per-service: Centrifugo clients, Dragonfly clients, Postgres connections — each vs. its max | OS FD limits; raw TCP counts       |
| 5xx / error tracking    | Out of scope (backend request-path concern) | Ingesting 5xx into feast-watch  |
| Agent install           | curl one-liner → systemd (DaemonSet on k8s) | Docker run command              |
