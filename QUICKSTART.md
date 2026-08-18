# QUICKSTART

## Local development

Local development only — production uses systemd (see below), not Docker.

```bash
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

## Production

1. Publish a release:

   ```bash
   git tag v1.3.0 && git push origin v1.3.0
   ```

   `.github/workflows/release.yml` builds every platform with the tag compiled
   in and uploads each binary plus its `.sha256` to the GitHub release. The tag
   *is* the version: it is what gets compiled in and what agents ask for, with
   no mapping in between.

   The mother stores no binaries and serves none. It reads the published
   releases from the GitHub API — a conditional request every five minutes,
   which is not counted against the unauthenticated rate limit when nothing
   changed — and offers only versions carrying both a binary and its checksum
   for the target host's platform.

   `bin/release.sh` still builds every platform locally, named exactly as the
   release assets, for development or for uploading by hand if CI is
   unavailable.

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

Targets are per server on purpose — update one host, confirm it, then the rest.
The mother is not self-updating: `GET /api/version` reports its version so you
can see what the agents should catch up to, but deploying it stays with
systemd/Docker/k8s.

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

## Removing it

Agent, on each monitored host — the installer leaves the uninstaller on disk, so
this works even when the mother is already gone:

```bash
sudo feast-watch-agent-uninstall --purge
```

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
bin/release.sh v1.3.0
sudo deploy/mother-install.sh bin/build/feast-watch
# edit /etc/feast-watch/mother.env, then:
sudo systemctl start feast-watch-mother
```
