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

1. Build and stage a release:

   ```bash
   OUT_DIR=/var/lib/feast-watch/downloads bin/release.sh v1.3.0
   ```

   This compiles the version into both binaries and writes every agent build,
   plus its `.sha256`, where the mother serves them from. Do not build with a
   bare `go build`: without the injected version every agent reports `dev`, the
   panel's version column says `dev` for the whole fleet, and no agent can ever
   satisfy a rollout target. Without the `.sha256` files an agent refuses to
   install the update it downloaded.

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

The agent picks the target up on its next push, verifies the checksum, replaces
itself and exits for systemd to restart it. Watch `update_state` on
`GET /api/servers`: `pending` while it converges, `idle` once `agent_version`
matches, `failed` with `update_error` if it could not install. Send
`{"version":""}` to cancel a rollout that has not landed.

Targets are per server on purpose — update one host, confirm it, then the rest.
The mother is not self-updating: `GET /api/version` reports its version so you
can see what the agents should catch up to, but deploying it stays with
systemd/Docker/k8s.
