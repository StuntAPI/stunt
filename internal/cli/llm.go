package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// llmReference is the compact, self-contained reference an LLM or coding agent
// needs to operate stunt successfully. It is baked into the binary so it can
// never drift from the shipped version. It mirrors the (longer) AGENTS.md in
// the repository; this is the always-available, version-coupled form.
//
// An agent harness can capture `stunt llm` output and feed it as context to a
// model that has never seen stunt before, and the model will have the full
// command list, manifest schema, and the complete Starlark handler API.
const llmReference = `# stunt — reference for LLMs and coding agents (from: stunt llm)

stunt creates LOCAL API simulators (mock servers) so you can test code that
calls real public APIs WITHOUT remote accounts or network access. Describe
services in stunt.yaml; ` + "`stunt up`" + ` serves them on local ports.

## Quick start
  stunt init          # write a sample stunt.yaml
  stunt plan          # validate it, show what will run
  stunt up            # serve (foreground; Ctrl-C to stop)
  stunt demo          # boot the stripe-style sim + print curl commands
  stunt doctor        # health check (run when something fails)
  stunt catalog search <q>   # find adapters (--json for machine output)

## Commands
  init/plan/up/down   manifest lifecycle (up is the main command)
  demo                one-shot stateful Stripe-style demo
  doctor              CA + manifest + adapter + port health check
  clean               wipe state, CA, hosts block (keeps manifest + adapters)
  catalog search|show discover adapters (--json)  # all 91 embedded, works offline
  adapter new|add|lint|test|list   build/validate adapters (lint MUST pass)
  hosts sync|clean    manage /etc/hosts (subdomain TLS mode)
  proxy start         TLS reverse proxy (subdomain mode)
  trust               install stunt CA in system trust store (needs privilege)
  service install|status|uninstall   system service unit
  version | --version
Global: --manifest <path> (default stunt.yaml). Cache: --cache-dir/$STUNT_ADAPTER_CACHE.

## Manifest (stunt.yaml)
  version: 1
  rng_seed: 42                       # deterministic fakes
  network: { mode: port, base_port: 8000 }   # or mode: subdomain (+ tls, tld, sync_hosts)
  services:
    svc:
      adapter: ./adapters/svc-style  # local path OR git:github.com/org/repo@ref
                                    # OR embedded:stripe-style (bundled in binary, no clone)
      config: { webhook_url: ... }   # optional, passed to scripts
      # OR rules-only (no adapter):
      # rules:
      #   - name: ok
      #     match: { method: GET, path: /x }
      #     when: { chance: 20 }                 # OR expr: "request.body.n > 1"
      #     respond: { status: 200, body: { inline: {...} | file: b.json | template: t.json }, latency_ms: 100 }
Rules match in ORDER (first wins); "/**" is the catch-all. Templates: {{ faker.Email }} {{ uuid }} {{ now.Format ... }}

## Adapter manifest (adapter.yaml)
  id / name / version / api: { name, version }   # api = the REAL upstream API simulated (REQUIRED)
  real_hosts: [...]
  endpoints:                          # route + method -> handler; LITERAL routes before {param} ones
    - { route: /v1/items, method: GET, handler: scripts/items.star#on_list }
    - { route: /v1/items/{id}, method: POST, handler: scripts/items.star#on_create }
  resources:                          # backing stores
    - { name: items, kind: collection|kv, seed: fixtures/items.jsonl }
  identity: { token_scheme: bearer }  # informational; enforce in handlers
  rules: [ { name: catchall-404, match: { path: "/**" }, respond: { status: 404 } } ]
  # optional transports: grpc: {service, descriptor, methods[]} | graphql: {schema, resolvers, path} | ws: [{route, handler}]

## Starlark handler API  (def on_x(req): ... return respond(...))
req keys: method, path, headers (dict, case-insensitive), body (parsed JSON or None),
          raw_body (string, for binary), query (dict), params (path captures).
Builtins:
  respond(status, body, headers)            # or return {status, body, headers}
  c = store_collection("items"); c.insert(d); c.get(id); c.list(); c.update(id,d); c.delete(id)
  store_kv_set(ns,k,v); store_kv_get(ns,k); n=store_kv_incr(ns,k); store_kv_delete(ns,k)
  b = store_blob("up"); b.put(name,bytes,ctype); b.get(name); b.stat(name); b.list(); b.delete(name)
  tok = identity_mint(subject, scopes=[...]); sub = identity_validate(token); identity_has_scope(token,scope)
  events_register(url); events_emit(event_type, payload)
  json.loads(s) / json.dumps(obj)           # json module predeclared
lib.star in scripts/ is PRELOADED — its defs are shared across handlers. NO load(). NO fs/net/import.
Gotchas: literal routes before param routes; IDs via store_kv_incr; ALL data synthetic (adapter lint
flags real emails/UUIDs/provider-IDs/cards/tokens — use {{ faker.Email }}/{{ uuid }}).

## Point your client at stunt
  Set YOURAPI_BASE_URL=http://127.0.0.1:<port>  (port from ` + "`stunt plan`" + `/` + "`stunt doctor`" + `)
  Subdomain TLS mode: https://<service>.localhost
  State persists across requests in one ` + "`stunt up`" + `; ` + "`stunt clean`" + ` resets it.

## When stuck
  stunt doctor  (CA/manifest/adapter-load/port conflicts)  ->  stunt plan  ->  stunt clean  ->  stunt up
Full reference (with examples + agent workflows): AGENTS.md in the repo.
`

func newLLMCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "llm",
		Short: "Print a compact reference for LLMs and coding agents",
		Long: `Print a compact, self-contained reference covering stunt's commands, manifest schema,
and the complete Starlark handler API — everything an LLM or coding agent needs to operate
stunt successfully.

Capture this output and feed it as context to a model that has never seen stunt before:

    stunt llm > stunt-context.md

This is baked into the binary so it can never drift from the shipped version. A longer
reference with examples and agent workflows lives in AGENTS.md in the repository.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := io.WriteString(cmd.OutOrStdout(), llmReference)
			return err
		},
	}
}

// runLLM is exported for tests that want to capture the reference without a
// cobra command context.
func runLLM(out io.Writer) (int, error) {
	return fmt.Fprint(out, llmReference)
}
