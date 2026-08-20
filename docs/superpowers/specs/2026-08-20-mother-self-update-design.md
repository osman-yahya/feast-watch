# Mother Self-Update — Rolling the Control Plane From the Panel

Date: 2026-08-20
Repos touched: `feast-watch` (mother, shared, deploy, release), `feast-mobile-backend` (proxy), `feast-mobile-backend-control` (panel)

Follows the agent rollout design (`2026-07-28-agent-version-rollout-design.md`) and the proxy contract
(`2026-08-18-backend-proxy-contract.md`). Written after a 25-agent root-cause pass on "we cut a version
and it never reached the panel", whose findings are folded into the release section below.

## Purpose

An operator can retarget any agent in the fleet from the panel, but not the mother. Upgrading the mother
means a shell on its host: `bin/release.sh --mother-only`, `deploy/mother-install.sh`, restart. The one
machine that runs the monitoring is the one machine the monitoring cannot manage.

This design gives the mother the same panel-driven rollout the agents have — pick a published version,
the mother converges on it — while respecting the two things that make the mother different: it is a
hardened, unprivileged systemd service that cannot write its own binary, and it is offline while it
updates, so it cannot report on itself the way an agent does.

## What exists today

- Release assets are agent-only. `shared/release.AssetName` builds `feast-watch-agent-<goos>-<goarch>`;
  `.github/workflows/release.yml` builds exactly that matrix. **No mother binary is published anywhere.**
- `mother/api/versions.go` reports `mother_version` from `shared/version.Version` — a compile-time
  constant with no writer. `PUT /api/servers/{id}/version` targets agents only.
- `deploy/feast-watch-mother.service` runs the mother as `User=feast-watch` with `ProtectSystem=strict`
  and `NoNewPrivileges=true`. `deploy/mother-install.sh:180` installs the binary as root, 0755.
- `agent/update.go` holds the working self-update: fetch checksum, stream the binary to a temp file beside
  the target while hashing, compare, `os.Rename`, `exit(0)`; `deploy/feast-watch-agent.service` has no
  `User=`, so the agent is root and can overwrite itself in place.
- `mother/store/settings.go:GetSettings` runs `strconv.Atoi` over **every** stored settings value.

## Executive summary

Six pieces, in dependency order:

1. **Publish the mother.** `shared/release` learns a second asset family; the release workflow and
   `bin/release.sh` build `feast-watch-mother-<goos>-<goarch>` for linux/amd64 and linux/arm64.
2. **Index it.** `mother/release` splits what it finds by asset family, so `GET /api/version` can offer
   mother builds without an agent ever seeing them as a target.
3. **Store the intent.** A new single-row `mother_update` table — *not* `settings`, which cannot hold a
   non-numeric value without breaking every settings read.
4. **Converge.** A small in-process loop reads that row, downloads and verifies the binary, stages it
   inside the state directory it is allowed to write, and asks the process to shut down.
5. **Promote.** `ExecStartPre=+/usr/local/sbin/feast-watch-mother-promote` runs as root on the next start
   and moves the staged binary into `/usr/local/bin/feast-watch` before `ExecStart` runs it.
6. **Surface it.** One new proxied endpoint and a Mother card on the panel's monitoring page.

The download half is lifted out of `agent/update.go` into `shared/selfupdate` so "the mother updates the
way the agent does" is shared code rather than a claim in a comment.

## Decisions

### The binary comes from GitHub Releases, like the agent's **(owner sign-off required)**

**Choice.** Publish `feast-watch-mother-linux-amd64` and `feast-watch-mother-linux-arm64`, each with its
`.sha256`, from the same workflow run that publishes the agents. `shared/release` gains
`MotherAssetName`, `MotherPlatforms`, and an `AssetKindOf(name) (kind, platform, ok)` that replaces the
current `PlatformOf` at its two call sites.

**Why.** It keeps the existing invariant that the mother stores and serves no binaries — a rollout cannot
be blocked by a file somebody forgot to stage, and the mother's disk is not part of the distribution
path. It also means one tag names one set of bytes for the whole system, mother and agents together.
`ExpectedAssets()` (added while fixing the release pipeline) extends by construction, so a release
missing the mother build fails the workflow's `assert` job instead of appearing complete.

**Why only linux.** The mother is a systemd service on a Linux host; `deploy/`, the unit file, and the
promote helper all assume it. Publishing a darwin build would offer a rollout target that no supported
deployment could apply. `MotherPlatforms` is a separate list from `Platforms` precisely so this stays
explicit rather than accidental.

**Rejected.** Reusing `Platforms` for both families — it would advertise windows/darwin mother builds that
nothing can install. Serving the binary from the mother itself — it re-introduces the staging problem the
GitHub Releases move removed, and a mother cannot serve the bytes that replace it while it is exiting.

### The running binary is replaced by root at the next start, not by the mother **(owner sign-off required)**

**Choice.** The mother downloads and verifies, then writes the new binary to
`/var/lib/feast-watch/update/feast-watch.new` and asks the process to shut down. The unit gains
`ExecStartPre=+/usr/local/sbin/feast-watch-mother-promote`; the `+` prefix runs it as root, outside the
unit's sandbox. The helper installs the staged file over `/usr/local/bin/feast-watch`, keeping the
previous one as `/usr/local/bin/feast-watch.bak`, and removes the staged file. Then `ExecStart` runs the
new binary.

**Why.** The mother cannot replace its own binary, and not for one reason but three, each sufficient on
its own: `ProtectSystem=strict` mounts the whole hierarchy read-only inside the unit's namespace except
its `StateDirectory`, so any write under `/usr/local/bin` is `EROFS` regardless of uid; `User=feast-watch`
is unprivileged against a root-owned file in a root-owned directory; and `NoNewPrivileges=true` closes
every escalation route out of the first two. The split keeps `/usr/local/bin` root-owned and the unit
hardening untouched, adds no new unit, and — because the promote runs on *every* start — a staged binary
can never be left half-applied.

**Why it mirrors the agent anyway.** The shape is identical to `agent/update.go`: verify before replacing,
replace atomically, exit, let the service manager bring the process back. Only the actor performing the
replacement differs, because only the mother is sandboxed.

**Rejected.** `ReadWritePaths=/usr/local/bin` plus a writable directory for the service user: `rename`
needs write permission on the *directory*, so this hands an unprivileged service the ability to replace
anything in `/usr/local/bin` — a system-wide privilege, not a scoped one. Moving the mother to
`/opt/feast-watch/bin` to dodge that: it changes the installed footprint, the manifest, the uninstaller
and every existing deployment, for the same privilege in a quieter location. A `.path` unit plus a root
oneshot updater: two new units, and a race between the mother's own exit and systemd's restart, to buy
independence from a start-time hook that has to run anyway.

### The panel sets a target; the mother converges on it **(owner sign-off required)**

**Choice.** `PUT /api/mother/version {"version": "v1.4.0"}` writes a desired version. An in-process loop
(default 30s, `FW_MOTHER_UPDATE_INTERVAL`) compares it against `shared/version.Version` and acts. An empty
string cancels a target that has not landed.

**Why.** It is the agent's model exactly — set the target, converge — so the panel affordance, the
validation rules and the operator's mental model are one thing across the fleet rather than two. The
carrier differs only because nobody polls the mother: an agent learns its target in the response to a push
it was making anyway (`agent/loop.go:177`), while the mother reads its own row.

**Why 30 seconds.** It is one indexed read of a single row against a database this process already holds
open — cheaper than the hourly retention sweep — and it bounds "I clicked update" to well under a minute.

**Rejected.** Applying inside the request (download, verify, then exit while the caller waits): the panel
would hold a request across the restart it triggered, and a failed download would be reported as a failed
button press rather than as durable state the next operator can see. A two-step download-then-apply pair
of buttons: more state, more copy, and the failure it protects against — a bad download — is already
caught before anything irreversible happens.

### The desired version lives in its own table, never in `settings`

**Choice.** A new single-row table:

```sql
CREATE TABLE IF NOT EXISTS mother_update (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  desired_version TEXT    NOT NULL DEFAULT '',
  staged_version  TEXT    NOT NULL DEFAULT '',
  attempts        INTEGER NOT NULL DEFAULT 0,
  error           TEXT    NOT NULL DEFAULT '',
  requested_at    INTEGER NOT NULL DEFAULT 0,
  applied_at      INTEGER NOT NULL DEFAULT 0
);
```

**Why not `settings`.** `GetSettings` scans every row and `strconv.Atoi`s the value, returning an error for
anything non-numeric. Storing `"v1.4.0"` there would not degrade the version display — it would make
**every settings read in the mother fail**, taking the settings dialog, the heartbeat threshold and the
retention sweep with it. The table also carries per-attempt bookkeeping that has no business in a
key/value bag.

**Migration.** Added to `schema.go` for fresh databases and to `migrate.go` under the existing
`user_version` gate for live ones, following the pattern groups used.

### A bounded attempt count, because a failing mother restarts itself

**Choice.** `attempts` is incremented and committed **before** the download starts, and the loop refuses to
start an attempt once `attempts >= maxAttempts` (3) — so a target is tried at most three times, ever,
across any number of restarts. On boot, if `version.Version == desired_version`, the whole row is cleared
to idle (`desired_version`, `staged_version`, `attempts` and `error` all reset) and `applied_at` stamped.
When the bound is reached with the target still outstanding, the mother clears `desired_version` and
leaves `error` standing as the explanation.

Accepting a **new** target through the API resets `attempts`, `error` and `staged_version` in the same
write: a fresh decision by an operator must not inherit the budget a previous one spent.

**Why.** The agent can retry indefinitely because it stays up and reports each failure on its next push.
The mother cannot: a target it never reaches means download, exit, restart, download — a silent loop that
looks like a crash-looping service and never explains itself. Committing the counter before the risky work
is what makes it a real bound; a counter written after a step that never completes counts nothing.

**Why 3.** Enough to ride out a transient network failure across two restarts, few enough that a genuinely
broken target stops within a minute or two and leaves a readable reason.

### A deployment that cannot promote says so instead of pretending

**Choice.** At boot the mother stats the promote helper (`FW_MOTHER_PROMOTE_PATH`, default
`/usr/local/sbin/feast-watch-mother-promote`). If it is absent, `mother_update_state` reports
`unsupported`, `PUT /api/mother/version` answers `400` with
`"self-update is not available on this deployment"`, and the panel disables the control with that reason.

**Why.** In Docker there is no systemd and no promote hook: the container restarts from its image, so a
staged binary is discarded and the *old* version comes back. Without this the compose deployment would
offer a button whose entire effect is a restart and a mysterious "still on the old version". `Dockerfile.mother`
deliberately does not ship the helper, so the detection is structural rather than a special case for
containers.

### The download half is shared with the agent

**Choice.** Move `fetch`, `download` and the verify-then-place sequence out of `agent/update.go` into
`shared/selfupdate`, exposing roughly:

```go
// Fetch verifies asset's published checksum, streams it to a temp file beside
// dest, and renames it into place. It never restarts anything.
func Place(client *http.Client, baseURL, tag, asset, dest string) error
```

The agent's `SelfUpdate` keeps its signature, its `exit(0)` and its tests; the mother calls `Place` with
the staging path instead of its own executable.

**Why.** Two copies of a checksum-verified binary replacement is two places for the verification to drift,
and the caps that matter (`maxBinarySize`, `maxChecksumSize`), the fsync before rename and the 0755 are
exactly the details a copy loses. Extraction is behaviour-preserving: `agent/update_test.go` is the
regression suite for it and must pass unchanged.

**Rejected.** Importing `agent` from `mother` — it drags the collectors and gopsutil into the mother
binary. Copying the file — see above.

## Data flow

```
panel  ──PUT /admin/monitoring/mother/version {version}──▶  backend proxy (PermSystemAgentUpdate)
                                                                    │
                                                      PUT /api/mother/version  (X-API-Key)
                                                                    ▼
                                                        validate against release index
                                                        write mother_update.desired_version
                                                                    │
                                        ┌───────────────────────────┘
                                        ▼  (≤30s later, in-process loop)
                              attempts++ ; commit
                              GET  <release>/download/<tag>/feast-watch-mother-<plat>.sha256
                              GET  <release>/download/<tag>/feast-watch-mother-<plat>
                              verify sha256  ─── mismatch/404 ──▶ record error, keep target, wait
                                        │ ok
                              place at /var/lib/feast-watch/update/feast-watch.new
                              staged_version = tag ; request shutdown
                              (staged_version is cleared by the boot reconcile,
                               together with the rest of the row)
                                        ▼
                              srv.Shutdown → store.Close → exit 0
                                        ▼
                     systemd: ExecStartPre=+feast-watch-mother-promote  (root)
                              /usr/local/bin/feast-watch → feast-watch.bak
                              staged → /usr/local/bin/feast-watch ; rm staged
                                        ▼
                              ExecStart: new binary boots
                              version.Version == desired ? clear row, applied_at=now
                                            : attempts >= 3 ? clear target, keep error
```

The panel polls `GET /api/version` throughout. Between shutdown and the new listener there is a gap of a
few seconds in which the proxy answers `502`; while `mother_update_state` was last seen as `pending`, the
panel renders that as "yeniden başlatılıyor…" rather than as an error.

## API contract

### `GET /api/version` (extended, backward compatible)

Existing fields are unchanged. Added:

```json
{
  "mother_version": "v1.3.0",
  "agents": [ { "version": "v1.4.0", "platforms": ["linux-amd64"] } ],
  "checked_at": "2026-08-20T09:12:00Z",
  "stale": false,

  "mother_builds": [ { "version": "v1.4.0", "platforms": ["linux-amd64", "linux-arm64"] } ],
  "mother_platform": "linux-amd64",
  "mother_desired_version": "v1.4.0",
  "mother_update_state": "pending",
  "mother_update_error": ""
}
```

`mother_update_state` is a projection, computed the way `updateState` already computes the agent's:

| state         | when                                                              |
|---------------|-------------------------------------------------------------------|
| `unsupported` | the promote helper is not present on this deployment              |
| `idle`        | no target, or target equals the running version                   |
| `pending`     | a target is set, not yet applied, no error recorded               |
| `failed`      | `error` is non-empty                                              |

### `PUT /api/mother/version`

Request `{"version": "v1.4.0"}`; `""` cancels. Responses:

| code | body error                                                | cause                                        |
|------|-----------------------------------------------------------|----------------------------------------------|
| 200  | —                                                         | target accepted (`{"desired_version": ...}`); `attempts`, `error` and `staged_version` reset |
| 400  | `latest is a moving alias, not a version; pick a concrete version` | `"latest"`                          |
| 400  | `version v1.4.0 has no published release`                 | not in the index                              |
| 400  | `no v1.4.0 mother build for linux-amd64`                  | published, but not for the mother's platform  |
| 400  | `self-update is not available on this deployment`         | no promote helper                             |
| 500  | `storage failure`                                         | write failed                                  |

The validation is the same rule as `rejectVersion`, taking a build list rather than reading one, so it
stays a pure function and the mother and agent paths cannot disagree about what "publishable" means.

### Backend proxy

One row in `monitoringRoutes` (`app/api/admin/monitoring_routes.go`):

```go
{http.MethodPut, "/admin/monitoring/mother/version", role.PermSystemAgentUpdate, h.MonitoringSetMotherVersion},
```

`PermSystemAgentUpdate`, not `PermSystemHealth`: this replaces the binary that runs the monitoring. Placed
with the other version routes, after the static `/servers` and `/groups` families — `mother` is a distinct
static segment and shadows nothing. `monitoring_routes_test.go` drives the table through the real
middleware chain, so the gate is asserted rather than assumed.

## Panel

`src/api/monitoring.js` gains `setMotherVersion(version)`, documented like its neighbours: which errors
mother owns, and that `""` cancels.

`MonitoringPage` grows a **Mother** card beside the fleet table showing the running version, the update
state, and the error when there is one. `canUpdate` gates the button, matching the proxy's permission. The
dialog reuses `UpdateAgentDialog`'s shape — a version select fed by `mother_builds`, the `updatePolicy`
downgrade confirmation, `versionListStatus` for the empty/stale distinction — with three differences:

- The target list comes from `mother_builds`, narrowed by `mother_platform`.
- `unsupported` renders the button disabled with mother's own reason, not a client-side guess.
- While `pending`, the card shows a restarting state and a `502` from the version poll is not an error.

## Deployment footprint

| path                                                | who writes it            | removed by            |
|-----------------------------------------------------|--------------------------|-----------------------|
| `/usr/local/sbin/feast-watch-mother-promote`          | `mother-install.sh`      | `mother-uninstall.sh` |
| `/usr/local/bin/feast-watch.bak`                      | promote helper           | `mother-uninstall.sh` |
| `/var/lib/feast-watch/update/`                        | the mother (StateDirectory) | `mother-uninstall.sh` |

All three are added to `/etc/feast-watch/mother-manifest`, which is the file the uninstaller reads — the
installer and uninstaller are meant to be read together, and a file created outside the manifest is a file
nothing cleans.

`ExecStartPre=+…` is added to `deploy/feast-watch-mother.service`. An existing deployment picks it up when
`mother-install.sh` is re-run, which now also restarts a running unit.

## Failure modes

| failure                                  | what happens                                                                 |
|------------------------------------------|------------------------------------------------------------------------------|
| tag has no mother build for this platform | rejected at `PUT` with the platform named; nothing is written                 |
| download 404 / network error              | `error` recorded, target kept, retried next tick until `attempts` runs out    |
| checksum mismatch                         | staged file removed, `error` recorded, target kept — the binary never lands   |
| staged but promote helper missing         | next boot sees version unchanged; after 3 attempts, target cleared with error |
| new binary starts but is the wrong version| same bounded path — the mother reports the mismatch rather than looping       |
| new binary will not start at all          | systemd `Restart=always` retries; **manual rollback** via `feast-watch.bak`   |
| Docker deployment                         | `unsupported`; the `PUT` is refused before anything is downloaded            |

## Testing

**Go, `feast-watch`**
- `shared/release`: mother asset names round-trip; `AssetKindOf` separates the two families and rejects
  checksum companions; `ExpectedAssets` covers mother builds.
- `mother/release`: `buildsFrom` splits agent and mother builds from one release; a release with a mother
  binary but no `.sha256` offers no mother build.
- `mother/store`: `mother_update` create/read/update; the migration adds the table to a pre-existing
  database; the single-row constraint holds.
- `mother/api`: the validation table above, endpoint by endpoint; `unsupported` short-circuits before any
  storage write; `GET /api/version` keeps every existing field.
- `mother/selfupdate`: the loop against an `httptest` release host and a temp state directory, with an
  injected clock and shutdown func — target applied, checksum mismatch, 404, attempts exhausted, and the
  boot-time reconciliation both ways. This mirrors `agent/update_test.go`, which must still pass unchanged.

**Shell**
- A promote-helper test on an `FW_ROOT` temp tree, the way `e2e/colocation_test.sh` already exercises the
  installers without root: promotes a staged file, keeps the `.bak`, is a no-op with nothing staged, and
  is idempotent.
- `shellcheck` at the repo's default (strict) severity — the CI job is green again and must stay so.

**Go, backend** — the route-gate table test picks up the new row; a handler test proves the payload and
mother's `400` pass through with their message intact.

**Panel** — `setMotherVersion` client test; dialog tests for the three differences above; a card test that
`unsupported` disables the control and shows mother's reason.

## Out of scope, and why

- **Automatic rollback on a mother that will not boot.** The `.bak` is written and the rollback is
  documented, but nothing reverts on its own. Doing it properly needs a health signal systemd can act on
  (`OnFailure=` plus a start-limit window), and getting that wrong turns one bad deploy into a flapping
  service. Worth its own round.
- **Schema compatibility across a rollback.** A newer mother migrates the database on boot; rolling back to
  the older binary then faces a schema its `user_version` gate does not know. Today's migrations are
  additive, so this is latent rather than live — but a rollback is exactly when it would bite, and it needs
  a compatibility rule of its own.
- **Rolling several mothers.** One mother per deployment is the current shape; nothing here assumes a
  fleet of them.
- **Signing releases.** Checksums prove integrity against corruption, not authorship. Unchanged from the
  agent's position, and named again here because self-updating the control plane raises the stakes.

## Open questions for the owner

1. **Downgrade guard on the mother.** The agent dialog holds a rollback behind a 5-second confirmation.
   The same guard on the mother, or a stronger one, given that a bad mother version takes the panel down
   with it?
2. **Who may do it.** `PermSystemAgentUpdate` groups this with agent rollouts. Should replacing the control
   plane need a separate permission?
3. **`v1.0.0` is ambiguous.** The tag was moved three times, so that version string names three different
   binaries. The mother rollout should probably refuse it explicitly rather than let someone target it.
