# feast-watch

feast-watch is an internal server-monitoring system — a separate codebase from the
feast backend, with the mother running on its own dedicated server (not embedded in
backend code or hosts). Lightweight agents run on every server and push metrics to
the central collector ("mother"). The mother stores rolled-up metrics and serves
summaries to the existing feast admin panel **through the feast backend only** —
feast-watch is never exposed to the public internet, and the frontend never talks to
the mother directly.

## Architecture

```
┌────────────┐  HTTPS push (10s)   ┌────────────┐   API key    ┌───────────────┐
│  agent(s)  │ ──────────────────► │   mother   │ ◄──────────  │ feast backend │
│ (each srv) │ ◄────────────────── │ Go+SQLite  │              └──────┬────────┘
└────────────┘  config in response └────────────┘                     │
                                     ▲    also runs an agent          ▼
                                     └── monitors its own host   admin panel
```

## Docs

- Full technical design: [`docs/superpowers/specs/2026-07-16-feast-watch-design.md`](docs/superpowers/specs/2026-07-16-feast-watch-design.md)
- Local dev / production setup: [`QUICKSTART.md`](QUICKSTART.md)
