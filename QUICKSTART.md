# QUICKSTART

## Local development

Local development only — production uses systemd (see below), not Docker.

The compose stack joins a `feast-watch` network it declares external, so that
`docker compose down` here cannot break the feast backend's devcontainer, which
declares the same one. Create it once, then bring the stack up:

```bash
docker network create feast-watch   # once per machine
docker compose up -d --build
```

Add a server via the admin API:

```bash
curl -sf -H "X-API-Key: dev-api-key" -X POST http://localhost:8443/api/servers \
  -d '{"name":"my-server"}'
```

Run the full push → rollup → chart cycle end-to-end:

```bash
./e2e/e2e_test.sh
```

Where Docker is not available — a laptop, or a quick check between edits —
`e2e/local_smoke.sh` runs the mother and an agent as plain binaries against a
throwaway database in a temp directory, removing everything on exit. It asserts
the same push → rollup → chart path plus groups, rollout validation against the
published releases, and the settings payload rules:

```bash
./e2e/local_smoke.sh
```

## Production

1. Publish a release:

   ```bash
   git tag v1.3.0
   git push origin v1.3.0     # the push is what publishes; the tag alone does nothing
   ```

   `.github/workflows/release.yml` then verifies the version links in, **creates
   the GitHub release for the tag**, builds every platform with the tag compiled
   in, uploads each binary plus its `.sha256`, and finally asserts the release
   carries every asset `shared/release.Platforms` says it should. The tag *is*
   the version: it is what gets compiled in and what agents ask for, with no
   mapping in between.

   Two failure modes this used to have, both of which produced a version that
   simply never appeared in the panel:

   - A tag that was created but never pushed. The workflow fires on
     `push: tags`, so a local-only tag publishes nothing at all. `bin/release.sh`
     now says so when the version it built names a tag that is not on origin.
   - A tag pushed with no release object behind it. The workflow only ever ran
     `gh release upload`, which fails with `release not found` against a tag
     that has no release — so the only release that ever worked was one created
     by hand. The workflow creates it now.

   Do not move a tag that has already been published. The upload step replaces
   assets in place, so a moved tag makes one version string mean two different
   binaries, and agents compare versions as strings — a fleet cannot tell those
   apart and will never reconcile. The workflow refuses a tag push onto a
   release that already has assets; re-run it from `workflow_dispatch` when a
   re-upload is genuinely what you want.

   By default the mother stores no binaries and serves none. It reads the
   published releases from the GitHub API — a conditional request every five minutes,
   which is not counted against the unauthenticated rate limit when nothing
   changed — and offers only versions carrying both a binary and its checksum
   for the target host's platform.

   `bin/release.sh` still builds every platform locally, named exactly as the
   release assets, for development or for uploading by hand if CI is
   unavailable.

### Agents that cannot reach the internet

Agents download their own binaries from GitHub Releases, which keeps binary
distribution off the monitoring path entirely: the mother stores no builds,
serves no bytes, and a rollout cannot be blocked by the mother's disk. That is
the better arrangement wherever it works.

Where it does not — a fleet whose agents have no route to the internet — the
mother can stand in the middle:

```bash
# /etc/feast-watch/mother.env
FW_MIRROR_BINARIES=true
```

The mother then serves GitHub's own URL shape (`/releases/download/<tag>/<asset>`
and `/releases/latest/download/<asset>`), fetching each build the first time it
is asked for, verifying it against the checksum GitHub published, and keeping it
under `/var/lib/feast-watch/binaries/`. Agents need no new code and no new
protocol: `RELEASE_BASE_URL` was always their way of being told where to
download from, and the installer now writes the mother's address into it.

What this changes, stated plainly:

- Binary distribution is now on the monitoring path. The mother's disk and its
  uptime decide whether a rollout can land.
- Each build is about 12MB per platform. Nothing is evicted automatically; the
  cache lives under `StateDirectory`, so `mother-uninstall.sh --purge` removes it.
- The mother still needs to reach `github.com` itself. If nothing can, a build
  has to be carried in by hand — mirroring solves the agents' isolation, not the
  mother's.
- The chain is unbroken, not merely shortened: CI computes the checksum, the
  mother refuses anything that does not match it, and the agent verifies again
  before replacing itself. The mother adds a hop, never an authority — it builds
  nothing and signs nothing.

**Existing agents keep their old setting.** The installer writes
`RELEASE_BASE_URL` at install time, so hosts installed before this was turned on
still name GitHub. Point them at the mother by editing that line in
`/etc/feast-watch/agent.conf` and restarting `feast-watch-agent`, or by re-running
the served installer.

### Upgrading the mother

From the panel: open the Mother card on the monitoring page, pick a published
version, confirm. The mother downloads that build from GitHub Releases, verifies
its SHA-256, stages it in `/var/lib/feast-watch/update/`, and shuts down. systemd
restarts it, `ExecStartPre=+/usr/local/sbin/feast-watch-mother-promote` installs
the staged binary as root, and the new one starts. The panel is unreachable for a
few seconds in the middle; while a target is pending it shows that rather than an
error.

The mother cannot install its own binary and is not meant to: its unit runs it as
an unprivileged user under `ProtectSystem=strict`, so `/usr/local/bin` is
read-only inside its mount namespace. It verifies and stages; root promotes.

A target is tried at most three times, counted across restarts. After that the
mother drops the target and leaves the reason on the card — it does not keep
downloading and exiting.

**Where this does not work.** In Docker there is no systemd and no promote
helper, so the image does not ship one: the mother reports `unsupported` and
refuses a target instead of restarting into the same version. Upgrade a
containerised mother by building a new image.

**By hand**, if the panel is not available:

```bash
sudo deploy/mother-install.sh --download            # newest published build
sudo deploy/mother-install.sh --download=v1.4.0     # a specific one
```

`--download` fetches the published mother binary for this host's architecture,
verifies its SHA-256, and installs it — the same rule the agent installer and
the mother's own self-update apply. Nothing else has to be on the host: no Go
toolchain, no checkout, no build. `--with-agent` takes its agent binary from the
same release.

The mother is a statically linked pure-Go binary: it links no libc, embeds its
SQLite (`modernc.org/sqlite`, no cgo), shells out to nothing, and compiles
nothing at runtime. A host that can run it needs no compiler afterwards, which
is also why the container image is a bare `alpine` plus the binary.

Building on the host instead — for an unreleased fix or a private fork — still
works and is the one case that needs Go:

```bash
bin/release.sh --mother-only
sudo deploy/mother-install.sh bin/build/feast-watch
```

The installer restarts an already-running unit, so the new binary is what ends up
running. It used to only `systemctl start`, which is a no-op on an active unit —
the upgrade appeared to succeed while the old process kept executing from the
unlinked inode, and the panel kept reporting the old version.

**Rolling back a mother that will not start.** The promote helper keeps the
previous binary:

```bash
sudo systemctl stop feast-watch-mother
sudo mv /usr/local/bin/feast-watch.bak /usr/local/bin/feast-watch
sudo systemctl start feast-watch-mother
```

2. Run the mother with environment variables from [`.env.example`](.env.example)
   (copy it to `.env`, fill in real values, and load it into the environment).

   The mother serves plain HTTP and does not terminate TLS. Where TLS is
   wanted, put a reverse proxy in front and set `FW_PUBLIC_URL` to the proxy's
   URL — that value is what every agent is handed, so it has to be the address
   that actually answers.

   Migrating a mother that used to serve TLS: either keep a proxy on the old
   `https://<ip>:8443` address, in which case no host is touched, or run
   [`deploy/migrate-agent-http.sh`](deploy/migrate-agent-http.sh) on each
   monitored host. An agent holding an `https://` URL against a plain-HTTP
   mother fails at the transport, and nothing in the protocol can re-point it:
   the ingest response carries no URL and the config is read once at startup.
   Kubernetes agents read their config from a Secret, so they need that Secret
   patched and the DaemonSet rolled instead.

3. From the admin panel (or CLI), add a server:

   ```bash
   feast-watch generate --name=DB_Sunucusu
   ```

   Either flow prints a one-liner. Paste it on the target server:

   ```bash
   curl -sSL http://<mother-ip>:8443/install/<token>.sh | sudo bash
   ```

   The install script downloads the right-arch agent binary, writes
   `/etc/feast-watch/agent.conf`, and installs + starts the `feast-watch-agent`
   systemd service. The server flips from *pending* to *online* on its first push.

   Kubernetes nodes use `deploy/k8s/daemonset.yaml` instead (hostPID + `/proc`
   mount, token supplied as a Secret).

## Network posture

The direction of every connection is fixed by the protocol, not by
configuration — which is what lets the mother's host be firewalled as
**inbound-only**.

- **Agents do not listen.** There is no server in the agent: no port, no
  socket, nothing to reach. Grep for it — `agent/` contains no
  `net.Listen`/`ListenAndServe`. A monitored host exposes no new surface at
  all.
- **The mother never dials an agent.** Every exchange is opened by the agent.
  It POSTs `/v1/ingest`, and the *response* carries what the mother wants
  applied: the collector set, the push interval and `desired_version`
  (`shared/protocol.IngestResponse`). A rollout is therefore an **answer to a
  push**, never a request to a host. The mother does record each agent's IP,
  but only to show it in the panel — no code path connects to it, and the
  fleet works identically with all outbound traffic from the mother blocked.
- **The mother's only egress is the GitHub API**, `api.github.com` — a
  conditional GET of the release list every 5 minutes
  (`FW_RELEASE_POLL_INTERVAL`), so it can offer only rollout targets that are
  actually downloadable. It is optional: see the closed-egress note below.
- **Binaries never cross the monitoring path.** The mother names a version;
  the agent downloads that build from the public GitHub release and verifies
  its SHA-256 itself. The mother stores no binaries and serves no bytes.

### What to open

On the **mother host**:

| Direction | Peer | Port | Why |
|---|---|---|---|
| in | every monitored host | `FW_LISTEN` (`8443`) | `POST /v1/ingest` (agent token) |
| in | feast backend (api pods) | `FW_LISTEN` | `/api/*` with `X-API-Key`, for the admin panel proxy |
| in | a host being installed | `FW_LISTEN` | `GET /install/{token}.sh`, `GET /uninstall.sh` |
| out | `api.github.com:443` | 443 | release index only |

Nothing else. In particular there is no mother→agent rule to write.

On a **monitored host**: no inbound rule at all. Outbound to the mother's
`FW_PUBLIC_URL`, and — only while installing or self-updating — to the release
host (`RELEASE_BASE_URL`, by default `github.com`, which redirects downloads to
`objects.githubusercontent.com`). Point `RELEASE_BASE_URL` at an internal
mirror to drop that one too.

`ufw` on the mother, spelled out:

```bash
ufw default deny incoming
ufw default deny outgoing
ufw allow proto tcp from <agent-subnet>   to any port 8443
ufw allow proto tcp from <backend-subnet> to any port 8443
ufw allow out 53                 # resolve api.github.com
ufw allow out proto tcp to any port 443   # the release index
```

### Closing the egress completely

Drop the `443` rule and the mother stops reading GitHub. Two consequences,
both bounded:

1. The rollout dropdown has no versions to offer, so state them by hand with
   `FW_AGENT_VERSIONS` (see [`.env.example`](.env.example)). A successful
   fetch would replace the seed; with no route, the seed simply stands.
2. The mother monitors its own host, and *that* agent then cannot self-update
   either — it downloads from the same release host every other agent does.
   Update it the way the mother itself is updated, by hand.

Neither touches ingest: agents keep pushing, the panel keeps reading, and a
target set by hand still reaches every host.

## Updating agents

Stage the new release on the mother (step 1 above), then set the target version
on one server at a time from the panel, or directly:

```bash
curl -sf -H "X-API-Key: $FW_API_KEY" -X PUT \
  http://<mother-ip>:8443/api/servers/<id>/version -d '{"version":"v1.3.0"}'
```

The agent picks the target up on its next push, downloads that build from the
GitHub release, verifies the checksum, replaces itself and exits for systemd to
restart it. The mother is never in the binary path, so a rollout cannot be
blocked by a file nobody staged on it. Watch `update_state` on
`GET /api/servers`: `pending` while it converges, `idle` once `agent_version`
matches, `failed` with `update_error` if it could not install. Send
`{"version":""}` to cancel a rollout that has not landed.

One host at a time is the intended rhythm — update it, confirm it, then the
rest. A group target (below) is a fan-out over that same per-server field, not
a second kind of state, so nothing about confirming a host first changes.

The mother is not self-updating: `GET /api/version` reports its version so you
can see what the agents should catch up to, but deploying it stays with
systemd/Docker/k8s.

### Groups

Servers can be put into named groups, the fleet list filtered by one, and a
whole group pointed at a version in a single call. Membership is many-to-many:
the axes worth slicing by — environment, role, region — are independent, and a
single group column on a server would force picking one of them.

```bash
# create a group, fill it, then point it at a version
curl -sf -H "X-API-Key: $FW_API_KEY" -X POST \
  http://<mother-ip>:8443/api/groups -d '{"name":"prod-db"}'
curl -sf -H "X-API-Key: $FW_API_KEY" -X PUT \
  http://<mother-ip>:8443/api/groups/<id>/servers -d '{"server_ids":[1,2,3]}'
curl -sf -H "X-API-Key: $FW_API_KEY" -X PUT \
  http://<mother-ip>:8443/api/groups/<id>/version -d '{"version":"v1.3.0"}'
```

The bulk rollout splits two different kinds of fault. A bad version —
unpublished, or the moving alias — is a property of the request: it fails with
400 and nothing is written, because no member could ever accept it. A missing
build for one host's platform is that host's problem: it is skipped and named in
`skipped[]` while the rest land in `applied[]`, so one darwin laptop cannot
permanently block a rollout across forty Linux servers. Either way the response
is 200 — the writes that could land did. Sending `{"version":""}` clears the
target on every member regardless of platform, so a rollout can always be
cancelled group-wide.

`GET /api/servers?group_id=<id>` narrows the fleet list to one group, and every
server row carries its own `groups[]`. `GET /api/groups` lists them, `PATCH
/api/groups/{id}` renames one (a duplicate name is a 409), and `DELETE
/api/groups/{id}` removes one along with its memberships — no server is touched.

## Storage

Every push is folded straight into the 1-minute and 1-hour rollups as it
arrives. There is no raw sample tier and no background rollup job: nothing ever
read the raw rows except the job that reduced them, and the chart API floors its
interval at 60 seconds, so the 10-second resolution was unreachable through any
endpoint. At 50 servers this takes daily row-writes from roughly 32M to 4M.

Upgrading an existing mother is automatic. On first start it rebuilds both
rollup tables — carrying `avg` across as `avg*cnt`, which is the same weighted
total the chart query already computed on read — drops the raw table, and runs
one `VACUUM` to hand the freed pages back. On the repo's own sample database
that took 471 KB to 74 KB with every stored value preserved. The rebuild copies
both tables, so allow for a slow first start on a large database and take a
backup first.

`retention_raw_hours` is gone from the settings payload. It is still accepted
and ignored on the way in, so an older panel or proxy is not rejected while it
catches up.

## The live view

The rollups start at 1-minute resolution, so nothing stored can show what a
server is doing *right now* at the cadence agents actually push at. That
resolution is kept in the mother's **memory** instead — the last N minutes of
every sample, per server — and served from there:

```bash
curl -sf -H "X-API-Key: $FW_API_KEY" \
  "http://<mother-ip>:8443/api/live?server_id=1&metric=cpu.usage,memory.usage"

# every poll after the first one: only what arrived since
curl -sf -H "X-API-Key: $FW_API_KEY" \
  "http://<mother-ip>:8443/api/live?server_id=1&metric=cpu.usage&since=1787155749"
```

It reads nothing from SQLite, which is the point: the panel polls it every few
seconds and never touches the single write connection ingest depends on.

- **Window:** `live_window_minutes` in settings, 1–60, default 60. It is a
  memory budget, not a retention policy — a minute of it is held for every
  server, so raising it costs RAM on the mother and nothing else. Measured at
  ~23 bytes per point held: a full hour across 30 servers reporting 17 metrics
  every 10 seconds is about 2MB, and the 60-minute ceiling is what keeps the
  arithmetic negligible on a fleet several times that size.
- **`since=<unix seconds>`** narrows the answer to the points that arrived
  after a timestamp the caller already holds. It is what makes polling cheap:
  the first read takes the window, every read after it takes the two or three
  points that are new. Strictly newer, so passing back the newest timestamp you
  hold never repeats a sample. Malformed or negative is a 400 rather than a
  silent fall back to the whole window.
- **`server_time`** in every answer is the mother's own clock. Points are
  stamped by that clock, so a caller slicing "the last five minutes" should
  slice against this rather than against its own — the two drift.
- **Not persisted, deliberately.** A restart empties it and the next pushes
  refill it within one window. Buying durability would cost a write per push,
  which is exactly the write volume the raw tier was dropped to avoid.
- **`GET /api/servers` carries the newest value of every metric** (`latest`,
  `latest_ts`) from the same store, so the fleet table and the group overview
  cost one request no matter how many servers they show.

Unlike every other settings field, `live_window_minutes` may be **omitted** from
a `PUT /api/settings` payload: it gates no delete, and requiring it would reject
every caller written before it existed.

## What the agent costs the host it watches

The agent wakes at the interval the mother gives it, collects, pushes, and goes
back to sleep — there is no background work between pushes and no server on it
to answer. One full collection of the four base collectors, measured with
`go test ./agent/collectors -bench . -run XXX` on an M4 Pro:

| Collector | per collection | allocations |
|---|---|---|
| cpu | ~6.5 µs | 1.3 KB |
| memory | ~11 µs | 1.9 KB |
| uptime | ~1.2 µs | 0.1 KB |
| disk | ~1.1 µs | 0.1 KB |
| **all four together** | **~17 µs** | **3.7 KB** |

At the default 10-second interval that is roughly two thousandths of a percent
of one core. Two details are what keep it there, and both are load-bearing:

- **CPU sampling never sleeps the loop.** `cpu.Percent(0, false)` reads the
  delta since the previous call. The blocking form would hold the agent for
  whatever sampling window it was given, on every push.
- **Service collectors hold one connection, not a pool.** The Postgres probe
  keeps a single handle capped at one connection with an idle and a lifetime
  bound, because a database is monitored precisely when its connection budget
  matters. Dial-per-sample would put the monitoring load where it hurts most.

The push itself is one HTTP request with a 5-second timeout; a failed push is
dropped and retried on the next tick rather than queued, so a mother outage
costs the host nothing that accumulates.

## Removing it

### Deleting a server from the panel

**Sil** removes the agent from the host as well as the row from the mother, and
it takes two phases to do it. The mother cannot dial an agent (see [Network
posture](#network-posture)), so the only channel to a host is the answer to its
own push:

1. The delete is recorded on the row, which stays. The server shows as
   **`uninstalling`**.
2. Every push from that agent is answered with "remove yourself" until it goes
   away — a failed or interrupted attempt is retried without anyone pressing
   anything again.
3. The agent starts the uninstaller as a **transient systemd unit**, because
   the first thing that script does is stop the agent's own service; a plain
   child would be killed halfway through removing the files it is standing on.
4. The uninstaller removes everything and then reports it (`POST
   /v1/uninstalled`, authenticated with the host's own token, and only valid
   for a server somebody actually deleted). **That** report drops the row.

The confirmation comes from the uninstaller rather than from the agent on
purpose: an agent can only ever say "I am about to remove myself", and if that
attempt then failed the mother would have forgotten a host still running an
agent it can no longer reach.

Two cases skip the wait, because there is nobody on the other end to tell:

- **Zorla Sil** (`DELETE /api/servers/{id}?force=true`) — the operator's way
  out for a host that is never coming back. The row and its history go now; if
  the machine is still alive, run the uninstaller on it by hand.
- A server that **never pushed** — nothing was ever installed that could
  report back, so the row is dropped outright.

An agent that cannot remove itself (no uninstaller on disk, no systemd) reports
why on its next push, and the panel shows that reason on the `uninstalling`
row instead of leaving it silently stuck.

### By hand, on the host

The installer leaves the uninstaller on disk, so this works even when the
mother is already gone:

```bash
sudo feast-watch-agent-uninstall --purge            # remove the agent
sudo feast-watch-agent-uninstall --purge --report   # ...and tell the mother, so the row goes too
```

`--report` reads the mother URL and token out of `agent.conf` — never from an
argument, which `/proc` would show to every user on the host — and is what the
agent passes when the removal was requested from the panel.

Mother, on its own host:

```bash
sudo deploy/mother-uninstall.sh --purge
```

Order matters and the flags are load-bearing. See
[`deploy/TEARDOWN.md`](deploy/TEARDOWN.md) for what each one removes, why the
host is cleaned before the panel record, and why the shared Docker network must
be left alone.

The mother can also be installed as a service rather than run by hand:

```bash
sudo deploy/mother-install.sh --download
# edit /etc/feast-watch/mother.env, then:
sudo systemctl start feast-watch-mother
```

That needs nothing on the host but `curl` and systemd: the published binary is
downloaded and its SHA-256 verified before it is installed. To install a binary
you built yourself — an unreleased fix, a private fork — pass its path instead,
which is the one path that needs Go on the host:

```bash
bin/release.sh v1.3.0
sudo deploy/mother-install.sh bin/build/feast-watch
```

## Mother and agent on the same host

This is the deployment the architecture assumes — the mother monitors its own
host — and it is one command:

```bash
sudo deploy/mother-install.sh --with-agent --download
```

It installs and starts the mother, registers this host under its own hostname,
and installs the agent — both binaries from the same published release, each
verified against its checksum.

From a checkout it works without any release existing at all, which is what
makes it the way to bring up a brand-new deployment:

```bash
bin/release.sh v1.3.0
sudo deploy/mother-install.sh --with-agent bin/build/feast-watch
```

Here the agent comes from the binary just built beside the mother, so nothing is
fetched — unlike the served one-liner, which can only download from a published
release.

The two share `/etc/feast-watch` but nothing else: separate binaries, separate
units, separate config files. Each uninstaller removes only its own files and
drops the shared directory only once it is empty, so removing one does not take
the other's API key or token with it. `e2e/colocation_test.sh` asserts exactly
that.
