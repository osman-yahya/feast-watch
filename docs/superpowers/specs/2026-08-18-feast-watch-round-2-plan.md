# feast-watch Round 2 — TLS Removal, Write Volume, GitHub Releases, Groups, Uninstall

Date: 2026-08-18
Repos touched: `feast-watch` (mother, agent), `feast-mobile-backend-control` (panel), backend proxy (absent from this machine)

Produced by a 13-agent analysis pass: six parallel domain investigations, each adversarially
verified against the real code, then synthesised into this plan.

## Purpose

Six owner requirements for this round:

1. Remove TLS (`FW_TLS_CERT` / `FW_TLS_KEY`) entirely — it constrains the deployment shape.
2. Too many rows written per push — reduce the ingest write volume.
3. The update dialog offers newer *and* older versions; picking an older one must impose a
   5-second wait plus an explicit confirmation.
4. A distinct permission for the update action, and server groups.
5. Agents download binaries from public GitHub Releases, not from the mother.
6. Clean uninstall from a system, for both the agent and the mother.

## Executive summary

Eight workstreams, ordered so the four most-contended files (`mother/api/install.sh.tmpl`, `mother/cmd/feast-watch/main.go`, `mother/store/{store,servers}.go`, `mother/api/versions.go`) are each edited by one workstream at a time, with every later edit building on the settled shape of the earlier one.

**The order is not arbitrary.** Installer hygiene goes first because it fixes a live defect — `mother/api/install.sh.tmpl:20` runs `curl -sSLk` with no `--fail`, so an HTTP 404 body is written to `/usr/local/bin/feast-watch-agent` and `chmod 0755`'d, producing a systemd crash-loop; the mother's own `downloads/` on this machine does not contain `latest-linux-amd64`, so this is broken *today*, and it also silently breaks the "re-run the installer" fallback that three later workstreams lean on. TLS removal goes second because the GitHub-Releases work is unsafe until `agent/cmd/feast-watch-agent/main.go:49` stops building the update client from `cfg.HTTPClient` (which sets `InsecureSkipVerify: true`). Write-volume goes before groups because both edit `DeleteServer` and the schema, and write-volume introduces the `user_version`-gated migration machinery that groups then reuses. GitHub-Releases goes before groups because it turns `availableBuilds()` from an `os.ReadDir` per call into a cached snapshot — which makes bulk group rollout a single read instead of N directory scans, for free.

**Two things are more urgent than the six requirements.** `handlePutSettings` (`mother/api/admin.go:175-196`) validates only `interval`; a settings PUT omitting `retention_1m_days` stores 0 and the next hourly job deletes fifteen days of history for every server. And nothing can re-point a deployed agent — `protocol.IngestResponse` carries three fields, none of them a URL — so the TLS change silently blinds the fleet unless a proxy fronts the old `https://<ip>:8443`.

**Deferred deliberately:** metric-name interning, a Windows/macOS installer, and release signing. Each is named in the open questions.

## Decisions

### What replaces the mother's `scheme` + `publicAddr` after TLS removal **(owner sign-off required)**

**Choice.** Delete both fields and `agentTLSSkipVerify`; introduce one validated `FW_PUBLIC_URL` base URL (new `mother/publicurl.go`), with `FW_PUBLIC_ADDR` accepted for one release as a deprecated fallback that logs a warning.

**Why.** Hardcoding `http` makes the target deployment — plain HTTP behind a TLS-terminating proxy — undescribable, because the proxy terminates at a hostname/port/path the mother never serves. A single validated base URL also makes a path prefix (`https://ops.feast.tr/watch`) work, and the agent already builds every URL by bare concatenation onto MotherURL (`agent/loop.go:83`, `agent/update.go:43`), so it flows through unchanged.

**Rejected.** Hardcoding `scheme = "http"`; keeping `SetScheme` with the no-downgrade guard. The guard at `mother/api/middleware.go:46-55` is load-bearing today and actively hostile tomorrow: `New()` defaults `scheme: "https"` at :39, so deleting the setter without deleting the default leaves https baked into every install script.

### How the deployed agent fleet survives TLS removal **(owner sign-off required)**

**Choice.** Zero-touch: run a TLS-terminating reverse proxy on the same `https://<ip>:8443` the agents already hold, forward to the mother's plain-HTTP port, and set `FW_PUBLIC_URL` to that same https URL. Ship `deploy/migrate-agent-http.sh` (sed + restart) as the fallback.

**Why.** `shared/protocol/protocol.go:30-34` carries only Collectors/Interval/DesiredVersion — there is no channel to re-point MOTHER_URL — and `update_error` only rides a *successful* push, so a scheme-broken agent produces no panel signal at all. A freshly-installed one stays `pending` forever, indistinguishable from 'the one-liner was never run'. TLS still leaves the Go code, which is what the requirement asks.

**Rejected.** Re-running the install one-liner on each host: `install.sh.tmpl:25-31` rewrites agent.conf wholesale with only MOTHER_URL/TOKEN/SERVER_NAME, while `agent/config.go:70-77` reads eight more optional keys — every service collector on that host silently stops reporting.

### How ingest write volume is reduced **(owner sign-off required)**

**Choice.** Upsert `rollup_1m` and `rollup_1h` directly on ingest; delete the `samples` table and the 30-second rollup goroutine; make both rollup tables WITHOUT ROWID; store `sum` instead of `avg`; enable WAL.

**Why.** Raw `samples` has exactly one reader in the whole repo — the rollup that produces the rollups (`mother/store/samples.go:36`) — while `chart.go:12` floors interval at 60s, so the 10-second tier is unreachable from any API. Meanwhile the 30s tick REPLACEs ~10,000 rollup rows per tick at 50 servers of which ~9,500 are byte-identical rewrites, via two full index scans that hold the single connection (`store.go:74`) for ~0.45s. Measured: 719MB→244MB, ~32.4M→4.32M row-writes/day.

**Rejected.** Narrowing the rollup lookback: measured 0.16s at 10 minutes vs 0.25s at 70 — the cost is the full scan, not the window, because `WHERE ts >= ?` cannot seek an index whose leading column is server_id.

### Idempotency, which `cnt = cnt + 1` gives up

**Choice.** Guard the ingest write with a per-server timestamp monotonicity check: reject (200, no write) any push whose `ts` is not strictly greater than the server's recorded `last_push`.

**Why.** `samples.go:23-24` documents idempotency as a design property ('REPLACE makes it idempotent'), and with raw deleted there is no recompute path to repair a double-counted bucket. The guard costs one comparison against a column the handler already reads, and it also closes the gap between the 2s `minPushGap` and the 10s interval without ever returning a non-200.

**Rejected.** Deriving the rate limit from `settings.Interval`. An agent learns its interval ONLY from a 200 body (`agent/loop.go:110-112`); a 429 makes PushOnce bail at `ingest returned %d` before reading it, so raising interval 10→60 permanently 429s every deployed agent with no way to deliver the fix.

### Who queries GitHub for the release list

**Choice.** The mother, on a background ticker (`FW_RELEASE_POLL_INTERVAL`, default 5m), with ETag conditional requests into an immutable in-memory snapshot. `GET /api/version` and `rejectVersion` become pure in-memory reads.

**Why.** `availableBuilds()` runs on the request path twice per operator action (`versions.go:157` and `:214`), and `MonitoringPage.jsx:54-57` fires the version query unconditionally on every page mount. A live call per request would exhaust the unauthenticated 60 req/hour/IP budget in roughly thirty page loads — during exactly the incident when someone is trying to roll back. Conditional 304s are not billed, so a 5-minute poll costs ~12 requests/hour worst case.

**Rejected.** The panel calling GitHub directly (moves the update surface outside the permission model requirement 4 adds, and fails on an intranet panel); a hand-edited static version list (reintroduces the manual step this requirement removes) — though it survives as `FW_AGENT_VERSIONS`, an offline seed unioned into the snapshot.

### How already-deployed agents reach GitHub-hosted binaries **(owner sign-off required)**

**Choice.** Keep `GET /download/agent/{version}` and default it to `FW_DOWNLOAD_MODE=proxy` during the migration window — the mother streams the GitHub asset through itself with an on-disk cache — switching to `redirect` only once no `servers.agent_version` predates the first GitHub-sourced release.

**Why.** A 302 is the obvious answer and is wrong for the existing fleet, twice over. A field agent installed against a self-signed mother has `TLS_SKIP_VERIFY=true` in agent.conf, and Go's default client follows the redirect on that same InsecureSkipVerify transport — so binary AND checksum arrive from one unverified peer, which is the exact attack this workstream exists to close. Separately `agent/update.go:28` sets a 60s whole-request timeout for a 15.6MB binary; adding a WAN redirect hop means anything under ~2.1 Mbps can never complete, and `agent/loop.go:26` retries every 5 minutes forever. Both fixes ship *inside* the download that cannot complete.

**Rejected.** A bare 302 shim (unfixably unsafe for pre-change hosts); deleting the route (bricks every deployed agent permanently, including for the update that would teach it about GitHub).

### Server-group membership shape **(owner sign-off required)**

**Choice.** Many-to-many via a `server_group_members` join table, with memberships purged explicitly inside `DeleteServer`'s existing transaction — no declared foreign keys.

**Why.** The requirement names filtering AND bulk targeting, which is where independent axes (env × role × region) pay off; exclusivity remains addable later as a `UNIQUE(server_id)` index, whereas the reverse is a data migration. FKs are omitted because they would be inert: no `PRAGMA foreign_keys` exists anywhere in the mother, verified empirically against the modernc driver, and `servers.go:207-209` already documents that SQLite reuses ids — an orphan membership row would enroll a brand-new server into a dead server's group and hand it that group's bulk version target.

**Rejected.** A `group_id` column on `servers` (forces the operator to pick one axis and re-tag the fleet when a second is needed); declaring `ON DELETE CASCADE` (unenforced on this driver, and implies protection that does not exist).

### Where the update permission is enforced

**Choice.** Entirely in the absent backend proxy: `system:agent_update` (requires `system:health`) gates every `/admin/monitoring/*` write route. The mother gets nothing; the panel hides the buttons via a new shared `useCan()`.

**Why.** The mother authenticates with one shared `X-API-Key` (`middleware.go:88-96`) and has no caller identity, no user and no role. A second key would be checked by the same backend that already decides.

**Rejected.** A second key or a role concept in the mother (theater — enforced by the same component that already made the decision); a panel-only gate (that is hiding, not enforcing: anyone holding the shared X-API-Key can PUT the version endpoint directly); reusing the `groups:*` namespace (already owned by user permission groups).

### Bulk group rollout on a mixed-platform group

**Choice.** Whole-request 400 for version-level faults (`latest`, unstaged); per-server skip-and-report for platform faults. HTTP 200 with `{version, applied[], skipped[{id,name,reason}]}` — never `success:false`, since writes landed.

**Why.** Whole-request failures are properties of the VERSION; per-server failures are properties of the PLATFORM. `writeJSON` (`admin.go:14-20`) hardcodes `success == (errMsg == "")` and has no partial field, so the partition must live inside `data`.

**Rejected.** Refusing the whole request on any per-server platform mismatch — one darwin/arm64 laptop would then permanently block a 40-host linux rollout. Returning `success:false` on a partial — the panel's apiErrorMessage path would tell the operator nothing happened while half the group is already converging.

### The downgrade comparator

**Choice.** A new panel-side `src/lib/versionOrder.js` that returns `unknown` (⇒ guarded confirm) whenever it cannot rank two versions, deliberately diverging from the mother's `naturalLess`.

**Why.** `naturalLess` falls through to `len(a) < len(b)` at `versions.go:141`, so it ranks `v1.3.0-rc1` ABOVE `v1.3.0` — verified by running it. On the mother that is a cosmetic dropdown wart; in the panel the same rule would classify the rc→release rollback as an upgrade and wave it through. Here the ordering drives a safety decision, so refusing to guess is correct.

**Rejected.** Porting `naturalLess` to JS (imports the inversion into a safety decision); normalising `current` from string equality to the new comparator — the mother (`admin.go:57`) and agent (`agent/loop.go:125`) both compare raw strings, so normalising only the panel would DISABLE the option that lets a host reporting `1.4.0` self-heal onto `v1.4.0`, with no other correction path.

### How uninstall reaches a host

**Choice.** The installer writes the uninstaller to `/usr/local/sbin/feast-watch-agent-uninstall` plus an `/etc/feast-watch/install-manifest`; a network fallback is served unauthenticated at `GET /uninstall.sh`. No token-served `/uninstall/{token}.sh`.

**Why.** A token-served script must resolve through `ServerByToken` (`install.go:24`) and 404s the moment the operator deletes the server — and a decommissioned host may not route to the mother at all, or the mother may already be retired. The manifest makes a v1.3-installed host cleanable by a v2.0 uninstaller.

**Rejected.** `/uninstall/{token}.sh` mirroring the install route: carries a token in a URL for no capability the on-disk script lacks (it already reads the token from agent.conf) while inheriting the 404-after-delete trap.

### Stopping an agent whose server was removed

**Choice.** Add `decommissioned_at` as a soft terminal state; ingest answers a decommissioned server with 410 Gone; new agents treat 410 as terminal via a typed sentinel error, and the installed unit gains `RestartPreventExitStatus=`.

**Why.** Today `DeleteServer` destroys every sample and both rollup tiers for the host — precisely when the post-mortem is most wanted — while the agent keeps POSTing every 10s forever (`agent/loop.go:120-134`, no backoff). A soft state gives a third option and a terminal panel status instead of a permanently-red row.

**Rejected.** Returning from Run on 410 alone: `Restart=always`/`RestartSec=5` restarts on clean exits too, so the agent would respawn every 5s and push MORE often than the 10s interval it was meant to stop. Also rejected: a hard `DELETE` from the token-authenticated self-service route, which would upgrade token possession into irreversible destruction of that host's entire history.

## Verified problems in the existing code

Every item below was found by one agent and confirmed against the code by a second.

| # | Severity | Problem |
|---|---|---|
| 1 | CRITICAL | A settings PUT that omits a retention field silently deletes that entire tier |
| 2 | CRITICAL | The install script writes a 404 error page to /usr/local/bin/feast-watch-agent and chmods it 0755 |
| 3 | CRITICAL | There is no channel to re-point an installed agent, and a broken one is invisible in the panel |
| 4 | HIGH | The 30-second rollup rewrites ~9,500 already-correct rows per tick through two full index scans |
| 5 | HIGH | The agent's update client inherits the mother's relaxed TLS settings |
| 6 | HIGH | Deleting a server never stops its agent |
| 7 | HIGH | api.New hardcodes scheme: "https" and SetScheme coerces anything else to https |
| 8 | MEDIUM | No PRAGMA foreign_keys anywhere, combined with documented SQLite id reuse |
| 9 | MEDIUM | deploy/install.sh.tmpl is dead, diverged, and is the file someone will edit by mistake |
| 10 | MEDIUM | The agent reads a 15.6MB binary fully into memory under a 60-second whole-request timeout |
| 11 | MEDIUM | knownPlatforms (6) and release.sh PLATFORMS (4) have already drifted, with a comment as the only enforcement |
| 12 | MEDIUM | The mother has no unit, no installer and no uninstaller anywhere in the repo |
| 13 | MEDIUM | There is no CI, and .github/ is untracked |
| 14 | LOW | naturalLess ranks a release candidate above the final release |
| 15 | LOW | A crashed self-update strands <binary>.new, and the write is not durable |

### 1. [CRITICAL] A settings PUT that omits a retention field silently deletes that entire tier

handlePutSettings decodes into a value-type store.Settings and validates only Interval and HeartbeatMissThreshold. Any omitted retention key decodes as 0, SaveSettings writes "0", and the next hourly EnforceRetention computes cutoff = now - 0 and issues DELETE FROM rollup_1m WHERE window_start < now — fifteen days of history for every server, gone. GetSettings compounds it by discarding the strconv.Atoi error, so a corrupt or non-numeric row also reads as 0. Today the 48-hour raw tier gives a window in which the tier could in principle be rebuilt; under the recommended write-volume package there is no raw tier, so this becomes unrecoverable. The panel is not the trigger — it already sends the full key set — which means the exposure is any direct or scripted caller.

*Evidence:* mother/api/admin.go:175-196 (only Interval validated at :183-186); mother/store/settings.go:35 (`n, _ := strconv.Atoi(v)`); mother/store/samples.go:56-58 (now - retention cutoffs)

*Fix:* Decode into a per-field pointer shadow struct, reject a body missing any retention key with 400, validate ranges against a MaxRetentionDays const, and stop swallowing the Atoi error. Ship this FIRST, independently of the rest of the write-volume work.

### 2. [CRITICAL] The install script writes a 404 error page to /usr/local/bin/feast-watch-agent and chmods it 0755

Neither install template passes --fail to curl. `curl -sSL -o <path>` on an HTTP 404 exits 0 and writes the response body, and http.ServeFile returns the literal text '404 page not found'. So the script does NOT abort under set -euo pipefail: it chmods a 19-byte text file to 0755, writes agent.conf, installs the unit and enables it, producing a systemd respawn loop every 5 seconds against a non-executable 'binary'. This is live: the mother's downloads/ on this machine holds feast-watch-agent-latest-amd64 and -latest-windows-amd64 but NOT feast-watch-agent-latest-linux-amd64, which is exactly the name line 20 requests.

*Evidence:* mother/api/install.sh.tmpl:20-21 (`curl -sSLk "$MOTHER_URL/download/agent/latest-linux-$ARCH" -o /usr/local/bin/feast-watch-agent` then chmod 0755); mother/api/install.go:49 (http.ServeFile); deploy/feast-watch-agent.service:7-8 (Restart=always, RestartSec=5); `ls downloads/`

*Fix:* Add --fail and keep it through every later rewrite of that line, plus sha256 verification before install. Until then, treat 're-run the install one-liner' as an unavailable recovery path — three later workstreams lean on it.

### 3. [CRITICAL] There is no channel to re-point an installed agent, and a broken one is invisible in the panel

IngestResponse carries only Collectors, Interval and DesiredVersion. LoadConfig runs exactly once at startup. update_error rides only a push that itself SUCCEEDS. So an agent whose MOTHER_URL scheme no longer matches what the mother serves dies at the transport and reports nothing. Worse than 'goes offline': status() has no offline state — pending when LastPush == 0, down past the heartbeat threshold, else online. A freshly-installed host that never completes a first push stays `pending` forever, indistinguishable from 'the install one-liner was never run'. Re-running the installer is not a remedy either: install.sh.tmpl:25-31 rewrites agent.conf wholesale with only MOTHER_URL/TOKEN/SERVER_NAME while config.go:70-77 reads eight more optional keys, so every service collector on that host silently stops.

*Evidence:* shared/protocol/protocol.go:30-34; agent/loop.go:62-68 and :120-134; agent/cmd/feast-watch-agent/main.go:18 (LoadConfig called once); mother/api/admin.go:66-74; mother/api/install.sh.tmpl:25-31 vs agent/config.go:70-77

*Fix:* Front the mother with a TLS-terminating proxy on the same https://<ip>:8443 the agents already hold. Ship deploy/migrate-agent-http.sh as the fallback, and patch the k8s Secret plus roll the DaemonSet separately — install.sh does not reach those agents at all.

### 4. [HIGH] The 30-second rollup rewrites ~9,500 already-correct rows per tick through two full index scans

The lookback is hour-aligned, so the window is 10-70 minutes wide (mean 39.5), not the 10 the code comment implies. At 50 servers that is ~10,000 rollup_1m rows REPLACEd per tick against ~500 that can genuinely have changed — 28.8M REPLACEs/day, ~95% waste, and on a rowid table with a composite PK each REPLACE is DELETE+INSERT rewriting both the table b-tree and the autoindex. Neither query can seek: idx_samples leads with server_id and rollup_1m's PK leads with server_id, so `WHERE ts >= ?` and `WHERE window_start >= ?` both degrade to full scans plus a temp b-tree for the GROUP BY. Measured ~0.46s per tick, and with SetMaxOpenConns(1) that exclusively owns the database, stalling every push for its duration. Narrowing the lookback does not help — measured 0.16s at 10 minutes vs 0.25s at 70.

*Evidence:* mother/store/samples.go:32-46; mother/cmd/feast-watch/main.go:63-69; mother/store/store.go:47-61 and :74; EXPLAIN QUERY PLAN on mother.db → 'SCAN samples USING INDEX idx_samples' plus 'USE TEMP B-TREE FOR GROUP BY'

*Fix:* Delete the goroutine and upsert both tiers on ingest; make both rollup tables WITHOUT ROWID (measured 394.0MB → 225.6MB at 5.4M rows).

### 5. [HIGH] The agent's update client inherits the mother's relaxed TLS settings

updateClient is built by cfg.HTTPClient(60s), the same constructor that sets InsecureSkipVerify: true whenever TLS_SKIP_VERIFY=true is in agent.conf — and the served installer writes exactly that key whenever the mother runs a self-signed cert. Point that client at github.com and both the binary and its .sha256 arrive over an unverified connection from the same peer, so a matching tampered pair is trivially servable. This is not hypothetical after the GitHub move: the obvious 302 compatibility shim is what would expose it, and there is no remote remedy because the fix ships inside the binary the compromised path is fetching.

*Evidence:* agent/cmd/feast-watch-agent/main.go:49-53 then :57; agent/config.go:119-121; mother/api/install.sh.tmpl:29

*Fix:* Build the update client on a cloned http.DefaultTransport (system trust store only) in the same change that removes the TLS fields, and default FW_DOWNLOAD_MODE=proxy so deployed agents never talk to github.com on a broken transport.

### 6. [HIGH] Deleting a server never stops its agent

DeleteServer removes the row so ServerByToken stops resolving and every push gets 401 — but Run treats every error identically, logging and retrying on the next tick with no backoff, forever. There is nothing in the protocol that can say 'stop'. Note the rate-limiter framing is a red herring: allowPush is keyed by srv.ID, so a revoked token has no entry to bypass, and the 401 path (one indexed SELECT, no body read) is strictly CHEAPER than the 429 path. The real defect is the missing terminal signal and the total absence of backoff, which applies to genuine network errors too.

*Evidence:* mother/store/servers.go:194-217; agent/loop.go:95-97 and :120-134; mother/api/ingest.go:18-27 vs :37-42

*Fix:* Add a soft decommissioned state, answer 410 Gone on ingest, teach new agents to stop via a typed sentinel error, and add RestartPreventExitStatus= to the emitted unit. Accept that this reaches only newly-installed agents.

### 7. [HIGH] api.New hardcodes scheme: "https" and SetScheme coerces anything else to https

The no-downgrade guard is load-bearing today and actively hostile tomorrow. If a refactor removes SetScheme but leaves the New default, every API constructed without an explicit setter — which is every test, and any future caller — still renders https into install.sh, and the resulting agents die silently.

*Evidence:* mother/api/middleware.go:39 and :46-55

*Fix:* Delete the field and the default in the same commit, and pin it with a test asserting a freshly-constructed API's publicURL is http-schemed.

### 8. [MEDIUM] No PRAGMA foreign_keys anywhere, combined with documented SQLite id reuse

Verified empirically against the modernc driver through store.Open: PRAGMA foreign_keys reads 0 and a child row with REFERENCES … ON DELETE CASCADE survived deletion of its parent. So any FK declared on a future group-membership table would be inert. That is not merely untidy: servers.id is an INTEGER PRIMARY KEY without AUTOINCREMENT and the code itself documents that SQLite may reuse a deleted id — which is why DeleteServer already hand-purges both rollup tables. An orphan membership row would silently enroll a brand-new server into a dead server's group and sweep it into that group's next bulk version rollout.

*Evidence:* mother/store/store.go:68-84 (no PRAGMA anywhere in the repo); empirical probe against store.Open; mother/store/servers.go:194-217 with the id-reuse rationale at :207-209

*Fix:* Purge memberships explicitly inside DeleteServer's transaction, and do NOT declare foreign keys that imply protection you do not have.

### 9. [MEDIUM] deploy/install.sh.tmpl is dead, diverged, and is the file someone will edit by mistake

mother/api/install.go:12 is the only go:embed in the tree, so the deploy/ copy is never rendered. It fetches feast-watch-agent-latest-$ARCH, a name bin/release.sh never stages (it stages latest-$platform at :68, and the bare-arch alias at :76-80 only for $VERSION), so against a release.sh-staged directory it 404s — and combined with the missing --fail, it installs a text file. Git confirms the drift: its history stops at 441c824 while the live template continued through 61adc6a and 60be630. A developer told to 'drop -k from the installer' has a coin-flip chance of editing the file nothing reads. This round rewrites the installer four separate times.

*Evidence:* mother/api/install.go:12-13; deploy/install.sh.tmpl:17 vs mother/api/install.sh.tmpl:20 vs bin/release.sh:60-80; `git log --oneline -- deploy/install.sh.tmpl`

*Fix:* Delete it, note the retirement in the six committed doc references that point at it (docs/superpowers/plans/2026-07-16-feast-watch.md:57,:2529,:2534,:2629,:2733,:2785), and add a test failing if any *.sh.tmpl reappears outside mother/api/.

### 10. [MEDIUM] The agent reads a 15.6MB binary fully into memory under a 60-second whole-request timeout

io.ReadAll(io.LimitReader(body, 256MB+1)) holds the entire download in RAM before writing — on a 512MB monitored VPS that is an OOM that kills the agent being updated. Separately Client.Timeout covers connect+headers+body, and the real artifacts are 15,587,667 bytes (linux/amd64), so anything slower than ~2.1 Mbps sustained can never complete; the attempt fails, tryUpdate backs off 5 minutes, and the host re-downloads most of a binary forever. Adding GitHub's redirect hop to objects.githubusercontent.com tightens it further, and for already-deployed agents the fix is inside the download that cannot complete.

*Evidence:* agent/update.go:15-18, :28, :51-58; agent/loop.go:26; `ls -la downloads/`

*Fix:* Stream through a TeeReader into a temp file with per-request contexts, and keep FW_DOWNLOAD_MODE=proxy during migration so deployed agents fetch over a LAN hop.

### 11. [MEDIUM] knownPlatforms (6) and release.sh PLATFORMS (4) have already drifted, with a comment as the only enforcement

darwin-amd64 and windows-arm64 are parseable by the mother but never built. Harmless today because availableBuilds only sees files that exist — but the moment the index is built from a remote asset list, rejectVersion could approve a target whose asset was never uploaded and the agent 404s in a loop. The comment at bin/release.sh:21-23 saying they must stay in sync is the tell that they cannot.

*Evidence:* mother/api/versions.go:32-36 vs bin/release.sh:24-29

*Fix:* One shared/release.Platforms list consumed by the agent, the mother and the CI matrix.

### 12. [MEDIUM] The mother has no unit, no installer and no uninstaller anywhere in the repo

deploy/ holds only feast-watch-agent.service, install.sh.tmpl and k8s/. QUICKSTART:39-40 hands the entire deployment to the operator as 'run it with these env vars'. Consequently the footprint is whatever each operator improvised: main.go:23 and :56 default to /var/lib/feast-watch/{mother.db,downloads} but store.Open never creates that directory, so somebody made it by hand with unknown ownership; bin/release.sh:51 produces bin/feast-watch with no defined destination; FW_TLS_CERT/KEY point at arbitrary paths. You cannot write a clean uninstaller for an install that was never defined.

*Evidence:* `ls deploy/`; QUICKSTART.md:39-40; mother/cmd/feast-watch/main.go:23,:34,:56; mother/store/store.go:68-74; bin/release.sh:51

*Fix:* Build the install side first (unit with StateDirectory=, installer, manifest), then mirror it.

### 13. [MEDIUM] There is no CI, and .github/ is untracked

`git status` shows `?? .github/`, holding only an uncommitted dependabot.yml, and there is no workflows directory. Every TDD and red-green argument in this plan assumes someone runs go test and npm test by hand. It also gates the release workflow: an `on: push: tags` workflow only runs if the file is in the tagged commit's tree, so the first v* tag would silently produce no release and leave the mother's index empty. `main` is also 46 commits behind `dev`.

*Evidence:* `git status --porcelain`; `git ls-files .github` → empty; `git branch -vv`

*Fix:* Commit .github/ and add go test plus shellcheck workflows in the first workstream, before anything depends on them.

### 14. [LOW] naturalLess ranks a release candidate above the final release

naturalLess("v1.3.0","v1.3.0-rc1") returns true — verified by running it — because it walks equal chunks until a is empty and then falls through to `len(a) < len(b)`. Since the descending sort calls naturalLess(builds[j], builds[i]), v1.3.0-rc1 sorts above v1.3.0. On the mother that is a cosmetic dropdown wart with no test pinning it either way. Ported into the panel's downgrade comparator it would classify the rc→release rollback as an upgrade and wave it through the guard.

*Evidence:* mother/api/versions.go:127-142 (the fall-through at :141); mother/api/versions_test.go:75-115 covers only v1.10.0 vs v1.9.0, checksums and the latest alias

*Fix:* The panel comparator returns 'unknown' (⇒ guarded confirm) whenever cores are equal but suffixes differ, with the divergence commented so nobody 'fixes' it to match Go.

### 15. [LOW] A crashed self-update strands <binary>.new, and the write is not durable

os.WriteFile then os.Rename, with cleanup only on rename failure. A SIGKILL, OOM or power loss between them leaves the file behind permanently — nothing sweeps it. There is no fsync, so a power loss after the rename can leave a truncated or zero-length file AS the agent binary, which systemd then respawn-loops against every 5 seconds.

*Evidence:* agent/update.go:82-89; deploy/feast-watch-agent.service:7-8

*Fix:* CreateTemp + Chmod + Sync + Rename in the GitHub-releases rewrite, and an explicit <binary>.new sweep in the uninstaller.

## Workstreams

Ordered so that the four most-contended files are edited by one workstream at a time.

| # | Workstream | Repo | Depends on |
|---|---|---|---|
| 1 | Installer hygiene and CI floor | feast-watch | — |
| 2 | Backend-proxy contract (written, not code) | backend-proxy(absent) | Installer hygiene and CI floor |
| 3 | TLS removal | both | Installer hygiene and CI floor, Backend-proxy contract (written, not code) |
| 4 | Ingest write-volume | both | TLS removal |
| 5 | Agent binaries from GitHub Releases | feast-watch | TLS removal, Ingest write-volume |
| 6 | Server groups and the update permission | both | Ingest write-volume, Agent binaries from GitHub Releases, Backend-proxy contract (written, not code) |
| 7 | Downgrade guard in the update dialog | panel | Server groups and the update permission, Agent binaries from GitHub Releases |
| 8 | Clean uninstall and server decommissioning | both | Agent binaries from GitHub Releases, Server groups and the update permission |

### W1 — Installer hygiene and CI floor

*Repo:* `feast-watch`  
*Depends on:* —

**Goal.** Make the one served installer correct and truncation-safe, delete its dead twin, and get a committed CI that runs go test and shellcheck — so every later workstream edits a single trustworthy template with a gate behind it.

**Steps.**

- **modify** `mother/api/install.sh.tmpl`
  Line 20: add `--fail` to the curl invocation. Today a 404 exits 0 and writes the body ('404 page not found') to /usr/local/bin/feast-watch-agent, which line 21 then chmods 0755; the unit is enabled and crash-loops every 5s. Wrap the whole body (lines 5-50) in `main() { … }` with `main "$@"` as the final line, so a connection dropped mid-`curl | sudo bash` defines a function and exits instead of executing a prefix.
- **delete** `deploy/install.sh.tmpl`
  Remove outright. Nothing embeds it — `mother/api/install.go:12` is the only go:embed in the tree — and it has diverged: :17 fetches `feast-watch-agent-latest-$ARCH`, a name `bin/release.sh` never stages (it stages `latest-$platform` at :68, and the bare-arch alias at :76-80 only for `$VERSION`). Its history stops at 441c824 while the live template continued through 61adc6a and 60be630.
- **modify** `docs/superpowers/plans/2026-07-16-feast-watch.md`
  Add a one-line note at :57 and :2733 that deploy/install.sh.tmpl was retired and mother/api/install.sh.tmpl is canonical. Six references in this committed plan point at the deleted file, including a `cp deploy/install.sh.tmpl mother/api/install.sh.tmpl` step that documents deploy/ as the intended source.
- **modify** `.github/`
  Commit the directory. It is currently UNTRACKED (`git status` → `?? .github/`) and holds only dependabot.yml, so no workflow can ever fire until it is in the tree. Note `main` is 46 commits behind `dev`.
- **add** `.github/workflows/ci.yml`
  `on: [push, pull_request]`. Jobs: `go vet ./...` plus `go test -race -cover ./...` with a coverage floor check. The panel repo gets its own workflow file running `npm ci && npm test`.
- **add** `.github/workflows/shellcheck.yml`
  shellcheck over `bin/release.sh`, `e2e/*.sh`, `deploy/*.sh`. For `mother/api/install.sh.tmpl`, render it through the Go template with placeholder data in a tiny test binary first — raw `{{.Token}}` is not valid shell. Shell is now load-bearing product code and an unquoted variable in a later `rm -rf` line is a destroyed host.

**Tests (written first).**

- mother/api/install_test.go — new TestInstallScriptFailsOnDownloadError asserting the rendered body carries `--fail` (or `-f`) on the curl line; new TestInstallScriptIsTruncationSafe asserting the last non-empty line is exactly `main "$@"`.
- mother/api/install_test.go — new TestNoOrphanShellTemplates walking the repo and failing if any `*.sh.tmpl` exists outside mother/api/, so the duplicate cannot come back.
- Manual: render the template, run it in a container against a mother whose downloads dir is empty, and assert the script exits non-zero WITHOUT having written /usr/local/bin/feast-watch-agent or enabling the unit.

**Risk.** Low. The `--fail` change makes a currently-silent failure loud, which may surface hosts installed with a garbage binary that have been crash-looping unnoticed — that is the point, but expect the first run after this lands to reveal broken installs rather than create them.

### W2 — Backend-proxy contract (written, not code)

*Repo:* `backend-proxy(absent)`  
*Depends on:* Installer hygiene and CI floor

**Goal.** Write down every change the third repo must make, clause by clause, each tagged with the workstream whose deploy window it must land in. No code is written here because that repository is not on this machine.

**Steps.**

- **add** `docs/superpowers/specs/2026-08-18-backend-proxy-contract.md`
  CLAUSE 1 (ships with W3, TLS): `MONITORING_API_URL` changes from `https://<mother>:8443` to the plain-HTTP mother URL or the fronting proxy's URL, and `MONITORING_TLS_SKIP_VERIFY` is dropped. Out of sync in either direction the panel renders the 502 'Mother sunucusuna ulaşılamıyor' state (MonitoringErrorCard.jsx:27-43) and the operator hunts the mother rather than an env var. Note the proxy runs on the same box as the mother per docs/2026-07-27:222.
- **modify** `docs/superpowers/specs/2026-08-18-backend-proxy-contract.md`
  CLAUSE 2 (ships with W4, write-volume): `retention_raw_hours` disappears from the settings GET/PUT payload, and the mother now rejects a PUT missing any retention key with 400. If the proxy whitelists settings fields rather than forwarding verbatim it must drop that key; confirm which. Update the settings contract table at docs/2026-07-27-monitoring-panel-design.md:31 in the same commit.
- **modify** `docs/superpowers/specs/2026-08-18-backend-proxy-contract.md`
  CLAUSE 3 (ships with W5, GitHub Releases): `GET /admin/monitoring/version` gains additive optional fields `source`, `checked_at`, `stale`. IMPORTANT observed fact that every clause below depends on: the proxy UNWRAPS the mother's `{success,data,error}` envelope down to `data` — `src/api/monitoring.test.js:34-45` and `:126-132` show the panel receiving bare arrays, `{desired_version}`, and a literal `null` on delete. Every new contract here is specified in UNWRAPPED form, despite the 'forwards verbatim' comment at src/api/monitoring.js:5-6.
- **modify** `docs/superpowers/specs/2026-08-18-backend-proxy-contract.md`
  CLAUSE 4 (ships with W6): add `{ key: "system:agent_update", label: "Agent sürüm güncelleme", requires: "system:health" }` to the `GET /admin/permissions` catalogue under the existing `system` domain — `requires` is mandatory and is consumed by `src/lib/permissionRules.js:10-18`. It must also appear in `GET /admin/me`'s `permissions[]`. NAMESPACE: this must NOT be `groups:*` and these routes must NOT mount under `/admin/groups`; that surface is already user permission-groups CRUD (`src/api/groups.js`, keys `groups:list|create|update|delete`). Server groups live only under `/admin/monitoring/groups`.
- **modify** `docs/superpowers/specs/2026-08-18-backend-proxy-contract.md`
  CLAUSE 5 (ships with W6): split the current blanket `PermSystemHealth` mount on `/admin/monitoring/*`. READS keep `system:health` alone: GET servers (incl. the new `?group_id=`), GET groups, GET version, GET chart, GET settings. WRITES require `system:health` AND `system:agent_update`: PUT servers/{id}/version, PUT groups/{id}/version, POST/PATCH/DELETE groups, PUT groups/{id}/servers, and (W8) PUT servers/{id}/decommission. New passthroughs forward verbatim to the mother with the shared X-API-Key, preserving the mother's 400/409 bodies (they are shown to the operator). `group_id` on GET servers MUST be forwarded — dropping it silently returns the whole fleet and the filter looks broken. A 403 must use the standard error body `apiErrorMessage` already parses.
- **modify** `docs/superpowers/specs/2026-08-18-backend-proxy-contract.md`
  CLAUSE 6 (ships with W6): `PUT /admin/monitoring/groups/{id}/version` returns HTTP 200 with the unwrapped body `{version, applied:[{id,name}], skipped:[{id,name,reason}]}`. A non-empty `skipped[]` must NOT be rewritten into an error status — writes landed and the panel renders it as a partial outcome. CLAUSE 7 (ships with W8): server `status` gains the terminal value `decommissioned`, and `GET /admin/monitoring/servers` items gain `groups: [{id,name}]`. CLAUSE 8: nothing changes in the mother's auth — it still takes one shared X-API-Key and has no user concept; the permission is enforced entirely at the proxy.

**Tests (written first).**

- Backend-side acceptance for that repo's owner: a caller holding system:health but not system:agent_update receives 403 on every write route and 200 on every read route; the permissions catalogue contains the new key with its `requires`; `?group_id=` survives the forward; a 200 with non-empty skipped[] is not rewritten into an error.
- Panel-side coverage already exists for the failure surface clause 1 protects against: MonitoringPage.test.jsx:137-142 asserts the unreachable-mother state.

**Risk.** This workstream produces a document, not code, so nothing here is verifiable by CI. The real risk is scheduling: clause 1 must deploy in the same window as W3 or monitoring goes dark, and the owner of that repo is currently unknown (see open questions).

### W3 — TLS removal

*Repo:* `both`  
*Depends on:* Installer hygiene and CI floor, Backend-proxy contract (written, not code)

**Goal.** Remove TLS from the mother and the agent↔mother link entirely, replacing scheme+publicAddr with one validated FW_PUBLIC_URL, without blinding the deployed fleet.

**Steps.**

- **add** `mother/publicurl.go`
  `func PublicURL(raw, legacyAddr string) (string, error)`. Both empty → the default const `http://127.0.0.1:8443`; legacyAddr only → `"http://"+legacyAddr` plus a deprecation slog.Warn; otherwise url.Parse, require scheme in {http,https}, require non-empty Host, reject RawQuery/Fragment, strip one trailing slash. Extracted from main because package main has no test file.
- **modify** `mother/api/middleware.go`
  Delete the `scheme` field (:21-25), `agentTLSSkipVerify` (:26-30), `SetScheme` (:46-55) and `SetAgentTLSSkipVerify` (:57). Rename `publicAddr`→`publicURL` (:20) and change the New default at :39 from `publicAddr: "127.0.0.1:8443", scheme: "https"` to `publicURL: "http://127.0.0.1:8443"` — the default MUST die in the same commit or every API built without a setter still emits https. Rename `SetPublicAddr`→`SetPublicURL`; keep it a setter, do not change `New`'s signature (the shared `setup(t)` at ingest_test.go:14 is used by five test files).
- **modify** `mother/api/install.go`
  :31 becomes `"MotherURL": a.publicURL` (a pass-through, not a concatenation); delete the `"TLSSkipVerify"` map entry at :34.
- **modify** `mother/api/install.sh.tmpl`
  :20 `curl -sSLkf` → `curl -sSLf` (the `-k` goes; `-f` stays from W1). Delete the `{{if .TLSSkipVerify}}TLS_SKIP_VERIFY=true` / `{{end}}` wrapper at :29-30 so the heredoc closes on a plain `EOF`. Keeping `-k` after a real-cert proxy is fronted would silently accept a MITM on the one fetch that becomes an executable.
- **modify** `mother/api/admin.go`
  `InstallCommand(publicURL, token string) string` — drop the scheme parameter and the `-k`, emit `curl -sSL %s/install/%s.sh | sudo bash`. Update the doc comment at :223 (it currently explains why scheme must match what the mother serves). The signature change is the forcing function that makes the compiler find every caller.
- **modify** `mother/generate.go`
  :18 `RunGenerate(st, publicURL string, args []string)`; :35 `api.InstallCommand(publicURL, srv.Token)`. Drop the 'scheme mirrors what the mother serves' clause from the doc comment.
- **modify** `mother/cmd/feast-watch/main.go`
  Delete :32-38 (cert/key read + scheme derivation), :58 SetScheme, :59-61 SetAgentTLSSkipVerify, the `"tls", cert != ""` field at :83, and the ListenAndServeTLS branch :84-88. Replace :30 with `publicURL, err := mother.PublicURL(os.Getenv("FW_PUBLIC_URL"), os.Getenv("FW_PUBLIC_ADDR"))` and fail fast, matching the FW_API_KEY check at :51-55.
- **add** `agent/motherurl.go`
  `func validateMotherURL(raw string) error` — url.Parse, require scheme http|https, require non-empty Host. Called from LoadConfig's validation block at config.go:86-94. Today a conf holding a bare `10.0.0.1:8443` starts cleanly and then fails every push forever, visible only in the host's journal — and MOTHER_URL is exactly the line operators hand-edit during this migration.
- **modify** `agent/config.go`
  Delete the crypto/tls and crypto/x509 imports (:6-7), the CAFile/TLSSkipVerify fields (:29-37), the CA_FILE/TLS_SKIP_VERIFY reads (:76-77) and the whole HTTPClient method (:98-123) — with tls.Config gone it is an `&http.Client{Timeout: t}` wrapper carrying an error that can never be non-nil. CRITICAL: leave the map-based key reading at :49-61 exactly as is. Unknown-key tolerance is the migration guarantee — every installed agent.conf still contains TLS_SKIP_VERIFY=true and must keep loading.
- **modify** `agent/cmd/feast-watch-agent/main.go`
  Replace :44-53 with two plain clients. Do NOT declare new timeout constants here: `agent.NewLoop` (loop.go:44) and `agent.SelfUpdate` (update.go:27) already hardcode the same 5s/60s, and SelfUpdate currently has no caller at all — call those wrappers, or move the constants into package agent. A second source of truth for the timeouts violates the no-hardcoded-values rule.
- **modify** `agent/collectors/k8s.go`
  NO CHANGE — explicitly out of scope, listed so a grep-driven sweep does not touch it. :5 imports crypto/tls and :23 sets InsecureSkipVerify for the in-cluster Kubernetes API server, not the mother. Also note the agent must keep speaking https outbound for W5 (GitHub is https-only): 'remove TLS' scopes to the agent↔mother link.
- **add** `deploy/migrate-agent-http.sh`
  Fallback migration for fleets not fronting a proxy: `sed -i 's|^MOTHER_URL=https://|MOTHER_URL=http://|' /etc/feast-watch/agent.conf && systemctl restart feast-watch-agent`, idempotent and safe to re-run. Document the k8s variant separately — DaemonSet agents get agent.conf from the `feast-watch-agent-conf` Secret (deploy/k8s/daemonset.yaml:26-27), so there is no file to sed and no unit to restart; the Secret must be patched and the DaemonSet rolled.
- **modify** `.env.example / QUICKSTART.md / README.md / docs/superpowers/specs/2026-07-16-feast-watch-design.md`
  .env.example: delete FW_TLS_CERT/KEY (:6-7) and the FW_AGENT_TLS_SKIP_VERIFY block (:8-11), replace FW_PUBLIC_ADDR with FW_PUBLIC_URL, and warn on :2 that FW_LISTEN now binds a plaintext listener. QUICKSTART:51,:68 https→http plus a TLS section and a migration subsection. README:14 and design doc :26 `HTTPS push`→`HTTP push`; design doc :115-117 drop the `-k` parenthetical; :172 rewrite the TLS line. Add docs/superpowers/specs/2026-08-18-tls-removal-design.md and a 'Superseded by' pointer at 2026-07-27:187 rather than rewriting that section — it records a decision being reversed.
- **modify** `downloads/AGENT-KURULUM.md + downloads/feast-watch-agent-kurulum.md`
  Manual local action, flagged not committed: both are gitignored (.gitignore:2) and byte-identical, and the SECOND one is what the mother actually serves at /download/agent/kurulum.md (install.go:44 builds the name as `feast-watch-agent-`+version). Regenerate or delete both, or the removed configuration keeps being taught to every new operator over HTTP.
- **modify** `/Users/ceydaakin/GitHub/feast-mobile-backend-control/src/features/monitoring/AddServerDialog.test.jsx`
  :12 fixture `'curl -sSLk https://10.0.0.1:8443/install/tk_secret.sh | sudo bash'` → `'curl -sSL http://10.0.0.1:8443/install/tk_secret.sh | sudo bash'`. It is a hardcoded string with no coupling to the mother, so it keeps passing while asserting a command form that no longer exists — stale-green is worse than red. No production panel code changes.

**Tests (written first).**

- mother/publicurl_test.go (NEW, write first, table-driven): empty→default; legacy addr→http:// prefix + warning; bare `10.0.0.1:8443`→error; `ftp://x`→error; trailing slash stripped; `https://ops.feast.tr/watch/`→`https://ops.feast.tr/watch`; empty host→error.
- agent/motherurl_test.go (NEW): valid http, valid https, no scheme→error, empty host→error, `ftp://`→error.
- mother/api/install_test.go: :26 expect `MOTHER_URL=http://10.0.0.1:8443`; DELETE TestInstallScriptRendersSchemeFromMother (:81-93), TestInstallScriptEmitsTLSSkipVerify (:97-106), TestInstallScriptOmitsTLSSkipVerifyByDefault (:109-117); rewrite TestInstallCommandUsesScheme (:119-126) as TestInstallCommandUsesPublicURL asserting the exact `curl -sSL …` string; update the four SetPublicAddr calls (:16,:83,:99,:111). ADD TestInstallScriptRendersNoTLSArtifacts (no `-k`, no `TLS_SKIP_VERIFY`) and TestInstallScriptHonorsPathPrefix using SetPublicURL("https://ops.feast.tr/watch") — the executable proof the reverse-proxy case works.
- mother/generate_test.go: :13,:21 drop the "https" arg; :17 expects `curl -sSL http://10.0.0.1:8443/install/tk_`. This file is NOT in the api package and is easy to miss.
- agent/config_test.go: write TestLoadConfigIgnoresLegacyTLSKeys (a conf containing CA_FILE and TLS_SKIP_VERIFY=true still loads) BEFORE deleting TestHTTPClientRejectsBadCAFile (:54-63), TestHTTPClientTrustsCustomCA (:65-93), TestHTTPClientSkipVerify (:95-110) and TestLoadConfigParsesTLSTrustKeys (:43-52), so the package never dips below the 80% floor. Change https:// fixtures at :23,:28,:37 to http://. Verify with `go test -cover ./agent/...` before and after.
- mother/api/middleware_test.go: assert a freshly-constructed API's default publicURL is http-schemed.

**Risk.** HIGHEST-RISK WORKSTREAM. Every deployed agent holds `MOTHER_URL=https://<ip>:8443`; the instant the mother stops calling ListenAndServeTLS, each push sends a TLS ClientHello into a plain HTTP server and dies at the transport, with NO panel signal — `mother/api/admin.go:66-74` has no 'offline' state, so a freshly-installed host stays `pending` forever, indistinguishable from 'the one-liner was never run'. Mitigation is the reverse proxy on the same https://<ip>:8443. Secondary risk: FW_LISTEN defaults to `:8443` all-interfaces, so per-server bearer tokens, the admin X-API-Key and the unauthenticated binary download all cross the LAN in cleartext once TLS is gone — decide explicitly whether to default FW_LISTEN to 127.0.0.1 (only workable behind the proxy) and record it. Do NOT smuggle a ReadHeaderTimeout / explicit &http.Server into this diff; it is a separate named decision.

### W4 — Ingest write-volume

*Repo:* `both`  
*Depends on:* TLS removal

**Goal.** Cut system-wide row writes ~87% and storage ~66% by upserting both rollup tiers on ingest and deleting the raw tier plus the 30-second recompute — and fix the retention data-loss bug that becomes unrecoverable once raw is gone.

**Steps.**

- **modify** `mother/api/admin.go`
  FIX FIRST, INDEPENDENTLY SHIPPABLE: handlePutSettings (:175-196) validates only Interval and HeartbeatMissThreshold. Decode into a per-field pointer shadow struct and reject a body missing ANY retention key rather than defaulting it to zero; validate `retention_1m_days >= 1`, `retention_1h_days` sane, both `<= MaxRetentionDays`. Today an omitted key stores 0 and the next hourly EnforceRetention issues `DELETE FROM rollup_1m WHERE window_start < now` — fifteen days of history for every server, gone.
- **modify** `mother/store/settings.go`
  Stop discarding the strconv.Atoi error at :35 — a corrupt or non-numeric stored value currently reads as 0 and triggers the same tier wipe. Remove RetentionRawHours from the struct, defaults, GetSettings switch and SaveSettings map. Add a cached accessor (RWMutex, invalidated on SaveSettings): handleIngest calls GetSettings on every push (ingest.go:62), i.e. 5 queries/s at 50 servers on the single connection the rollup already monopolises.
- **add** `mother/store/pragmas.go`
  `applyPragmas(db)` called from Open before the schema exec: journal_mode=WAL (read back and verify — SQLite silently keeps the old mode if the file is locked; a mismatch is a startup error), synchronous=NORMAL, busy_timeout, auto_vacuum=INCREMENTAL. Values from named consts. Justify WAL by the fsync win ONLY: with SetMaxOpenConns(1) retained, WAL buys zero reader/writer concurrency — every statement queues on one conn regardless. Today each push runs two full rollback-journal create/write/fsync/delete cycles (ingest.go:45 and :50).
- **add** `mother/store/schema.go`
  Extract the `schema` const out of store.go. Both rollup tables become WITHOUT ROWID (measured 394.0MB → 225.6MB at 5.4M rows: a composite-PK rowid table stores the key twice, once in the row and once in sqlite_autoindex). `avg REAL` becomes `sum REAL` so the incremental merge is `sum = sum + excluded.sum` with no repeated multiply-divide drift. Drop `samples` and idx_samples entirely.
- **add** `mother/store/rollup.go`
  `ApplySamples(serverID, ts int64, samples map[string]float64) error` — one transaction, one multi-row `INSERT … ON CONFLICT(server_id, metric, window_start) DO UPDATE SET min=MIN(min,excluded.min), max=MAX(max,excluded.max), sum=sum+excluded.sum, cnt=cnt+1` against rollup_1m (ts/60*60) and again against rollup_1h (ts/3600*3600). Build a sorted slice from the map so statement text and bind order are deterministic. Typed error naming the failing tier.
- **add** `mother/store/migrate.go`
  Extract migrate() from store.go and add a numbered, PRAGMA user_version-gated migration list — the existing append-only ALTER list whose success case is a swallowed 'duplicate column name' cannot express a table rebuild, and user_version is currently 0/unused. Migration N, one transaction, before the listener binds: create `*_new` WITHOUT ROWID with the sum column, `INSERT … SELECT server_id, metric, window_start, min, max, avg*cnt, cnt`, DROP old, RENAME, `DROP TABLE IF EXISTS samples`, `DELETE FROM settings WHERE key='retention_raw_hours'`, set user_version. slog the rows copied and elapsed. This file is the mechanism W6 and W8 reuse.
- **modify** `mother/store/store.go`
  Shrink to Open + Close + DB(). applyPragmas → schema → migrate. Keep SetMaxOpenConns(1) with a comment tying it to the WAL choice. Add a VACUUM gated on the migration having actually dropped `samples` — auto_vacuum is 0 on existing DBs, so dropping the raw tier frees ~292MB into the freelist without shrinking the file by a byte.
- **delete** `mother/store/samples.go`
  Delete InsertSamples and RollupSince. Move EnforceRetention and DeleteHistory into a new mother/store/retention.go with `samples` removed from both table lists.
- **modify** `mother/store/servers.go`
  MOST SEVERE OMISSION IF MISSED: DeleteServer executes `DELETE FROM samples WHERE server_id = ?` at :204 inside its transaction. After the migration every DeleteServer call fails with 'no such table: samples', rolls back, and deleting a server becomes impossible in production. Remove that statement.
- **modify** `mother/api/ingest.go`
  Swap InsertSamples for ApplySamples. Add boundary validation before the write: every metric key must match a compiled regexp (`^[a-z][a-z0-9_]*(\.[a-z0-9_]+){0,2}$`, ≤64 chars) and every value must pass NaN/Inf rejection — 400 on either. Cardinality is currently unbounded (maxSamplesPerPush=256 caps one push, nothing caps distinct series), and one NaN would now poison a bucket permanently instead of one raw row. Add the IDEMPOTENCY GUARD: skip the write (200, no rows) when `ts <= srv.LastPush` — `cnt = cnt + 1` is not idempotent and with raw gone there is no recompute path to repair a double-counted bucket. Do NOT derive minPushGap from settings.Interval: a 429 makes the agent bail before reading the new interval (loop.go:95-97, :110-112), permanently bricking every deployed agent.
- **modify** `mother/api/chart.go`
  :54 `SUM(avg*cnt)/SUM(cnt)` → `SUM(sum)/SUM(cnt)`. Numerically identical — the same weighted mean, one multiplication earlier — so the panel contract is untouched.
- **modify** `mother/cmd/feast-watch/main.go`
  Delete the 30s rollup goroutine (:63-69) entirely. Move the retention goroutine into a small `mother/maintenance` package, run it once at startup (a mother restarted every 59 minutes never enforces retention at all today), and follow a successful EnforceRetention with PRAGMA incremental_vacuum — plain DELETEs return pages to the freelist and the file never shrinks, so an operator lowering retention to reclaim disk observes zero bytes returned.
- **modify** `mother/store/*_test.go, mother/api/*_test.go`
  COMPILE BREAKS THE CHANGE LIST MUST OWN: servers_test.go:105-147 TestDeleteServerPurgesRollupHistory calls InsertSamples (:111) and RollupSince (:114); admin_test.go:131 calls InsertSamples; chart_test.go:24,:26 seed via both; settings_test.go:11,:20 construct Settings with RetentionRawHours. All four break the moment those symbols are deleted. Port each to seed via ApplySamples. Note TestRetentionDeletesOldTiers (samples_test.go:75-79) asserts ONLY the raw tier, so 'porting' it is really writing a new test; and TestRollupSinceMidWindowDoesNotCorrupt (:82) encodes the hour-alignment invariant the recompute existed to protect — retire it consciously, in the commit message, rather than silently.
- **modify** `e2e/e2e_test.sh + docs + panel`
  e2e_test.sh:33 `sleep 45  # let the 30s rollup ticker fire` and the :2 header describe a goroutine that no longer exists — rewrite. docs/2026-07-16-feast-watch-design.md:148-167 (tier list, 'raw 48h' default). docs/2026-07-27-monitoring-panel-design.md:31 settings contract table (the doc the panel and the absent proxy are written against). Panel: remove the retention_raw_hours field at SettingsDialog.jsx:35 and the fixtures at SettingsDialog.test.jsx:16 and src/api/monitoring.test.js:73. Do NOT 'make the PUT send the full object' — SettingsDialog.jsx:78-82 already maps over every FIELDS entry and has never sent a partial patch.

**Tests (written first).**

- mother/store/rollup_test.go (NEW, write first): six pushes in one minute → one rollup_1m row with cnt=6 and exact min/max/weighted avg; the same six → one rollup_1h row; 360 pushes across an hour → 60 rollup_1m rows and 1 rollup_1h row whose avg equals the count-weighted mean; two servers pushing the same metric in the same minute never merge (port TestRollupIsPerServerNotAverage); a ts moving backwards across a closed bucket still yields exact min/max/sum/cnt; an empty samples map is a no-op, not an error.
- mother/store/migrate_test.go (NEW): build a DB with the OLD schema (rowid rollups with avg, populated samples), run Open, assert rollup_1m is WITHOUT ROWID, `sum == old avg*cnt` for every row, samples is gone, retention_raw_hours is gone from settings, user_version is set, page_count dropped, and a second Open is a no-op.
- mother/store/pragmas_test.go (NEW): journal_mode==wal, synchronous==1, auto_vacuum==2; Open errors when WAL cannot be established.
- mother/store/schema_test.go (NEW): both rollup tables report WITHOUT ROWID in sqlite_master; `samples` does not exist on a fresh DB.
- mother/api/ingest_test.go: metric name 'cpu usage!' → 400 with nothing written; NaN value → 400; a replayed push with ts <= last_push → 200 with cnt unchanged (the idempotency guard); a normal push writes exactly 2 rows per metric and zero elsewhere.
- mother/api/admin_test.go: PUT {interval, heartbeat_miss_threshold} with no retention keys → 400 and stored settings UNCHANGED; retention_1m_days:0 → 400; a valid full body → 200 and round-trips through GET.
- mother/store/settings_test.go: GetSettings errors on a non-numeric value; the cache returns the new value immediately after SaveSettings; concurrent GetSettings during SaveSettings is race-free under `go test -race`.
- mother/maintenance/maintenance_test.go (NEW): retention runs once immediately on Start; a failing retention call is logged and the loop survives to the next tick.

**Risk.** Two honest regressions the owner must accept. (1) PER-PUSH writes go UP: 5 raw inserts become 10 rollup upserts. System-wide daily writes fall from ~32.4M to ~4.32M (-87%), but requirement 2 was phrased 'too many rows written per push', so if the owner watches per-request write count they will see it increase — state this out loud. (2) Ingest failure semantics change from 'delayed rollup' to 'lost data point': the agent has no retry, so a non-200 discards that push's samples until the next tick (loop.go:95-97, :120-134). Mitigating context: today a rollup that misses its ~70-minute horizon loses that window permanently anyway, and the new write is one small transaction rather than a 9.7M-row scan, so failure probability drops. Also unaccounted in the arithmetic: TouchServer runs a full `UPDATE servers` on every push (servers.go:139-151), 432,000/day at 50 servers, and survives untouched — the post-change figure is understated. Migration is a one-time ~30-60s startup outage at 50-server scale, listener unbound, with VACUUM needing free disk equal to the final DB size.

### W5 — Agent binaries from GitHub Releases

*Repo:* `feast-watch`  
*Depends on:* TLS removal, Ingest write-volume

**Goal.** Move binary distribution to public GitHub Releases and turn the mother from a file server into a cached version index, without stranding a single deployed agent.

**Steps.**

- **add** `shared/release/asset.go`
  Single source of truth for release naming, shared by agent, mother and (mirrored) CI. `DefaultBaseURL`/`DefaultAPIBaseURL` consts; `AssetName(goos, goarch)` → `feast-watch-agent-<goos>-<goarch>` (VERSION-FREE — the tag carries the version, and that is what makes `/releases/latest/download/<asset>` resolvable, replacing the whole `latest-*` alias mechanism); `ChecksumName`; `DownloadURL(base, tag, asset)`; `LatestDownloadURL`; `ParsePlatform`; and `Platforms`, the ONE ordered list replacing both `bin/release.sh:24-29` (4 entries) and `mother/api/versions.go:32-36` (6 entries), which have already drifted despite a comment saying they must not.
- **add** `shared/release/checksum.go`
  `ParseChecksum(raw []byte) (string, error)`: trim, take the first whitespace-separated field (so both bare hex and `sha256sum`'s `<hex>  <file>` form work), lowercase, require exactly 64 hex chars. `agent/update.go:75-80` does a raw TrimSpace compare, so a GitHub Actions `sha256sum` file would fail as 'checksum mismatch' on a perfectly good binary — indistinguishable from an actual tamper.
- **modify** `agent/config.go`
  Add optional `RELEASE_BASE_URL` → `ReleaseBaseURL`, defaulting to `release.DefaultBaseURL`, validated at load as an absolute https URL (reject http:// and bare hosts). Air-gapped or mirrored fleets need to repoint without a rebuild, and https-only validation stops a config typo from downgrading binary distribution to plaintext now that the mother itself is plain HTTP.
- **modify** `agent/update.go`
  Build `url := release.DownloadURL(cfg.ReleaseBaseURL, desiredVersion, release.AssetName(GOOS, GOARCH))`. Fetch the `.sha256` FIRST (1KB) so a bad tag fails in one small request instead of after a 15.6MB download. Then stream with io.Copy into `os.CreateTemp(filepath.Dir(target), …)` hashing through an io.TeeReader under an io.LimitedReader cap; on mismatch remove the temp. On match: Chmod(0o755), Sync(), Close(), Rename, exit(0). Use http.NewRequestWithContext with a per-request deadline instead of Client.Timeout. Delete the duplicate binaryPath/checksumPath consts. Keep the function signature so loop.go and its tests are untouched. This fixes three latent hazards at once: `io.ReadAll` under a 256MB cap is an OOM on a 512MB VPS; the 60s whole-request timeout cannot complete 15.6MB under ~2.1 Mbps; and there is no fsync before the rename.
- **modify** `agent/cmd/feast-watch-agent/main.go`
  Build the update client on a cloned http.DefaultTransport with an explicit comment: 'system trust store only — this client talks to GitHub, never to the mother'. W3 already removed HTTPClient; this is the wiring that guarantees the coupling never comes back.
- **add** `mother/release/github.go`
  `GET {apiBase}/repos/{owner}/{repo}/releases?per_page=100` with Accept, X-GitHub-Api-Version, a real User-Agent (GitHub 403s without one), `If-None-Match`, and an optional Bearer only when FW_GITHUB_TOKEN is set. Distinguishable `ErrNotModified` on 304; a typed error carrying X-RateLimit-Remaining/Reset on 403/429.
- **add** `mother/release/index.go`
  Immutable snapshot `{Builds, FetchedAt, Source, Stale}` behind a Cache with a RWMutex — Refresh replaces the whole snapshot, never mutates. Drop drafts always; drop prereleases unless FW_INCLUDE_PRERELEASES; keep a version only when BOTH the asset and its `.sha256` are present (the existing both-files rule from versions.go:89); sort with the existing naturalLess. On refresh failure keep the previous snapshot and flip Stale. Union in a static `FW_AGENT_VERSIONS` seed for mothers with no outbound internet.
- **modify** `mother/api/versions.go`
  Delete availableBuilds, parseBuildName, knownPlatforms, binaryPrefix, checksumSuffix. MOVE naturalLess/leadingChunk into mother/release/index.go — do not delete them; they are the ordering that builds the snapshot. handleGetVersion reads the snapshot. agentBuild/versionView JSON stay byte-identical; add only additive optional `source`/`checked_at`/`stale`. rejectVersion keeps all four rules (still reject `latest`/`latest-*` — now because it is a moving GitHub redirect an agent can never report; still reject unknown; still reject wrong-platform; still skip when the agent reported no platform) and gains a fifth: when the snapshot is stale AND the version is unknown, say 'release list could not be refreshed (last checked <t>)' instead of 'is not staged on the mother', so an operator in an incident can tell a typo from a GitHub outage.
- **modify** `mother/api/install.go`
  KEEP `GET /download/agent/{version}` as a compatibility shim with `FW_DOWNLOAD_MODE=proxy|redirect|off`, DEFAULTING TO proxy for the migration window. Parse `{version}` into (tag, platform) accepting both the current `<ver>-<goos>-<goarch>` and the legacy GOOS-less `<ver>-<goarch>` shape (assume linux, exactly as bin/release.sh:76-80 did), strip and re-apply a `.sha256` suffix, keep the traversal guard, and reject `latest`. Proxy mode streams the GitHub body through the mother with an on-disk cache; redirect mode 302s; off returns 410 once no server row reports a pre-GitHub agent_version. Also replace the hardcoded `"feast-watch-agent-"` literal at :44 with the shared const.
- **modify** `mother/api/install.sh.tmpl`
  Replace the mother download at :20 with `RELEASE_BASE={{.ReleaseBaseURL}}` then two `curl -fsSL` fetches of `$RELEASE_BASE/releases/latest/download/feast-watch-agent-linux-$ARCH` and its `.sha256`, verified with `sha256sum -c` before `install -m0755` — the installer has never verified the checksum the release process has always produced. handleInstallScript gains ReleaseBaseURL in the template data.
- **add** `.github/workflows/release.yml`
  `on: push: tags: ['v*']` plus workflow_dispatch, `permissions: contents: write`. Matrix from shared/release.Platforms; setup-go with `go-version-file: go.mod`; `go test ./...` as a gate so a red tree never produces a release; `CGO_ENABLED=0 go build -ldflags "-X …/shared/version.Version=${GITHUB_REF_NAME}"` — GITHUB_REF_NAME is the tag verbatim, which is what makes desired_version and the tag the same string; `sha256sum | cut -d' ' -f1` per asset; a guard step failing if any matrix platform is missing from dist/ so a partially-uploaded release can never enter the index; then gh release create with every asset plus `.sha256` plus checksums.txt, `prerelease: contains(ref_name, '-rc')`. PREREQUISITE: `.github/` must already be committed (W1) — a tag-triggered workflow only runs if the file is in the tagged commit's tree.
- **modify** `bin/release.sh`
  Reduce to a LOCAL developer build: keep the builds, delete the OUT_DIR staging, the `latest-*` aliases (:66-70) and the GOOS-less legacy alias (:72-80). Replace the sync comment at :21-23 with a pointer to shared/release.Platforms. Optionally keep a `--stage` flag for air-gapped installs feeding proxy mode. Leaving the staging in place guarantees someone stages a build the GitHub-backed index cannot see.
- **modify** `docker-compose.yml, QUICKSTART.md, .env.example, docs`
  Drop FW_DOWNLOADS_DIR (compose:9 — note nothing has ever populated it inside the container, so the rollout path has never been exercisable there; .env.example:5). QUICKSTART:26-38 and :61-75 document the whole `OUT_DIR=… bin/release.sh v1.3.0` staging flow and the 'without .sha256 the agent refuses' rationale — rewrite. Amend docs/2026-07-28-agent-version-rollout-design.md:56-78 and explicitly reverse its 'No CI pipeline' decision at :44/:54. Fix the endpoint table at docs/2026-07-16-feast-watch-design.md:106,:118,:137 which still names the mother as the binary distributor. Point e2e agents at a stub release base URL by editing the printf lines in e2e/e2e_test.sh:19-21, NOT e2e/agent-*.conf (gitignored, regenerated every run).
- **modify** `/Users/ceydaakin/GitHub/feast-mobile-backend-control/src/features/monitoring/UpdateAgentDialog.jsx`
  Copy only: the empty-build message at :98-108 tells the operator to run `bin/release.sh` on the server — replace with 'no published release for this platform (<plat>) — check GitHub Releases'. Surface the new optional `stale`/`checked_at` as a small warning. Also update the now-wrong contract comment at src/api/monitoring.js:37-39 ('the agent builds staged on it').

**Tests (written first).**

- shared/release/asset_test.go (NEW, table-driven over Platforms): AssetName round-trips through ParsePlatform; ParsePlatform rejects `.sha256` companions, unknown platforms and bare prefixes; DownloadURL/LatestDownloadURL exact strings including a tag containing a dash (`v1.3.0-rc1`) and a base URL with a trailing slash.
- shared/release/checksum_test.go (NEW): bare hex; `hex  name`; `hex *name`; uppercase; CRLF; empty; 63-char; non-hex; an HTML error-page body (the 404-that-returns-200 case).
- mother/release/github_test.go (NEW, httptest): happy path parses tags and assets; 304 → ErrNotModified with the cached value surviving; 403 with rate-limit headers → typed error naming the reset time; malformed JSON → error with the cache untouched; token present/absent controls the header.
- mother/release/index_test.go (NEW): drafts excluded; prereleases excluded by default and included when configured; an asset with no `.sha256` sibling is not offered; unknown names ignored; v1.10.0 sorts above v1.9.0; refresh failure keeps the previous snapshot and only flips Stale; static seed union; concurrent Get during Refresh under `-race`.
- agent/update_test.go: rewrite the fixture server to serve `/releases/download/v1.3.0/feast-watch-agent-<goos>-<goarch>` and `.sha256` off an httptest ReleaseBaseURL. KEEP the three existing behaviours (replace+exit 0; reject a bad checksum leaving the binary untouched; never fall back to a GOOS-less name — update_test.go:58-92). ADD: 404 on the tag → error with the binary untouched; checksum fetched BEFORE the binary (assert request order); over-size body → error; a 302 chain across two httptest hosts is followed; no Authorization header reaches the second host.
- mother/api/versions_test.go: replace the filesystem `stage()` helper (:19-30) with an index-injecting helper and migrate ALL its users, including the three a naive list misses — TestSetServerVersionReachesTheAgent (:116-133), TestSetServerVersionLeavesOtherServersAlone (:137-150), TestServerListExposesUpdateState (:236-270). Preserve every existing assertion. ADD: the stale-index rejection message; rejectVersion performs ZERO network calls (inject a client that fails the test if called).
- mother/api/install_test.go: rewrite TestDownloadServesBinaryAndChecksum (:40-61) as TestDownloadProxiesGitHubRelease / TestDownloadRedirectsToGitHubRelease — exact Location for `v1.3.0-linux-amd64` and its `.sha256`; legacy `v1.3.0-amd64` maps to the linux asset; `latest` rejected; traversal rejected; off mode → 410.
- Panel: UpdateAgentDialog.test.jsx:141-147 asserts the empty-build copy via /yüklenmiş agent sürümü yok/i — the reword removes that phrase, so rewrite the assertion alongside the copy.
- Manual release smoke: push `v0.0.1-rc1`, assert exact asset names, `curl -fsSL .../releases/latest/download/feast-watch-agent-linux-amd64` resolves, the checksum matches, and the mother's index picks it up within one poll interval.

**Risk.** The compatibility shim is the whole risk. Defaulting to proxy rather than redirect is deliberate: a field agent installed against a self-signed mother still carries `TLS_SKIP_VERIFY=true`, and a 302 would make Go's default client fetch both binary and checksum from github.com with certificate verification disabled — the exact attack this workstream closes for new agents, re-created for old ones, with no remote remedy because the fix ships inside the download. Proxy mode also keeps the deployed 60s whole-request timeout on a LAN hop. Secondary: `git describe --tags --always --dirty` (bin/release.sh:16) on a repo with zero tags yields a bare SHA, so version↔tag identity is only guaranteed for CI-built releases — the mother must reject any target its index does not contain, which rejectVersion already does provided the index is the only source. Note also that the mother's local downloads/ currently holds no version-stamped builds and no .sha256 at all, so `GET /api/version` returns an empty list on this host today: the migration starts from an already-broken rollout path, and the shim protects agents that were installed, not agents that were ever successfully updated. Finally, deploy/k8s/daemonset.yaml agents would attempt an outbound github.com fetch from kube-system and try to overwrite a binary inside an image — nothing prevents an operator setting desired_version on a k8s-hosted server, and their config is a read-only Secret mount so RELEASE_BASE_URL cannot be added without a redeploy.

### W6 — Server groups and the update permission

*Repo:* `both`  
*Depends on:* Ingest write-volume, Agent binaries from GitHub Releases, Backend-proxy contract (written, not code)

**Goal.** Add server groups (filter plus bulk version targeting) in the mother and panel, and a distinct system:agent_update permission enforced at the proxy and reflected in the panel.

**Steps.**

- **modify** `mother/store/schema.go`
  Append `server_groups (id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, created_at INTEGER NOT NULL)`, `server_group_members (group_id, server_id, PRIMARY KEY(group_id, server_id))` and `idx_group_members_server ON server_group_members(server_id)` to the schema const — NEW TABLES need no migration entry, because Open re-executes the whole CREATE TABLE IF NOT EXISTS block on every start (store.go:75); only new COLUMNS need an ALTER. Declare NO foreign keys: verified empirically that `PRAGMA foreign_keys` reads 0 on this driver and a cascade does not fire. Name it `server_groups`, never `groups` — the panel and backend already own that word for user permission groups. Add ErrInvalidGroupName and ErrDuplicateGroup.
- **add** `mother/store/groups.go`
  ~200 lines. `Group{ID, Name, CreatedAt, ServerCount}`. CreateGroup with its OWN `validGroupName` (trimmed, 1..64 runes, no control chars, Unicode letters allowed) — do NOT reuse store.validName (`^[A-Za-z0-9._-]{1,64}$`), which is ASCII-only because server names are interpolated raw into a shell script and would reject 'Veritabanı Sunucuları'. ListGroups with LEFT JOIN counts; RenameGroup; DeleteGroup (transaction: memberships then the row); SetGroupServers as a whole-set replace in one transaction (never an add/remove delta API); ServerIDsInGroup; GroupsByServer() returning every membership in ONE query for the list projection.
- **modify** `mother/store/servers.go`
  (a) Add `DELETE FROM server_group_members WHERE server_id = ?` inside DeleteServer's existing transaction — with no FK enforcement and documented id reuse (:207-209), an orphan membership row would silently enroll a future server into a dead server's group AND hand it that group's next bulk version target. (b) Add `SetDesiredVersionForGroup(groupID, version) ([]int64, error)`: one transaction, one `UPDATE servers SET desired_version=?, update_error='' WHERE id IN (…)`, returning the ids written. Do NOT loop SetDesiredVersion — it returns ErrNotFound on 0 rows (wrong semantics for a member set) and N transactions can leave a group half-targeted. Clearing update_error matters: updateState derives 'failed' purely from a non-empty UpdateError (admin.go:56-64), so a retargeted group would keep stale red 'başarısız' badges.
- **modify** `mother/api/versions.go`
  Split rejectVersion into a pure `rejectVersionWith(srv, ver, builds)` holding the existing logic verbatim, plus a thin wrapper that reads the snapshot. Extract `isLatestAlias(ver)`. Keep rejectVersion's signature so versions_test.go stays green. After W5 the build list is already an in-memory snapshot, so this is a pure refactor.
- **add** `mother/api/groups.go`
  `registerGroups(mux)` — all routes behind requireAPIKey: GET/POST /api/groups, PATCH/DELETE /api/groups/{id}, PUT /api/groups/{id}/servers, PUT /api/groups/{id}/version. Views groupView{id,name,server_count,created_at} and groupRef{id,name}. Status mapping 400/409/404/500 — do NOT copy handleDeleteServer's blanket 500 (admin.go:139-142), which returns 'storage failure' for an ordinary nonexistent id. Slices always `make(…, 0, …)` so JSON is [] not null.
- **modify** `mother/api/groups.go`
  handleSetGroupVersion, the load-bearing handler. (1) parse, decode, TrimSpace. (2) Empty version → clear every member and return 200 with applied[] filled, mirroring the single-server cancel path (versions.go:190-197). (3) `latest` → 400 whole request. (4) read the build snapshot ONCE; version in no build → 400 whole request with nothing written (an operator typo, not a per-server fact). (5) partition members with rejectVersionWith — the only reachable failure now is 'no <ver> build for <platform>'. (6) write the passing ids in ONE transaction. (7) 200 with `data = {version, applied:[{id,name}], skipped:[{id,name,reason}]}`. Never success:false on a partial — writes landed and the panel's error path would report 'nothing happened'. Empty group → 200 with empty arrays.
- **modify** `mother/api/admin.go`
  Add `Groups []groupRef` to serverView; call GroupsByServer() ONCE before the loop in handleListServers and attach, defaulting to an allocated empty slice per server (admin.go:88 already allocates the outer slice for the same reason). Add `?group_id=` filtering: non-numeric → 400; unknown id → 200 with an empty list (a filter matching nothing is not an error). Register registerGroups(mux) in Handler().
- **add** `/Users/ceydaakin/GitHub/feast-mobile-backend-control/src/lib/useCan.js`
  `useCan()` → `{can, isLoading}` backed by useQuery on the shared ['admin','me'] key, `can = (perm) => Boolean(me?.permissions?.includes(perm))`, fail-closed while loading. Export `PERM_MONITORING_UPDATE = 'system:agent_update'`. There are SEVEN divergent inline copies of this expression today (DashboardPage:49, GroupsPage:44, FeedPage:26, BannedUsersPage:38, ModerationDetailDialog:81, RequirePermission:48, Sidebar:19) and the Sidebar copy differs — it treats a missing perm as ALLOWED, which is correct for optional nav gating and dangerously wrong if copied to gate a button. Adding an eighth is how that difference eventually lands in the wrong place.
- **add** `/Users/ceydaakin/GitHub/feast-mobile-backend-control/src/api/monitoringGroups.js`
  Separate module, deliberately NOT appended to monitoring.js: listServerGroups, createServerGroup, renameServerGroup, deleteServerGroup, setServerGroupMembers, setGroupVersion against /admin/monitoring/groups*. Six monitoring test files mock '@/api/monitoring' with hand-written factories listing only the exports that component uses, so a new export called by a component under test silently resolves to undefined. Document in the header that setGroupVersion returns the UNWRAPPED `{version, applied[], skipped[]}` and that a 200 with non-empty skipped[] is a partial success the caller must render, not swallow.
- **modify** `/Users/ceydaakin/GitHub/feast-mobile-backend-control/src/api/monitoring.js`
  `listServers({ groupId } = {})`, forwarding `params: {group_id}` ONLY when set — src/api/monitoring.test.js:34-38 pins `expect(apiClient.get).toHaveBeenCalledWith('/admin/monitoring/servers')` with a single argument, so passing a params object unconditionally breaks it. Note both call sites pass `queryFn: listServers` bare, so React Query invokes it with a QueryFunctionContext as the first argument — the destructure works only because `groupId` is absent from it; add an explicit wrapper arrow at each call site rather than relying on that.
- **modify** `/Users/ceydaakin/GitHub/feast-mobile-backend-control/src/features/monitoring/MonitoringPage.jsx + ServerDetailPage.jsx`
  Gate the per-row 'Güncelle' (MonitoringPage.jsx:154-162) and 'Agent Güncelle' (ServerDetailPage.jsx:120-123) on can(PERM_MONITORING_UPDATE) — hide, don't disable, matching Sidebar. Add a 'Gruplar' column rendering server.groups as badges, and group badges on the detail page. Add groupFilter state and change the query key to ['monitoring','servers',{groupId}] — UpdateAgentDialog.jsx:28 invalidates the PREFIX so a parameterized key is still caught (react-query v5), and four other components invalidate the same key. CAUTION: ServerDetailPage.jsx:50-51 uses the SAME bare ['monitoring','servers'] key and picks its server out of the fleet — if only MonitoringPage is parameterized they silently stop sharing the cache (an extra fleet-wide fetch per detail open); if both are, a filtered response makes the detail page 404 its own server. Leave ServerDetailPage on the unfiltered key deliberately, with a comment saying why.
- **add** `/Users/ceydaakin/GitHub/feast-mobile-backend-control/src/features/monitoring/ServerGroupsBar.jsx, ManageServerGroupsDialog.jsx, ServerGroupMembersDialog.jsx, UpdateGroupAgentDialog.jsx`
  Filter chips plus a manage entry point gated on the update permission; group CRUD with 409/400 surfaced via apiErrorMessage; a membership picker submitting the WHOLE set (replace semantics); and a bulk version dialog that does NOT pre-filter by platform (the group is heterogeneous — the mother decides per server) and does NOT close on success: it renders 'N sunucuya uygulandı' plus every skipped row as 'name — reason' and requires an explicit close. All four names carry the Server prefix because src/features/groups/GroupMembersDialog.jsx already exists. NOTE: do NOT reuse components/ui/transfer-list.jsx as-is — its signature is `({groups, value, onChange})` where `groups` must be a permission catalogue `[{domain, label, permissions:[{key,label}]}]`; using it for servers means generalizing shared UI, which is unbudgeted scope.
- **modify** `/Users/ceydaakin/GitHub/feast-mobile-backend-control/src/features/monitoring/*.test.jsx`
  Add `vi.mock('@/api/me')` with a default granting ['system:health','system:agent_update'] to MonitoringPage.test.jsx and ServerDetailPage.test.jsx. NO monitoring test mocks /admin/me today, and src/test/setup.js has no global axios stub — the moment useCan() enters these components, me resolves to undefined, can() fails closed, the buttons correctly disappear, and the existing 'Güncelle' assertions fail. That is a missing fixture, not a regression, and it must land in the SAME change as the gate or the TDD red step is indistinguishable from a real break.

**Tests (written first).**

- mother/store/groups_test.go (NEW, write first): duplicate name → error; empty/65-char/control-char name → ErrInvalidGroupName; 'Veritabanı Sunucuları' → accepted; SetGroupServers REPLACES rather than appends; an empty list empties the group; DeleteGroup removes memberships but leaves the servers; GroupsByServer returns [] (not nil) for an ungrouped server; a server in two groups appears in both.
- mother/store/servers_test.go: DeleteServer then re-create a server that reuses the id → the new server has no groups. MIRROR the existing TestDeleteServerPurgesRollupHistory pattern including its `t.Skipf` guard at :141 — SQLite only reuses an id when the deleted row held the max rowid, so without the guard this test is flaky. Also: SetDesiredVersionForGroup writes every member, clears update_error and returns the ids; an empty group returns an empty slice and no error.
- mother/api/groups_test.go (NEW, using adminReq/envelope from admin_test.go:15-28 and setup(t) from ingest_test.go:14): every route 401s without X-API-Key; POST duplicate → 409; PATCH unknown id → 404; PUT servers with an unknown server id → 400; GET returns [] on an empty mother.
- mother/api/groups_test.go bulk cases: a mixed-platform group where the version has only a linux-amd64 build → 200, applied holds the linux hosts, skipped holds the darwin host with reason 'no v1.4.0 build for darwin-arm64', and a follow-up GET /api/servers confirms the darwin host's desired_version is UNCHANGED; an unstaged version → 400 with NO server moved; 'latest' → 400; an empty version clears every member; an empty group → 200 with empty arrays; a member that never reported a platform is applied, not skipped.
- mother/api/versions_test.go: a table test asserting rejectVersionWith returns exactly the same strings as today for latest / unstaged / wrong-platform / unknown-platform — provable behaviour preservation for the extraction.
- mother/api/admin_test.go: a server in two groups lists both ordered by name; an ungrouped server lists []; ?group_id=N returns only members; ?group_id=abc → 400; ?group_id=999 → 200 with [].
- src/lib/useCan.test.jsx (NEW): false while me is undefined (fail-closed); true only for keys present in permissions.
- MonitoringPage.test.jsx: with ['system:health'] the Güncelle button is ABSENT; with the update permission it is present; the group column renders both badges; a filter chip re-queries with group_id. UpdateGroupAgentDialog.test.jsx: a response with skipped[] keeps the dialog OPEN and renders every skipped server with its reason; a 400 renders the mother's message with no success text.

**Risk.** Naming collision is the loudest failure mode: the panel already owns /groups, src/features/groups/, src/api/groups.js hitting /admin/groups, and the keys groups:list|create|update|delete — all for USER permission groups. Server groups must not reuse the path, the namespace, the directory or the bare component basenames. Second risk: the mother gets no permission enforcement at all by design, so until the proxy clauses ship, anyone holding the shared X-API-Key can PUT the version endpoints directly — the panel gate is hiding, not enforcing, and that gap is real between this workstream's deploy and the proxy's. Third: the panel plan gates buttons but not routes; a caller with system:health alone can still load group queries (reads, fine) — state explicitly whether the dialogs must be unreachable or merely unbuttoned.

### W7 — Downgrade guard in the update dialog

*Repo:* `panel`  
*Depends on:* Server groups and the update permission, Agent binaries from GitHub Releases

**Goal.** Recognise that a chosen agent version is older than — or not comparable to — what the server runs, and require a 5-second cooling-off plus explicit confirmation before it can be applied.

**Steps.**

- **add** `src/lib/versionOrder.js`
  Pure, React-free, no imports. parseVersion (strip one leading v/V, 1–4 dot-separated pure-digit segments or null); compareCore (segment-wise, missing segments = 0, so 1.4 === 1.4.0); compareVersions returning -1|0|1|null where null means NOT COMPARABLE — either side unparseable, OR equal cores with differing suffixes; classifyTarget → 'same'|'newer'|'older'|'unknown'|'no-current'; sortVersionsDesc returning a NEW array with non-comparable entries stably parked at the end. Comment the deliberate divergence from the mother's naturalLess at the top so nobody 'fixes' it to match Go later.
- **add** `src/features/monitoring/updatePolicy.js`
  DOWNGRADE_CONFIRM_MS = 5000 with DOWNGRADE_CONFIRM_SECONDS derived from it (not duplicated); RELATION_LABELS ('eski sürüm', 'karşılaştırılamıyor', 'şu an çalışıyor'); `updateRisk(server, targetVersion)` → {relation, risky, title, description} with `risky === (relation === 'older' || relation === 'unknown')` and Turkish copy built from server.name, server.agent_version and the target. Keeps the 5000 out of JSX and makes the risk rule unit-testable without rendering.
- **add** `src/components/ui/use-countdown.js`
  `useCountdown(seconds, active)` → {secondsLeft, done}. Seeds and starts a 1000ms interval when active flips true (functional updater, never mutating), clears and resets when it flips false, stops at 0, cleans up on unmount. No component may hold a raw setInterval. Reusable by W6's bulk group rollout.
- **modify** `src/components/ui/confirm-dialog.jsx`
  Add optional `confirmDelayMs = 0` and `delayHint`, defaulting to today's exact behaviour so all three existing danger call sites (ServerDetailPage:214, UsersPage:301, GlobalFeedCard:118) are byte-identical. When > 0: drive useCountdown(open), disable the confirm button while secondsLeft > 0, append ` (${secondsLeft})` to the label, and render an aria-live="polite" sr-only region announcing only at the BOUNDARIES ('Onay düğmesi 5 saniye sonra etkinleşecek.' → 'Onay düğmesi etkinleşti.') alongside an aria-hidden visible ticker updating every second — a screen reader reciting '5, 4, 3, 2, 1' conveys nothing. Pin the existing asymmetry rather than assuming it away: Cancel is `disabled={loading}` (confirm-dialog.jsx:38) while the X and Esc come from DialogContent and are NOT gated, so during the mutation Cancel greys out but X still dismisses.
- **modify** `src/lib/monitoringMetrics.js`
  In buildsForServer (:217-225) add `relation: classifyTarget(server.agent_version, build.version)` to each returned build and run the result through sortVersionsDesc, so the panel no longer depends on the source's order — GitHub's releases API returns newest-by-publish-date, not by version. DO NOT change `current` from raw string equality: the mother (admin.go:57) and the agent (agent/loop.go:125) both compare raw strings, so normalising only the panel would DISABLE the option that lets a host reporting `1.4.0` self-heal onto `v1.4.0`, with no other correction path.
- **modify** `src/features/monitoring/UpdateAgentDialog.jsx`
  Label options by relation (keep '(şu an çalışıyor)' plus disabled for same; append '(eski sürüm)' and '(karşılaştırılamıyor)', both still SELECTABLE). Derive `risk = updateRisk(server, selected)` and render a warning strip under the Select when risky. Add `pending` state: the submit handler (:37-41) becomes — no selection, return; risky, setPending(selected); otherwise mutate immediately. Render a ConfirmDialog with confirmDelayMs={DOWNGRADE_CONFIRM_MS}, confirmLabel 'Sürümü düşür', loading={mutation.isPending}, closed from the mutation's onSuccess/onError so a mother 400 lands in the existing banner (:52-56) rather than behind a stuck modal. Keep the submit label 'Güncelle'. The countdown starts at APPLY, not at selection — attaching it to the dropdown would burn the five seconds while the operator is still comparing and expire before intent forms.
- **modify** `src/features/monitoring/MonitoringPage.jsx`
  Optional, cut if the round is tight: when desired_version is older than agent_version, render a `<Badge variant="warning">sürüm düşürme</Badge>` in the target-version cell (:136-147), so a downgrade in flight is visible from the list — a guarded confirm that becomes invisible afterwards teaches nothing. Mirror it in ServerDetailPage.jsx:143-144 or the two views disagree.
- **modify** `docs/superpowers/specs/2026-07-28-agent-version-rollout-design.md`
  CROSS-REPO commit into feast-watch: :40-42 records an explicit non-goal — 'No compatibility interpretation. The panel … does not compute compatible or outdated' — and :156-161 pins the current dialog behaviour. A comparator that decides newer/older IS that interpretation. Amend with the reasoning for reversing it (the guard is a safety decision, not a compatibility claim) rather than silently contradicting the record.

**Tests (written first).**

- src/lib/versionOrder.test.js (NEW, it.each): v1.2.0→v1.4.0 newer; v1.4.0→v1.2.0 older; v1.10.0 vs v1.9.0 newer (the naturalLess case); v1.4 vs v1.4.0 same; V1.4.0 vs v1.4.0 same; 'dev'→v1.4.0 unknown and the reverse; ''→v1.4.0 no-current; v1.3.0-rc1 vs v1.3.0 unknown BOTH directions; rc1 vs rc2 unknown; 'abc' unknown; sortVersionsDesc returns a new array (`not.toBe(input)`) and parks 'dev' last. ADD the deployed-fleet cases: `v1.3.0-4-gabc1234`→v1.4.0 and `v1.3.0-dirty`→v1.4.0, which bin/release.sh:16's `git describe --tags --always --dirty` produces on real hosts — decide consciously whether these are 'unknown' (the guard then fires on every ordinary upgrade of a non-tagged host, training operators to click through) or get a trailing-suffix-tolerant rule.
- src/features/monitoring/updatePolicy.test.js (NEW): older ⇒ risky with both versions named; newer ⇒ not risky; 'dev' ⇒ risky with the karşılaştırılamadı copy; empty agent_version ⇒ not risky; SECONDS × 1000 === MS.
- src/components/ui/use-countdown.test.js (NEW, renderHook plus fake timers): starts at 5 with done false; reaches 0 and done true after 5×1000ms; never negative; re-arming reseeds; unmount clears (vi.getTimerCount() back to 0).
- src/components/ui/confirm-dialog.test.jsx (NEW — src/components/ui has NO tests at all today): write the no-delay path RED FIRST as a regression guard for the three existing call sites, including the irreversible server delete. Then confirmDelayMs=5000: disabled at t=0 and t=4000, enabled at 5000; clicking while disabled does not call onConfirm; the role=status region carries both boundary sentences; Vazgeç is clickable during the countdown; close-and-reopen restarts at 5.
- UpdateAgentDialog.test.jsx — MANDATORY TEST MECHANICS, verified empirically: @testing-library/dom 10.4.1 gates fake-timer detection on `typeof jest !== 'undefined'` (helpers.js:14-28), never true under vitest with globals:true, so userEvent plus vi.useFakeTimers() DEADLOCKS (reproduced: 8000ms timeout). Use fireEvent.change/click, synchronous getBy* only, and `await act(async () => await vi.advanceTimersByTimeAsync(ms))` — advanceTimersByTimeAsync(0) flushes the react-query version fetch since promises are not faked. Arm and disarm fake timers strictly INSIDE the new describe with its own useRealTimers afterEach; a file-level beforeEach would HANG all ten existing tests rather than fail them.
- UpdateAgentDialog.test.jsx cases: a NEWER version still applies immediately with no confirm (regression on the existing 'targets the chosen version' test); an OLDER version opens the confirm and does NOT call setServerVersion; the confirm is a no-op before 5s; after advancing 5000ms it calls setServerVersion(7, 'v1.1.0'); Vazgeç closes and calls nothing, leaving the selection intact; a 'dev' agent routes even a newer-looking target through the confirm; agent_version '' applies with no confirm; the older option renders with '(eski sürüm)' and is NOT disabled.

**Risk.** Panel-only, no mother or agent change, so blast radius is small — but the guard is purely cosmetic to anyone calling the proxy directly, since rejectVersion has no notion of newer/older and versions.go:199 is the only writer. Two mechanical traps: the fake-timer deadlock above, and nested Radix dialogs — every existing ConfirmDialog is a SIBLING of the dialog it relates to (ServerDetailPage:213-223), so this is the app's first stacked modal. Two focus scopes and two z-50 contents; jsdom will not catch a pointer-events or focus-trap regression, so one manual browser check is required. Escape hatch if it misbehaves: render the confirmation as a second STEP inside the same DialogContent, keeping the hook and the copy, with no other file changes.

### W8 — Clean uninstall and server decommissioning

*Repo:* `both`  
*Depends on:* Agent binaries from GitHub Releases, Server groups and the update permission

**Goal.** Give both the agent and the mother a defined, idempotent, offline-capable uninstall — and give the mother a way to tell a removed host to stop pushing without destroying its history.

**Steps.**

- **add** `mother/api/uninstall.sh`
  ~120-line plain (non-template) uninstaller, the single source of truth. Whole body in `main() { … }` with `main "$@"` last; set -euo pipefail; flags --purge/--yes/--no-deregister/--manifest; reads /etc/feast-watch/install-manifest into variables before touching anything, falling back to compiled defaults; a `safe_rm` guard rejecting any path that is not absolute, is /, has fewer than two components, or does not contain 'feast-watch'; a `have_systemd()` guard around every systemctl call (WSL2 without systemd and containers are both documented realities); a best-effort deregister reading TOKEN/MOTHER_URL from agent.conf BEFORE the config is removed, `|| true` throughout. Removal ORDER is forced by Restart=always/RestartSec=5: `disable --now` → rm unit → daemon-reload → reset-failed → rm binary AND `<binary>.new` → (purge) rm config dir and manifest. Every systemctl subcommand returns non-zero on an absent unit, which under set -e aborts mid-way and leaves a half-cleaned host, so idempotency is the whole correctness argument.
- **modify** `mother/api/install.sh.tmpl`
  After writing agent.conf, write `/etc/feast-watch/install-manifest` (0644, NO secrets) recording bin=/conf=/unit=/data= paths, the wants-symlink at /etc/systemd/system/multi-user.target.wants/feast-watch-agent.service (created by `enable --now` at :49 and missing from every path enumeration so far), the agent version and the timestamp. Then write the embedded uninstaller verbatim to /usr/local/sbin/feast-watch-agent-uninstall (0755) via a quoted heredoc with a distinctive delimiter. Also add `RestartPreventExitStatus=` to the emitted unit so a 410-terminated agent actually stays stopped.
- **modify** `mother/api/install.go`
  `//go:embed uninstall.sh`; pass it into the template data as UninstallScript; register `GET /uninstall.sh` serving the same bytes as text/x-shellscript with NO token lookup. The unauthenticated route is the recovery path for hosts installed before this change and for a lost on-disk copy; it carries no secrets and is strictly less powerful than the already-unauthenticated /download/agent route. Do NOT add /uninstall/{token}.sh.
- **modify** `mother/store/schema.go + migrate.go + servers.go`
  Add `decommissioned_at INTEGER NOT NULL DEFAULT 0` to the servers table plus a numbered migration in W4's user_version list. FOUR SITES that must move together or this is a runtime `sql: expected 15 destination arguments` on every server read: the Server struct (servers.go:20-43), serverCols (:81-82), scanServer's fixed Scan call (:84-103), and serverView construction (admin.go:90-97). Add DecommissionServer(id) (a single UPDATE, idempotent) and keep ServerByToken resolving decommissioned servers so the mother can answer them with a terminal signal rather than a bare 401.
- **modify** `mother/api/admin.go`
  status() (:66-74) returns a terminal `"decommissioned"` ahead of the down/pending checks — without it, an uninstalled host either shows `down` forever or must be DELETEd, and DeleteServer wipes both rollup tiers for that host precisely when the post-mortem is most wanted. Add an ADMIN route `PUT /api/servers/{id}/decommission` (and its clear) — without one the column is write-only-by-nobody, the status branch can never fire, and a self-decommissioned host has no path back. Add UninstallCommand as a sibling of InstallCommand.
- **modify** `mother/api/ingest.go + agent/loop.go`
  After bearerServer succeeds, a decommissioned server gets 410 Gone with no sample insert and no TouchServer. In the agent, PushOnce currently collapses the status into `fmt.Errorf("ingest returned %d")` (loop.go:95-97), so Run sees an opaque error — introduce a typed sentinel (never string-match '410') and have Run stop on it. Today a deleted server's agent keeps POSTing every 10s forever with no backoff, and the panel shows nothing.
- **add** `mother/api/selfservice.go`
  `POST /v1/self/decommission` authenticated by the existing bearerServer, gated behind FW_ALLOW_SELF_DECOMMISSION (default off), marking the server decommissioned and never deleting anything. Abuse surface stated plainly: the token is 128 bits of crypto/rand at mode 0600, so holding it already implies root on that host and already grants forging that server's metrics; a soft, reversible, single-server state change is the right amount of authority, whereas a hard DELETE would upgrade it to irreversible destruction of the host's history. The uninstaller must succeed with `|| true` when this fails — a decommissioned host frequently cannot reach the mother.
- **add** `deploy/feast-watch-mother.service + deploy/mother-install.sh + deploy/mother-uninstall.sh`
  The mother has NO unit, installer or uninstaller anywhere — QUICKSTART:39-40 hands the whole deployment to the operator, so its footprint is whatever each operator improvised. Unit: User/Group=feast-watch, StateDirectory=feast-watch (systemd owns /var/lib/feast-watch, which store.Open never creates), NoNewPrivileges/ProtectSystem=strict/PrivateTmp, Restart=always. Installer: binary to /usr/local/bin/feast-watch, system user, /etc/feast-watch/mother.env from a template refusing to start on an unset or literal `change-me` FW_API_KEY (.env.example:4 ships that value and main.go:51-55 only checks for empty), state dirs, its own manifest. Uninstaller: same main()/safe_rm/idempotency discipline; non-purge KEEPS /var/lib/feast-watch and mother.env; --purge names mother.db and its size and requires --yes or an interactive confirm read from /dev/tty (not stdin — it must work when piped).
- **add** `deploy/docker-teardown.md + deploy/k8s/uninstall.md + deploy/k8s/secret.example.yaml`
  Docs, not scripts, because a script here is more dangerous than a paragraph. Docker: `down` vs `down -v`, image removal, and an explicit DO-NOT — never `docker network rm feast-watch`, which is external precisely so the feast backend devcontainer survives (docker-compose.yml:32-38). Warn that e2e_test.sh:9 already runs `down -v` unconditionally on every EXIT, and that the untracked .devcontainer extends the same compose file with the same `mother` service, so a root-level teardown also destroys the developer's own container. K8s: delete the DaemonSet AND the `feast-watch-agent-conf` Secret, which the repo references at daemonset.yaml:27 but never defines — hence the committed example manifest, so install and uninstall become symmetric apply/delete over one directory.
- **modify** `QUICKSTART.md + panel SERVER_STATUS`
  Add an Uninstall section for all three surfaces, stating the ORDER explicitly: run the host uninstaller FIRST, decommission or delete in the panel SECOND. Panel: src/lib/monitoringMetrics.js:189-193 is a hardcoded three-entry Turkish map — add the decommissioned label, or the new status renders as the raw English word in a grey fallback badge inside an otherwise Turkish UI on both the list and the detail page, and requirement 6's stated payoff (no more permanently-red rows) is not delivered.
- **modify** `agent/update.go`
  Small follow-on to W5: nothing further is needed for the atomic write (W5 added Chmod+Sync+Rename into a CreateTemp), but confirm the uninstaller's `<binary>.new` sweep matches whatever temp-name pattern W5 settled on, and that a SIGKILL between write and rename cannot strand a file the uninstaller does not know to remove.

**Tests (written first).**

- e2e/uninstall_test.sh (NEW, container-based — idempotency under set -euo pipefail cannot be asserted by unit tests, since every failure mode is a non-zero exit from systemctl on an absent unit): install → verify binary, conf, unit and wants-symlink present → uninstall → verify all gone → uninstall AGAIN (must exit 0) → uninstall on a virgin host (must exit 0). Variants: systemctl absent from PATH → exit 0; a pre-created /usr/local/bin/feast-watch-agent.new → removed; a manifest supplying an EMPTY path → safe_rm refuses and the script exits non-zero WITHOUT calling rm (adversarial manifest inputs get their own case — set -u does not protect against `rm -rf "$CONF_DIR"/` with CONF_DIR empty).
- mother/api/uninstall_test.go (NEW): GET /uninstall.sh → 200, text/x-shellscript, body byte-identical to the embedded file, and working with ZERO server rows in the store. Plus a unit test asserting the embedded uninstall.sh contains no line equal to the heredoc delimiter — that is the failure mode that silently truncates the emitted uninstaller.
- mother/store/servers_test.go: opening a pre-column DB adds decommissioned_at without error and twice is a no-op; a fresh DB has 0; DecommissionServer is idempotent; a decommissioned server still resolves by token and still appears in ListServers.
- mother/api/ingest_test.go: a push with a decommissioned server's token → 410, no rows written, last_push unchanged. agent/loop_test.go (NOTE: there is currently NO test on Loop.Run anywhere — only PushOnce and tryUpdate — so these are all new): a 410 ends Run via the typed sentinel while a 401 keeps retrying.
- mother/api/selfservice_test.go (NEW, table-driven): valid token plus flag on → 200 with the timestamp set; flag off → 404 (route not registered, no capability disclosure); unknown token → 401; admin X-API-Key alone → 401 (token-scoped, not admin-scoped); a decommissioned server cannot un-decommission itself.
- mother/api/install_test.go: the rendered script's last non-empty line is `main "$@"`; it contains the uninstaller heredoc and writes install-manifest; the manifest block contains NO token. CI: systemd-analyze verify on both units; a container round-trip for mother-install.sh asserting the service reaches active, /var/lib/feast-watch is 0750 with correct ownership, and a placeholder FW_API_KEY fails fast BEFORE the unit is installed.

**Risk.** Two effects reach only newly-installed agents and must be stated as such rather than claimed as fixes. The 410 terminal signal is invisible to every agent already in the field — they collapse it into the same opaque error as a 401 and retry forever — and `RestartPreventExitStatus=` lives in the unit that install.sh.tmpl writes at install time, so a host that is not reinstalled keeps `Restart=always` and would respawn every 5 seconds, pushing MORE often than the 10s interval it was meant to stop. Secondary risk: the mother installer's `StateDirectory=` at mode 0750 interacts with the operator-staged downloads flow QUICKSTART:26-30 documents; after W5 that directory is only a proxy-mode cache, but get the ownership wrong and proxy mode silently fails. Also unknowable by design: FW_TLS_CERT/FW_TLS_KEY point at arbitrary paths, so no uninstaller can enumerate leftover key material — the doc must tell the operator to remove it by hand, and W3's migration should shred it. Finally, journald retains agent logs after removal with no per-unit vacuum, so a genuinely zero-trace uninstall is not achievable; document it rather than pretend.

## Cross-workstream conflicts

### W1 installer hygiene ↔ W3 TLS removal ↔ W5 GitHub Releases ↔ W8 uninstall (all edit mother/api/install.sh.tmpl)

**Collision.** Four workstreams rewrite the same file, and three of them rewrite the same `curl` line at :20. W1 adds `--fail` and the `main()` wrapper; W3 drops `-k` and the `{{if .TLSSkipVerify}}` block at :29-30; W5 replaces the download with a GitHub fetch plus `sha256sum -c`; W8 injects the uninstaller heredoc and the install-manifest.

**Resolution.** Strict order W1 → W3 → W5 → W8, one concern per diff. The payoff is not just avoiding merge pain: `mother/api/install_test.go:109-117` asserts `!Contains(body, "TLS_SKIP_VERIFY")` over the WHOLE rendered body, so if W8's uninstaller heredoc (which reads agent.conf) landed before W3 deleted that test, it would fail for a reason unrelated to TLS. Likewise W1's `main()` wrapper must precede W8, because a truncated uninstall download that executes a prefix could stop the service and then die mid-`rm -rf /etc/feast-w`.

### W3 TLS removal ↔ W4 write-volume ↔ W5 GitHub Releases (all edit mother/cmd/feast-watch/main.go)

**Collision.** W3 deletes :32-38, :58-61 and the ListenAndServeTLS branch :84-88; W4 deletes the 30s rollup goroutine :63-69 and reworks the retention goroutine :70-80; W5 replaces the `FW_DOWNLOADS_DIR` wiring at :56 with the release-cache construction and a poll goroutine.

**Resolution.** W3 → W4 → W5. W3 collapses the listener branch first so W4 and W5 edit a main() with one serve call. W4 empties the goroutine block before W5 adds one back, so the release poller is the only background job there besides retention. Each workstream extracts its logic out of main (`mother/publicurl.go`, `mother/maintenance`, `mother/release`) because package main has no test file.

### W4 write-volume ↔ W6 groups ↔ W8 uninstall (all edit mother/store/store.go and servers.go)

**Collision.** `DeleteServer` (servers.go:194-217) is edited three times: W4 removes the `DELETE FROM samples` at :204, W6 adds a membership purge, W8 leaves it alone but adds `decommissioned_at`. The `schema` const and `migrate()` are edited by all three, and `serverCols`/`scanServer` (servers.go:81-103) is a fixed 14-argument `row.Scan` that W8 extends.

**Resolution.** W4 → W6 → W8. W4 introduces the `PRAGMA user_version`-gated migration file (`mother/store/migrate.go`) that W6 and W8 then append numbered migrations to, instead of each inventing its own mechanism — the existing `migrate()` at store.go:93-106 is an append-only ALTER list whose success case is a swallowed 'duplicate column name' string match, which cannot express W4's table rebuild. W6 adds tables (no scanServer change, since groups are joined not stored on the row) and W8 adds exactly one column plus one matching `&srv.X`, so each is a compiler-checked edit rather than a three-way conflict on a positional scan.

### W5 GitHub Releases ↔ W6 groups (both edit mother/api/versions.go and middleware.go)

**Collision.** W5 deletes `availableBuilds`/`parseBuildName`/`knownPlatforms` and swaps `API.downloads` for a release cache; W6 needs `rejectVersion` split into a pure `rejectVersionWith(srv, ver, builds)` so a bulk rollout validates one snapshot, and needs `registerGroups(mux)` added to `Handler()`.

**Resolution.** W5 first — and it hands W6 the fix for free. Today `rejectVersion` calls `availableBuilds()` per invocation (`versions.go:214`), so a naive bulk loop over a 50-member group would be 50 `os.ReadDir` calls, and a concurrent `bin/release.sh` could make two members of one request see different build sets. After W5 the build list is already an immutable in-memory snapshot, so W6's extraction is a pure refactor with no performance or torn-read argument to make. Both use setters (`SetReleaseIndex`, `SetPublicURL`) rather than changing `New(st, apiKey, downloads)`, because the shared `setup(t)` helper at `mother/api/ingest_test.go:14` is called from five test files.

### W6 groups ↔ W7 downgrade guard (both edit the panel's update button and submit path)

**Collision.** W6 wraps the per-row 'Güncelle' button (`MonitoringPage.jsx:154-162`) and 'Agent Güncelle' (`ServerDetailPage.jsx:120-123`) in a permission check and adds a bulk `UpdateGroupAgentDialog`; W7 rewrites `UpdateAgentDialog`'s submit handler (`:37-41`) to route risky targets through a delayed ConfirmDialog.

**Resolution.** W6 → W7. W6 lands `src/lib/useCan.js` and the permission constants first, so W7's confirm path is reachable only through the same gate rather than needing a second retrofit. Both must add `vi.mock('@/api/me')` to `MonitoringPage.test.jsx` and `ServerDetailPage.test.jsx` — no monitoring test mocks it today, so the moment `useCan()` enters those components the buttons correctly disappear and the existing assertions fail; W6 owns that fixture change so W7 inherits it.

### W4 write-volume ↔ W2 backend-proxy contract (settings shape)

**Collision.** W4 removes `retention_raw_hours` from `store.Settings`, the GET/PUT payload and `SettingsDialog.jsx:35`, and requires a complete body on PUT. The proxy forwards settings.

**Resolution.** The contract clause is written in W2 and the proxy change deploys in the same window as W4's mother deploy. Ordering note that saves a wasted edit: the panel already sends the full key set (`SettingsDialog.jsx:78-82` maps over every FIELDS entry), so no 'partial patch' fix is needed — only the field removal, plus fixture updates in `src/api/monitoring.test.js:73` and `SettingsDialog.test.jsx:16`.

## Open questions for the owner

### Will a reverse proxy front the mother in production, and at what hostname and port?

**Recommended default.** Yes, terminating TLS on the same https://<ip>:8443 the agents already hold, with FW_PUBLIC_URL set to that same URL.

**Why it matters.** This single answer decides whether TLS removal is a pure code-deletion change or a fleet-wide migration. If the proxy reuses the existing address, no agent host is touched and the critical-severity blinding risk evaporates entirely. If not, every host must be visited, and there is no channel to reach them — a missed host goes dark with no panel signal.

### How many agents are actually installed right now, and is there SSH reach to all of them, including any Kubernetes DaemonSet?

**Recommended default.** Inventory before scheduling W3. If the fleet is only the WSL2 host from the LAN test, the sed one-liner is sufficient and the migration cost is zero.

**Why it matters.** It sizes the TLS migration, the GitHub-Releases proxy-mode window, and the uninstall backfill. K8s agents specifically cannot be reached by any of the three host migration paths — their config is a read-only Secret mount — so they need a separate patch-and-roll.

### Requirement 2 says 'too many rows written per push', but the recommended fix increases per-push writes from 5 to 10 while cutting system-wide daily writes from ~32.4M to ~4.32M. Is the system-wide number what you meant?

**Recommended default.** Yes — optimise total write volume and storage, and accept the per-push increase.

**Why it matters.** If the concern is genuinely per-request cost (a specific latency or IO metric you are watching), the answer is a different design: keep the 5 raw inserts and only make the background rollup incremental with a persisted watermark. That keeps the 292MB raw tier but halves the per-push write count relative to the dual-upsert.

### Should the 10-second raw tier be deleted outright, or kept behind a retention knob defaulting to off?

**Recommended default.** Delete it. No code reads it, and the chart API floors interval at 60s.

**Why it matters.** Keeping it as a debug switch costs one never-exercised code path and preserves a recompute route for repairing a double-counted bucket. Deleting it is what produces most of the -66% storage and -87% write win. Everything else in the package is unaffected either way.

### Is `desired_version` exactly the GitHub tag (v1.3.0), and should prereleases appear in the rollout dropdown?

**Recommended default.** Exact identity, and exclude prereleases behind FW_INCLUDE_PRERELEASES=false.

**Why it matters.** Exact identity is what lets CI use GITHUB_REF_NAME verbatim and removes any need for a tag↔version mapping table. It must be confirmed before the release workflow is written, because reversing it later means rewriting the mother's index, the agent's URL construction and every existing tag.

### Do any monitored hosts — or the mother itself — lack outbound internet to github.com and api.github.com?

**Recommended default.** Assume yes for at least some hosts: keep FW_DOWNLOAD_MODE=proxy as the default through the migration, and ship FW_AGENT_VERSIONS as an offline index seed.

**Why it matters.** It decides whether proxy mode is temporary scaffolding or a permanent supported mode, and whether firewall egress rules need documenting. Also relevant to repo visibility: the public repo is what makes token-free asset downloads possible, so making it private later would break every agent's download model at once.

### Should server-group CRUD share `system:agent_update`, or get its own permission?

**Recommended default.** Share it — both are monitoring writes, and a second key doubles the catalogue churn in the absent backend repo.

**Why it matters.** You may want group management separable from the ability to push a version (a team lead who can organise the fleet but not roll binaries). Splitting later means a second catalogue entry and a second route-gating pass in a repo we cannot edit here.

### Should a server whose agent_version is empty, or a non-tag version like `v1.3.0-4-gabc1234`, trigger the 5-second downgrade guard?

**Recommended default.** Empty → no guard (it provably cannot be a downgrade). Non-tag suffixes → treat a trailing `-N-g<sha>` or `-dirty` as the same core version rather than 'unknown', so ordinary upgrades of non-tagged hosts do not fire the guard.

**Why it matters.** bin/release.sh's `git describe --tags --always --dirty` produces exactly those strings on real hosts. Classifying them as 'unknown' makes the scary dialog fire on every ordinary upgrade, which trains operators to click through it — destroying the guard precisely where it matters.

### Should FW_ALLOW_SELF_DECOMMISSION default on or off?

**Recommended default.** Off, with an admin PUT /api/servers/{id}/decommission always available.

**Why it matters.** Off is safe but means the common case (operator runs the uninstaller, the panel still shows the host) needs a second manual step — which is the friction requirement 6 is complaining about. On-by-default is defensible: the token already implies root on that host, and the action is soft, single-server-scoped and reversible.

### Who owns the backend-proxy repo, and can its clauses deploy in the same windows as the mother changes they pair with?

**Recommended default.** Identify the owner before W3 ships and agree a joint window for clause 1 (MONITORING_API_URL) at minimum.

**Why it matters.** Out of sync in either direction on clause 1, the panel's monitoring page shows 502 'Mother sunucusuna ulaşılamıyor' and the operator hunts the mother rather than an env var. The other clauses (settings shape, groups routes, permission catalogue) degrade more gracefully but each blocks a user-visible feature.

### Are metric-name interning, release signing (cosign/minisign with the public key compiled into the agent), and a Windows/macOS installer in scope for this round?

**Recommended default.** No to all three. Interning is a further -28% on storage but touches every query; signing is the real upgrade over a mother-supplied checksum now that the mother is plain HTTP; a Windows uninstaller presupposes a scripted Windows install that does not exist.

**Why it matters.** release.sh builds windows-amd64 and darwin-arm64 and the mother offers six platforms, but the only installer in the tree is bash+systemd — so those agents have no install path and therefore no uninstall path. Naming this as a known gap is better than half-solving it.

