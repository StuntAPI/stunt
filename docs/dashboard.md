# The stunt dashboard — observability + control for your sims

Every running stunt server serves its **own localhost dashboard** — a live request
inspector, a state browser, snapshot/restore, and an instance manager. There's no
separate daemon: it reads in-process state directly. A matching CLI (`--json`) backs
every feature for LLMs and automation.

Open it from a running server:

```
$ stunt up
  dashboard:  http://127.0.0.1:54321   (token: 9f3c…)
$ stunt ui          # opens the dashboard in your browser
```

The dashboard binds **loopback only** (`127.0.0.1`), requires an auth token (printed to
your terminal, never auto-shared), and rejects non-localhost `Host` headers as a
[DNS-rebinding](https://en.wikipedia.org/wiki/DNS_rebinding) defense. Sensitive request
headers (`Authorization`, `Cookie`, `X-API-Key`, …) are **redacted** before they're stored.

---

## 1. Request inspector

A real-time feed of every request hitting your sims — HTTP, gRPC, and WebSocket — with
method, path, status, and **sub-microsecond** timing. Click a row for full detail:
request/response headers (redacted) and **bodies**, **copy-as-curl**, and **replay**.

![Request inspector](img/dashboard-hero.png)

The feed is **live** (WebSocket push, gap-free on reconnect) and **searchable** — filter
by service, method, status, or free-text on the path:

![Filtered feed](img/dashboard-requests-filtered.png)

Clicking a row opens the detail panel — headers, bodies, copy-as-curl, and a replay
button that re-issues the request in-process against current state:

![Request detail](img/dashboard-request-detail.png)

### CLI

```bash
stunt requests                 # list captured requests (table)
stunt requests --json          # machine-readable (for LLMs/automation)
stunt requests --follow        # live stream (like tail -f)
stunt replay <id>              # re-issue a captured request
stunt request <id>             # full detail
```

---

## 2. State browser

stunt's differentiator is **stateful** simulation — your POSTs create data the sim
remembers. The state browser shows you exactly what your tests created: **collections**
(SQLite documents) with counts, **kv** pairs, and uploaded **blobs** (files). Read-only.

![State browser](img/dashboard-state.png)

Drill into a collection to inspect the actual documents:

![Collection documents](img/dashboard-state-docs.png)

### CLI

```bash
stunt state collections <service>           # list a service's collections (+ counts)
stunt state collection <service> <name>     # list the documents in a collection
stunt state kv <service> [--ns <ns>]        # list kv namespaces, or pairs in --ns
stunt state blobs <service> [--ns <ns>]     # list uploaded blobs
```

---

## 3. Snapshot, restore & reset — deterministic runs

**Reset** wipes a service's state (or everything) to a clean slate. **Snapshot** +
**restore** go further: capture a known-good state, run a flaky test against it, then
restore the exact pre-test state — so reruns are truly reproducible.

A snapshot is a portable `.tar.gz` (a logical dump of collections + kv + blobs, not a
raw DB copy — so it survives layout changes and is debuggable as JSON).

```bash
stunt reset <service>          # wipe one service's state
stunt reset --all              # wipe all services + the request log
stunt snapshot save [-o file]  # download a snapshot archive
stunt snapshot load <file>     # restore state from a snapshot
```

From the dashboard's **state** tab, the ⤓ snapshot and ⤒ restore controls do the same
graphically.

---

## 4. Instance manager

Running more than one server? The global registry (`~/.stunt/instances.json`) tracks every
stunt server across all manifests, and `stunt ps` lists them. Stale entries (crashed
servers) **self-prune** on the next read via a PID-liveness check.

```bash
stunt ps              # list running servers (PID, manifest, mode, age, dashboard, services)
stunt ps --json       # machine-readable
stunt stop [<pid>]    # stop one (by PID, or the current manifest's); SIGTERM → SIGKILL
stunt down            # stop the server for the current manifest
```

The dashboard's **instances** tab lists your running servers and lets you stop any of
them, or jump to another instance's dashboard:

![Instance manager](img/dashboard-instances.png)

---

## Architecture & security notes

- **Embedded, per-server** — each `stunt up` serves its own dashboard on an OS-assigned
  localhost port, reading in-process state. No daemon, no IPC.
- **Loopback-only** + **token auth** + **Host-header validation** (DNS-rebinding defense).
- **Bodies on by default** (it's a localhost dev tool and the killer feature); sensitive
  *headers* are redacted; the state dir is `0600` and `.gitignore`d.
- **Logging is off the hot path** — async/batched SQLite writes; a parked dashboard tab
  never backpressures request handlers.
- **Auto-discovery** — CLI commands resolve the running server's dashboard URL + token
  from the manifest's runtime file, so you rarely need `--url`/`--token`.

See `stunt llm` for the in-binary reference of every dashboard command.
