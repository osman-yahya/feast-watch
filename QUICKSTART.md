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
mother's build catalogue, and the settings payload rules:

```bash
./e2e/local_smoke.sh
```

## Production

1. Cut a version:

   ```bash
   sudo -u feast-watch \
     FW_DB_PATH=/var/lib/feast-watch/mother.db \
     feast-watch build v1.3.0
   ```

   The mother fetches that tag's source archive, compiles every platform — four agent builds
   and two of itself — writes a SHA-256 beside each, and publishes them into its
   catalogue at `/var/lib/feast-watch/builds/v1.3.0/`. It is the one command
   that needs a Go toolchain on this host, and the price of answering to nothing
   outside it.

   The toolchain itself comes with the installer — `deploy/mother-install.sh`
   downloads the pinned Go, verifies its published SHA-256, unpacks it into
   `/usr/local/go` and links it as `/usr/local/bin/go`. A host that already has
   a new-enough Go keeps it, `--skip-go` opts out entirely, and
   `mother-uninstall.sh --purge` removes only a toolchain the installer put
   there (it records `go=` in the manifest to tell the two apart).

   Run it as the service account, as above. The toolchain's caches go beside the
   catalogue when the environment names none, so this works despite
   `feast-watch` having no home directory — which the unit wants and the Go
   toolchain otherwise refuses to compile without. Where the caches should live
   somewhere else, or have been seeded by hand on a host with no egress, name
   them and they are used as given:

   ```bash
   sudo -u feast-watch \
     FW_DB_PATH=/var/lib/feast-watch/mother.db \
     GOMODCACHE=/var/lib/feast-watch/gomod \
     feast-watch build v1.3.0
   ```

   The tag *is* the version: it is what gets compiled in and what agents ask
   for, with no mapping in between. **A version is built once.** Rebuilding one
   that already exists is refused, because agents compare versions as strings
   and a fleet cannot tell two builds of `v1.3.0` apart.

   The build lands whole or not at all: it compiles into a staging directory and
   renames at the end, so a version never appears in the catalogue holding four
   of its six platforms.

   Fetching the source is the only thing this project asks of the internet, only
   this host asks it, and it asks for **source rather than binaries**: what the
   fleet runs is compiled here. Point `FW_SOURCE_DIR` at a checkout instead and
   even that goes away — useful for a private fork, or a host that can reach
   nothing.

   There is no switch for any of this. The mother reads that catalogue as its
   release index — which versions exist, which platforms each covers — and
   serves the binaries themselves at GitHub's own URL shape. Nothing in the loop
   reaches the internet: not the bytes, not the tag that names them, and nothing
   on the fleet has anywhere else to ask.

   **This replaces GitHub Releases**, and with it the CI that used to publish
   them. What CI checked now runs from `bin/check.sh`, which anyone can run and
   which needs nothing to be reachable:

   ```bash
   bin/check.sh            # vet, gofmt, race tests, shellcheck, every e2e suite
   bin/check.sh go         # just the Go suite
   ```

   What it costs, stated plainly: this mother is the only authority for what a
   version means. Nothing outside it computed the checksum an agent verifies
   against, and there is no published artifact left to compare a build with —
   reproducing a version means having the same source tree, not fetching the
   same file. And binary distribution is now on the monitoring path: the
   mother's disk and its uptime decide whether a rollout can land. Each build is
   about 12MB per platform, nothing is evicted automatically, and the catalogue
   lives under `StateDirectory`, so `mother-uninstall.sh --purge` removes it.

### Agents never reach the internet

An agent has exactly one address: `MOTHER_URL`. It pushes there, it is told what
to become there, and it downloads the binary to become there — from
`/releases/latest/download/<asset>` and `/releases/download/<tag>/<asset>`, the
URL shape a release host used to answer, now answered out of the catalogue the
mother compiled. There is no second host in `agent.conf` to point somewhere
else, because a fleet with no route off its network has nowhere else to point.

The served installer writes that one address too, so a host is never handed a
URL it cannot resolve. If the download 404s, nothing has been built on the
mother yet — run `feast-watch build <version>` there, then re-run the one-liner.

What an agent verifies is still verified: the mother writes a SHA-256 beside
every binary it compiles, the agent fetches both and refuses anything that does
not match. What the checksum no longer proves is that somebody *else* agreed
what the bytes should be — the mother built them and vouches for them. That is
the trade, and what buys it back is that there is no third party in the path at
all.

**Hosts installed before this change** carry a `RELEASE_BASE_URL` line in
`/etc/feast-watch/agent.conf` naming GitHub. Newer agents ignore it; the line is
inert and can be deleted. Their next update comes from the mother either way.

### Upgrading the mother

From the panel: open the Mother card on the monitoring page, pick a built
version, confirm. The mother downloads that build from its own catalogue, over
its own HTTP surface — the exact path the fleet's updates travel, so a broken
download is something it finds out about its own update rather than being the
one client that never noticed. It verifies
the SHA-256, stages it in `/var/lib/feast-watch/update/`, and shuts down. systemd
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
sudo deploy/mother-install.sh --build              # compile from this checkout
sudo deploy/mother-install.sh --download           # published bootstrap build
sudo deploy/mother-install.sh --download=v1.4.0    # a specific one
```

`--download` fetches a published mother binary for this host's architecture,
verifies its SHA-256, and installs it. It is the **bootstrap and only the
bootstrap**: a first mother has to come from somewhere, and this host — unlike
every host it will go on to monitor — is the one with a route off the network.
Afterwards the mother produces its own replacements, with the Go toolchain the
installer put here alongside it — which is what `--build` uses to compile the
mother from a checkout instead, no published release needed. `--with-agent`
takes its agent binary from this mother's catalogue, the same place every other
agent on the fleet takes one from.

The mother binary itself is statically linked pure Go: it links no libc, embeds
its SQLite (`modernc.org/sqlite`, no cgo) and shells out to nothing at runtime.
What it does need on its host is Go, for `feast-watch build` — which is why the
container image is `golang` with the source and the module cache in it rather
than a bare `alpine`.

Building on the host by hand — for an unreleased fix or a private fork — is the
same toolchain doing the same work in two steps instead of one:

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
- **The mother's only egress is a source archive**, `github.com`, and only when
  `feast-watch build` is run without `FW_SOURCE_DIR`. Nothing fetches it on a
  timer and nothing needs it to serve, ingest or roll out — see the
  closed-egress note below.
- **Agents have exactly one peer.** They push to the mother, and they download
  the binary it named from the same address, verifying the SHA-256 the mother
  wrote beside it. A monitored host resolves nothing but `FW_PUBLIC_URL`.

### What to open

On the **mother host**:

| Direction | Peer | Port | Why |
|---|---|---|---|
| in | every monitored host | `FW_LISTEN` (`8443`) | `POST /v1/ingest` (agent token) |
| in | feast backend (api pods) | `FW_LISTEN` | `/api/*` with `X-API-Key`, for the admin panel proxy |
| in | a host being installed | `FW_LISTEN` | `GET /install/{token}.sh`, `GET /uninstall.sh` |
| out | `github.com:443` | 443 | source archive, only while running `feast-watch build` |

Nothing else. In particular there is no mother→agent rule to write.

On a **monitored host**: no inbound rule at all, and outbound to the mother's
`FW_PUBLIC_URL` and nothing else — pushes, the install script, the uninstaller
and the binaries all live there.

`ufw` on the mother, spelled out:

```bash
ufw default deny incoming
ufw default deny outgoing
ufw allow proto tcp from <agent-subnet>   to any port 8443
ufw allow proto tcp from <backend-subnet> to any port 8443
ufw allow out 53                 # resolve github.com
ufw allow out proto tcp to any port 443   # source archives for `feast-watch build`
```

### Closing the egress completely

Drop the `443` rule and the mother can no longer fetch a source archive. That is
the only thing it loses, and there is a direct replacement: carry a checkout onto
the host and point `FW_SOURCE_DIR` at it.

```bash
# /etc/feast-watch/mother.env
FW_SOURCE_DIR=/opt/feast-watch/src
```

`feast-watch build v1.3.0` then compiles from that tree, the catalogue fills as
before, and the fleet updates from it. Nothing else changes: the rollout dropdown
reads the same catalogue, agents keep pushing, and the mother's own host — which
runs an agent like any other — updates from the mother beside it.

Two things have to be carried in rather than fetched, and both are one-time.
The Go toolchain: install it while the egress is open, or with `--skip-go` and
by hand — the installer's own download needs `go.dev`. And the module cache: a
`go build` on a tree it has never compiled before reaches `proxy.golang.org`, so
run one build before the egress closes, or bring `$GOMODCACHE` in with the
source and name it (see step 1).

## Updating agents

Stage the new release on the mother (step 1 above), then set the target version
on one server at a time from the panel, or directly:

```bash
curl -sf -H "X-API-Key: $FW_API_KEY" -X PUT \
  http://<mother-ip>:8443/api/servers/<id>/version -d '{"version":"v1.3.0"}'
```

The agent picks the target up on its next push, downloads that build from the
mother, verifies the checksum, replaces itself and exits for systemd to restart
it. A version it was never given cannot be a target: the panel and the API both
check against the catalogue, so a rollout fails at the point somebody typed it
rather than on thirty hosts an hour later. Watch `update_state` on
`GET /api/servers`: `pending` while it converges, `idle` once `agent_version`
matches, `failed` with `update_error` if it could not install. Send
`{"version":""}` to cancel a rollout that has not landed.

One host at a time is the intended rhythm — update it, confirm it, then the
rest. A group target (below) is a fan-out over that same per-server field, not
a second kind of state, so nothing about confirming a host first changes.

The mother updates itself from the same catalogue, from the panel — see
[Upgrading the mother](#upgrading-the-mother). `GET /api/version` reports what it
is running, what it was told to become, and how that is going.

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

`--build` is the from-scratch path: the installer puts the Go toolchain on the
host first, then compiles the mother from the checkout it was run out of, so no
published release has to exist and nothing has to be built beforehand. With
`--with-agent` it compiles the agent in the same pass, which is the whole of a
brand-new deployment in one command.

That bootstrap needs nothing on the host but `curl`, `tar` and systemd: the
mother binary and the pinned Go toolchain are both downloaded and both verified
against a SHA-256 before anything is installed. The toolchain is not optional
here — this host compiles what the fleet runs — but `--skip-go` leaves it to you
where the host provisions its own. To install a mother binary you built yourself
— an unreleased fix, a private fork — pass its path instead:

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
and installs the agent — the mother from the published bootstrap build, the
agent from the mother's own catalogue, each verified against its checksum. On a
brand-new deployment that catalogue is empty, so either run `feast-watch build
<version>` on this host first, or let the installer compile both from the
checkout in one pass:

```bash
sudo deploy/mother-install.sh --with-agent --build
```

Here the agent comes from the binary just built beside the mother, so nothing is
fetched at all — which is what makes this the way to bring up a deployment that
has never compiled a version yet.

The two share `/etc/feast-watch` but nothing else: separate binaries, separate
units, separate config files. Each uninstaller removes only its own files and
drops the shared directory only once it is empty, so removing one does not take
the other's API key or token with it. `e2e/colocation_test.sh` asserts exactly
that.
