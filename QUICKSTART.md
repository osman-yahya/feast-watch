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

3. From the admin panel (or CLI), add a server:

   ```bash
   feast-watch generate --name=DB_Sunucusu
   ```

   Either flow prints a one-liner. Paste it on the target server:

   ```bash
   curl -sSL https://<mother-ip>:8443/install/<token>.sh | sudo bash
   ```

   The install script downloads the right-arch agent binary, writes
   `/etc/feast-watch/agent.conf`, and installs + starts the `feast-watch-agent`
   systemd service. The server flips from *pending* to *online* on its first push.

   Kubernetes nodes use `deploy/k8s/daemonset.yaml` instead (hostPID + `/proc`
   mount, token supplied as a Secret).
