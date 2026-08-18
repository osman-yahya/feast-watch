# Backend Proxy Contract — 2026-08-18 round

Date: 2026-08-18
Audience: whoever owns the Go backend that mounts `/admin/monitoring/*`

That repository is not part of this change and was not available to edit. This
file is the complete list of what it must do, clause by clause, each tagged with
the feast-watch change it pairs with. Nothing here is optional: a clause that
does not ship leaves a user-visible feature dead.

---

## Clause 1 — the mother is plain HTTP (ships with: TLS removal)

`MONITORING_API_URL` must change from `https://<mother>:8443` to the mother's
plain-HTTP URL, or to the URL of whatever terminates TLS in front of it.

The mother no longer calls `ListenAndServeTLS`; `FW_TLS_CERT` and `FW_TLS_KEY`
are gone. If the proxy keeps pointing at `https://`, every monitoring request
fails at the transport and the panel shows `502 Mother sunucusuna ulaşılamıyor`
— which sends the operator looking at the mother rather than at an env var.

**Deploy together with the mother.**

---

## Clause 2 — `retention_raw_hours` is retired (ships with: ingest write-volume)

`GET /admin/monitoring/settings` no longer returns `retention_raw_hours`, and
`PUT` no longer requires it. The mother folds every push straight into the
1-minute and 1-hour rollups and keeps no raw tier, so the field bounded nothing.

The mother still **accepts and ignores** the key on the way in, so a proxy that
forwards the body verbatim needs no change and can be updated whenever
convenient. A proxy that decodes into a typed struct with a required field must
drop it.

`PUT` now **rejects a payload missing any of** `interval`,
`heartbeat_miss_threshold`, `retention_1m_days`, `retention_1h_days` with a 400.
Previously an omitted retention key was stored as `0` and the next hourly sweep
deleted that entire tier for every server. If the proxy synthesises settings
payloads rather than forwarding them, it must send the complete set.

---

## Clause 3 — the version endpoint gains freshness fields (ships with: GitHub Releases)

`GET /admin/monitoring/version` gains three fields alongside the existing
`mother_version` and `agents`:

```json
{ "mother_version": "v1.4.0",
  "agents": [{ "version": "v1.4.0", "platforms": ["linux-amd64"] }],
  "checked_at": "2026-08-18T11:02:00Z",
  "stale": false }
```

These are additive. A proxy that forwards the payload verbatim needs no change.
A proxy that re-serialises through a typed struct must carry them, or the panel
cannot warn that the release list is the last known good one.

`agents` no longer describes files staged on the mother — it is the published
GitHub releases. Nothing about its shape changed.

---

## Clause 4 — the permission catalogue gains `system:agent_update`

Add to whatever backs `GET /admin/permissions`:

```json
{ "key": "system:agent_update",
  "label": "Agent sürüm güncelleme",
  "requires": "system:health" }
```

The mother cannot enforce this and must not pretend to. It authenticates the
backend with a single shared `X-API-Key` and has no notion of a caller, a user
or a role — a second key would be checked by the same component that already
made the decision, which is theatre. Enforcement belongs here.

Do **not** reuse the `groups:*` namespace: it is already taken by user
permission groups in the panel (`/groups`, `src/api/groups.js`).

---

## Clause 5 — split the blanket permission mount

`/admin/monitoring/*` is currently mounted wholesale under
`RequirePermission(PermSystemHealth)`. Split it:

| Routes | Permission |
| --- | --- |
| `GET /servers`, `GET /chart`, `GET /settings`, `GET /version`, `GET /groups` | `system:health` |
| `POST/DELETE /servers`, `PUT /servers/{id}/collectors` | `system:health` |
| `PUT /servers/{id}/version` | **`system:agent_update`** |
| `PUT /groups/{id}/version` | **`system:agent_update`** |
| `POST /groups`, `PATCH /groups/{id}`, `DELETE /groups/{id}`, `PUT /groups/{id}/servers` | **`system:agent_update`** |
| `DELETE /history` | `system:health` |

Group management shares `system:agent_update` rather than getting its own key:
both are monitoring writes, and a second catalogue entry doubles the churn here
for a distinction nobody has asked for yet. Splitting it later is additive.

**Acceptance:** a caller holding `system:health` but not `system:agent_update`
gets 403 on every write route above and 200 on every read route.

The panel hides these controls when the caller lacks the permission, but that is
hiding, not enforcing — anyone holding the shared `X-API-Key` can call the
mother directly.

---

## Clause 6 — new routes to proxy (ships with: server groups)

Forward these to the mother unchanged, under the permissions in Clause 5:

| Route | Body | Response |
| --- | --- | --- |
| `GET /admin/monitoring/groups` | — | `[{id, name, server_count, created_at}]` |
| `POST /admin/monitoring/groups` | `{name}` | the created group; **409** on a duplicate name |
| `PATCH /admin/monitoring/groups/{id}` | `{name}` | `null` |
| `DELETE /admin/monitoring/groups/{id}` | — | `null` |
| `PUT /admin/monitoring/groups/{id}/servers` | `{server_ids:[]}` | `null` |
| `PUT /admin/monitoring/groups/{id}/version` | `{version}` | see below |

`GET /admin/monitoring/servers` gains an optional `?group_id=` passthrough, and
every server row gains `groups: [{id, name}]` — always a list, never null.

`PUT /groups/{id}/version` answers **HTTP 200** with:

```json
{ "version": "v1.4.0",
  "applied": [{ "id": 1, "name": "web-1" }],
  "skipped": [{ "id": 2, "name": "mac-box", "reason": "no v1.4.0 build for darwin-arm64" }] }
```

It is 200 and `success: true` even when `skipped` is non-empty, because the
writes in `applied` landed. Do not translate a non-empty `skipped` into an
error: the panel's error path would tell the operator nothing happened while
half the group is already converging.

A fault in the **version** — unpublished, or the moving alias `latest` — is a
400 with nothing written, because no member could accept it. A missing build for
one host's **platform** is that host's problem and only skips that host: one
darwin laptop must not permanently block a rollout across forty Linux servers.

---

## Not in this contract

The mother gets no user, role or second key. Every clause above is enforcement
the proxy already owns; feast-watch changes only what it can honestly enforce.
