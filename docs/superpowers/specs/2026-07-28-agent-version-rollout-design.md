# Agent Version Rollout and Version Visibility — Design

Date: 2026-07-28
Repos touched: `feast-watch` (mother, agent), `feast-mobile-backend-control` (panel, proxy)

## Purpose

The panel has no way to update an agent and no way to see what version anything
is running. The mother already carries a self-update mechanism, but the path
behind it does not work end to end, so an update button added on top of it today
would be a button that silently does nothing.

This design makes agent versions visible and rollout operator-driven, per server,
from the panel.

## Starting state

- `shared/version.Version` defaults to `"dev"` and **no build injects it**.
  Every agent reports `dev`, so the panel's version column carries no information
  and no agent can ever satisfy a rollout target.
- The mother never imports `shared/version`. Its own version is not exposed by
  any endpoint.
- `downloads/` holds no `.sha256` files, but `SelfUpdate` fetches
  `<version>-<platform>.sha256` before installing. Every update fails on a 404.
- `selfUpdate` keys the download on `runtime.GOARCH` only. A Windows agent
  fetching `v1.3.0-amd64` would overwrite itself with the Linux binary.
- `settings.desired_version` is fleet-wide and unvalidated. Setting it to
  `latest` — the only name staged in `downloads/`, and the name the install
  script uses — makes every agent download and restart on every push, forever,
  because an agent can never report being on `"latest"`.
- A failed update is recorded nowhere. The mother learns nothing; the reason
  stays in the agent host's journal.

## Non-Goals

- **No mother self-update.** The mother serves the panel; a button that makes it
  replace itself and exit takes the panel down with it, and there is no rollback
  path. Its version is exposed for visibility only; deployment stays with
  systemd/Docker/k8s.
- **No compatibility interpretation.** The panel shows the mother version and
  each agent version side by side. It does not compute "compatible" or
  "outdated", and no protocol-version negotiation is introduced.
- **No fleet-wide update action.** Rollout is per server by construction.
- **No CI pipeline.** Releases are built by a script in the repo.

## Decisions

| Decision | Choice | Alternatives considered |
| --- | --- | --- |
| Rollout granularity | Per server | Fleet-wide; fleet-wide with per-server override |
| Update target | Operator picks from the builds staged on the mother | Always the mother's own version |
| Mother updates | Visibility only | Confirmed self-update button |
| Compatibility | Display only | Protocol version + minimum supported agent |
| Release staging | `bin/release.sh` in the repo | CI on tag push; keep staging manual |

## Release — `bin/release.sh`

Reads the version from `git describe --tags` (or argv), injects it into **both**
binaries via `-ldflags -X .../shared/version.Version=$VERSION`, and for each
platform writes into the downloads directory:

```
feast-watch-agent-<version>-<goos>-<goarch>        + .sha256   canonical
feast-watch-agent-latest-<goos>-<goarch>           + .sha256   install script target
feast-watch-agent-<version>-<goarch>               + .sha256   legacy alias, linux only
```

No `.exe` suffix on Windows: the agent replaces its own executable at whatever
path it already runs from, and both the download URL and the mother's build
listing key on the bare name.

The legacy alias is the migration path. Agents installed before builds carried a
GOOS request `<version>-<goarch>`; without the alias they cannot be updated
without touching each host. It is droppable once no agent reports a version
older than the first platform-explicit build.

`PLATFORMS` in the script must stay in sync with `knownPlatforms` in
`mother/api/versions.go`, which decides what the panel is allowed to offer.

## Protocol

Two fields on `IngestRequest`:

- **`arch`** (`runtime.GOARCH`), sent with the identity fields on the first push.
  Without it the mother cannot tell which binary a host can run, so it cannot
  reject an impossible target before the agent tries it.
- **`update_error`**, sent on **every** push, empty when the last attempt
  succeeded or none was made. This is what makes a failed rollout visible in the
  panel instead of only in the host's journal. Because it rides every push, it
  is stored verbatim and a recovered agent clears it with no operator action.

`IngestResponse.desired_version` is unchanged on the wire; it is now sourced from
the server row rather than global settings.

## Mother — storage

Three columns on `servers`, added through the existing `migrate()` pattern:
`arch`, `desired_version`, `update_error`.

`store.Heartbeat` replaces `TouchServer`'s positional identity arguments. The two
kinds of field behave differently and the distinction is easy to get wrong:
identity fields ride the first push only, so an empty value means "not reported
now" and must not erase what is known; `update_error` rides every push, so an
empty value legitimately clears a previous failure.

`SetDesiredVersion(id, version)` also clears `update_error`, so a retry from the
panel starts clean rather than showing the previous attempt's failure while the
new one is in flight.

**`settings.desired_version` is removed.** A migration copies a non-empty global
value onto every server row and deletes the key, so a rollout an operator had
already started is not silently cancelled; deleting the key makes it idempotent.

## Mother — API

| Endpoint | Shape |
| --- | --- |
| `GET /api/version` | `{mother_version, agents:[{version, platforms[]}]}` |
| `PUT /api/servers/{id}/version` | body `{version}` → `{desired_version}`; `""` cancels |
| `GET /api/servers` | gains `arch`, `desired_version`, `update_state`, `update_error` |
| `PUT /api/settings` | loses `desired_version` (five fields remain) |

`GET /api/version` lists builds by scanning the downloads directory. A build is
offered only when **both** the binary and its `.sha256` are present — the agent
refuses to install without a verified checksum, so a half-staged build would
produce a button that can only fail. `latest-*` is never offered. Versions sort
newest-first using a digit-aware comparison, so `v1.10.0` precedes `v1.9.0`.

`PUT /api/servers/{id}/version` rejects, with the reason in the error envelope:

- `latest` and `latest-*` — a moving alias, not an identity an agent can reach
- a version not staged on the mother
- a version staged, but with no build for this server's `os-arch`

When the agent has not reported a platform, the platform check is skipped rather
than failing: requiring one would lock pre-`arch` agents out of the very update
that teaches them to send it.

`update_state` is a projection the panel renders instead of re-deriving:
`idle` when no target is set or the agent has reached it, `failed` when
`update_error` is set, `pending` otherwise.

## Agent

- The download is keyed on `<version>-<goos>-<goarch>`.
- `Loop.Run`'s callback returns an `error`. The loop stores the failure and
  reports it on the next push.
- `tryUpdate` throttles retries to one per `updateRetryGap` (5 minutes) per
  target, resetting when the mother names a different version — so an operator
  correcting a bad target is acted on at the next push, not after the previous
  backoff expires. Without the gap an unreachable target is re-downloaded on
  every push: a whole binary every 10 seconds, indefinitely.

## Panel

- Mother version in the page header, as a plain value.
- Server list gains `Agent sürümü`, `Hedef`, and an update-state badge:
  `idle` neutral, `pending` info, `failed` danger with `update_error` shown.
- Per-row `[Güncelle]` opens a dialog whose version list is
  `GET /api/version` filtered to the row's `os-arch`; choosing one issues the
  `PUT`. The current version is not selectable. Mother 400s render on the dialog.
- Settings dialog drops the `desired_version` field.
- Backend proxy adds `GET /admin/monitoring/version` and
  `PUT /admin/monitoring/servers/{id}/version` under the existing
  `system:health` permission. This supersedes the "no new backend endpoints"
  non-goal in the 2026-07-27 panel design.

## Testing

Go, alongside the existing per-package tests:

- store — per-server targeting and isolation, `ErrNotFound`, error clearing on
  retry and on recovery, identity fields surviving a steady-state push, the
  global-to-per-server migration and its idempotence.
- api — mother version reported, newest-first ordering, builds hidden when the
  checksum or the alias rule says so, target reaching the agent through ingest,
  each rejection case, unknown-platform allowance, cancel, 404, auth.
- agent — `arch` on the first push, failure reported on the next push, recovery
  clearing it, retry throttling, reset on a new target, and that a build without
  this agent's GOOS does not satisfy an update.

Panel tests follow the existing Vitest layout.

## Verification

`bin/release.sh` staging a real release, then against a running mother: reject an
unstaged version, reject `latest`, accept a valid one, confirm the next ingest
response carries it, and confirm the server row reports `failed` with the reason
when the agent reports an update error.
