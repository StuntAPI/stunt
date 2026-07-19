# Contributing to stunt

Thanks for your interest in `stunt`! The fastest way to contribute is an
**adapter** — a local simulator for some public API. Adapters are just a
directory of YAML + Starlark + fixtures; no Go knowledge is required to write
one.

This guide covers the contribution workflow. For the **adapter authoring
reference** (the `adapter.yaml` schema, Starlark builtins, rules engine,
gRPC/WebSocket), see [`adapters/README.md`](./adapters/README.md).

---

## TL;DR — add an adapter

```bash
stunt adapter new myapi-style        # scaffold an adapter
cd myapi-style
# edit adapter.yaml, scripts/*.star, fixtures/*.jsonl
stunt adapter lint ./myapi-style     # MUST pass: synthetic-data guard
stunt adapter test ./myapi-style     # replay traces / conformance (optional)
# add a DISCLAIMER (required for branded APIs — see below)
```

Then open a PR adding the directory under `adapters/` and (optionally) an entry
in the catalog.

---

## Ways to contribute

- **An adapter** for an API you test against (highest value).
- **A bug fix or feature** for the Go core (`internal/...`).
- **Docs, examples, and test coverage.**
- **Reports** — open an issue for bugs, missing API coverage, or rough edges.

## Code of conduct

Be kind and constructive. Assume good faith. "Unofficial, not affiliated" is
the project's posture toward every provider whose API style an adapter mimics —
please extend the same respect to people.

---

## Repository layout

```
cmd/stunt/        # the CLI binary
internal/
  adapter/        # adapter format + loader
  rules/          # declarative rules engine (match/evaluate/faker/template)
  starlark/       # sandboxed Starlark VM
  engine/         # wires it all together; HTTP + gRPC + WebSocket servers
  primitives/     # Collection / KV / Blob / Clock / Identity / Events / ...
  netutil/        # portless.dev-style TLS proxy + CA + /etc/hosts
  contrib/        # contributor tooling: new / import / lint / test / catalog
  adapterdist/    # git adapter distribution
  grpcsim/        # descriptor-driven gRPC server
  manifest/       # stunt.yaml schema
adapters/         # first-party reference adapters (contributions land here)
docs/             # specs + plans
```

## Dev environment

You need **Go 1.23+**.

```bash
git clone https://github.com/deblasis/stunt
cd stunt
go build ./...                 # builds everything
go test ./...                  # runs the suite
```

> **Dev-machine note:** if `go` commands fail with a stdlib-version mismatch,
> prefix them with `env -u GOROOT`. (A local `GOROOT`/toolchain
> misconfiguration on some setups; not required in CI or for end users.)

---

## Writing an adapter

1. **Scaffold it:**
   ```bash
   stunt adapter new myapi-style
   ```
   This creates `adapter.yaml`, an example `GET /hello` endpoint, a sample
   template, a seed fixture, a Starlark handler, a JSON schema, and a README —
   all synthetic.

2. **Author it.** See [`adapters/README.md`](./adapters/README.md) for the full
   reference: the `adapter.yaml` schema, the rules engine (templated responses,
   probabilistic faults, conditional expressions), the Starlark builtins
   (`store_collection`, `store_kv_*`, `store_blob`, `identity_*`, `events_*`),
   and the gRPC + WebSocket transports.

3. **Import from a spec (optional):** jump-start an adapter from an existing
   API description:
   ```bash
   stunt adapter import openapi  path/to/openapi.json  --dir ./myapi-style
   stunt adapter import har     path/to traces.har     --dir ./myapi-style
   stunt adapter import proto   path/to api.proto      --dir ./myapi-style
   ```

4. **Test it locally:**
   ```bash
   # point a manifest at it and serve
   echo 'version: 1
   services:
     myapi:
       adapter: ./adapters/myapi-style' > stunt.yaml
   stunt up
   curl http://127.0.0.1:8000/hello
   ```

### Branded adapters: naming & disclaimer (mandatory)

Adapters that mimic a specific company's API **must** follow the neutral-naming
convention (see [`adapters/README.md`](./adapters/README.md#neutral-naming--disclaimers-mandatory-for-branded-adapters)
for the full rules):

1. **`-style` naming** for the id, directory, and description — e.g.
   `stripe-style`, never `stripe`. Never claim to *be* the provider.
2. **A `DISCLAIMER` file** in the adapter dir (copy
   [`adapters/DISCLAIMER.template`](./adapters/DISCLAIMER.template)) stating it
   is not affiliated/endorsed, is for local testing only, and uses synthetic
   data only.
3. **Synthetic data only** — all fixtures/templates are faker-generated.
   `stunt adapter lint` must pass clean. This is the core safety property.
4. **Structure, not documentation** — reproduce the API shape needed for local
   testing; do not reproduce the provider's proprietary docs verbatim.
5. **No provider logos/trademarks.**

Generic, non-branded adapters (e.g. `echo-style`) are exempt from `-style`
naming but still must pass `stunt adapter lint`.

---

## Quality gates (every contribution must pass)

### Adapters

```bash
stunt adapter lint ./adapters/<your-adapter>      # no findings
```

`lint` enforces the **synthetic-data** invariant: it scans fixtures, templates,
and handler scripts for real-looking data (emails, UUIDs, tokens, credit-card
numbers, PII field names). All adapter data must be synthetic. If you see a
finding, replace the value with a faker or a clearly-fake placeholder.

### Go core

If you touch `internal/...` or `cmd/...`, the canonical gate is one command:

```bash
just ci
```

That runs `build` + `test -race` + `vet` + `fmt-check` + `mod-tidy` +
`lint-adapters` — the exact set a PR must pass. (A hosted CI job invokes the
same `just ci`, so there's one source of truth for "does this ship".) Granular
recipes exist too: `just test`, `just vet`, `just fmt`, `just fmt-check`,
`just lint-adapters`, `just smoke`. The equivalent raw commands are
`go test -race ./...`, `go vet ./...`, and `gofmt -l .` (must be empty).

All gates must pass. `-race` is non-negotiable for new concurrent code. Tests
are **host-safe** by convention: they use temp dirs, free high ports,
`file://` git fixtures, and `httptest` sinks — they never touch `~/.stunt`,
`/etc/hosts`, the system trust store, or the real network.

### Test conventions

- Prefer table-driven tests.
- Integration tests build a temp adapter + start the engine via
  `ServeForTest` (free ports) and use real clients (HTTP / gRPC / WebSocket)
  — mirror the existing `*_test.go` in `internal/engine/`.
- Add a test for every new behavior; fix bugs with a failing test first (TDD).

---

## Submitting a PR

1. Fork & branch from `master`.
2. Make your change; keep commits focused (one logical change per commit).
   Conventional-ish commit subjects are appreciated (`feat:`, `fix:`,
   `test:`, `docs:`, `chore:`) but not strictly enforced.
3. Run the gates above.
4. If adding an adapter, include:
   - the adapter directory under `adapters/` (with `DISCLAIMER` if branded),
   - a one-line entry in the `adapters/README.md` table,
   - confirmation that `stunt adapter lint` is clean.
5. Open the PR with a clear description of **what** and **why**. Link any
   related issue.

### Review criteria

- Adapters: lint clean; synthetic data; branded naming + disclaimer; useful
  coverage of the API's common paths.
- Core: tests added; `-race`/`vet`/`gofmt` clean; host-safe tests; matches
  existing style; security-conscious (adapters run sandboxed Starlark — the
  sandbox is the trust boundary, so never add host I/O to the VM).

---

## Security & trust model

The defining property of `stunt` is that **community adapters are safe to
install**: adapter logic runs inside a **sandboxed Starlark VM** with no host
I/O (no filesystem, no network, no subprocess), bounded by an execution-step
limit. State is mediated through primitives (Collection/KV/Blob/Identity/
Events), not raw host access. When contributing core changes, preserve this
boundary — never expose host I/O to adapter scripts.

Adapters must contain **only synthetic data** (enforced by `stunt adapter lint`)
so installing one never ships real PII or provider secrets.

## Licensing

By contributing you agree your contributions are licensed under the project's
license (see the repository root). Ensure you have the right to contribute any
code or fixtures you submit, and that **adapters contain no proprietary
provider data or documentation**.
