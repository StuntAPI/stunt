# stunt

Local API simulators — test against real APIs, locally, without remote accounts.

> **Status:** foundation only. This build supports inline, stateless,
> rule-driven mock servers defined in `stunt.yaml`. Adapters, Starlark,
> state stores, TLS, and `*.localhost` subdomains arrive in later plans.

## Install

```bash
go install github.com/stunt-adapters/stunt/cmd/stunt@latest
```

## Quickstart

```bash
stunt init       # writes a sample stunt.yaml
stunt plan       # validates the manifest and shows what would run
stunt up         # serves all services (Ctrl-C to stop)
```

Example `stunt.yaml`:

```yaml
version: 1
rng_seed: 42
network:
  mode: port
  base_port: 8000
services:
  example:
    rules:
      - match: { method: GET, path: /hello }
        respond: { status: 200, body: { inline: { message: hi } } }
      - match: { method: GET, path: /hello }
        when: { chance: 20 }            # 20% of replies error
        respond: { status: 503, body: { inline: { error: boom } } }
```

Then:

```bash
curl http://127.0.0.1:8000/hello
```

## Rules

Rules are evaluated first-match-wins, top-to-bottom.

- **match**: `method`, `path` (globs: `*` one segment, `**` zero+ segments), `headers`.
- **when.chance**: percent probability the rule fires; otherwise evaluation
  falls through to the next rule (deterministic via `rng_seed`).
- **respond**: `status`, `headers`, `body` (`inline` literal or `file` path,
  relative to the manifest), `latency_ms`, and `behavior: timeout`
  (hold then close the connection to simulate a timeout).

## Roadmap

See `docs/specs/2025-07-15-stunt-design.md` and
`docs/plans/2025-07-15-stunt-foundation.md`.
