# Removing feast-watch

Prose rather than a script, because the dangerous parts here are one flag apart
from the safe ones and a script would hide which is which.

## Order

1. **Host first.** Run the agent uninstaller on each monitored server. An agent
   whose mother is gone keeps pushing every 10 seconds forever — nothing in the
   protocol can tell it to stop.
2. **Panel second.** Delete the server, which removes its record and its stored
   history. The agent cannot delete its own record: it holds a per-server token,
   not an API key, and turning token possession into irreversible destruction of
   that host's whole history is not a trade worth making.
3. **Mother last**, if the whole system is being retired.

## Agent, on each monitored host

```bash
sudo feast-watch-agent-uninstall            # service, binary, leftovers; keeps the config
sudo feast-watch-agent-uninstall --purge    # also removes /etc/feast-watch and the token
```

The installer writes this to `/usr/local/sbin/`, so it works on a host that can
no longer reach the mother — which is the normal case for a machine being
decommissioned. If it is missing:

```bash
curl -fsSL http://<mother>/uninstall.sh | sudo bash -s -- --purge
```

`--purge` removes the token, and no endpoint reissues one. Re-adding the host
later means running the install one-liner again, which mints a new token.

## Mother, on its own host

```bash
sudo deploy/mother-uninstall.sh             # service and binary; keeps the database
sudo deploy/mother-uninstall.sh --purge     # also removes the database, config and user
```

Without `--purge` the database survives. It holds every server's history **and
every agent's token**; losing it means reinstalling every agent, not just
restoring a backup of some metrics.

## When both live on one host

The mother monitors its own host, so `/etc/feast-watch` usually holds
`mother.env` **and** `agent.conf`. Each uninstaller removes only the files it
installed and drops the shared directory only when nothing is left in it:

- removing the agent keeps `mother.env` — the mother's API key
- removing the mother keeps `agent.conf` — the agent's token, which no endpoint
  reissues

Order is unchanged: uninstall the agent, delete the server in the panel, then
uninstall the mother.

## Docker (local development)

```bash
docker compose down            # stop and remove the containers
docker compose down -v         # also delete the mother-data volume (the database)
```

**Do not** `docker network rm feast-watch`. That network is declared external
here and is also declared by the feast backend's devcontainer, which cannot
start without it. Removing it breaks a project this one does not own.

## Kubernetes

```bash
kubectl -n kube-system delete daemonset feast-watch-agent
kubectl -n kube-system delete secret feast-watch-agent-conf
```

The DaemonSet updates by image tag, not by self-update, so it is not part of the
agent rollout flow and the host uninstaller never reaches it. Its config is a
read-only Secret mount, which is also why the `agent.conf` migration scripts do
not apply to it: change the Secret and roll the DaemonSet instead.

Deleting the DaemonSet leaves the servers' records in the panel. Delete them
there as in step 2.
