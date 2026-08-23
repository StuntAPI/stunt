# Profiles in practice — define a behavior once, switch it at runtime

A **profile** is a named behavior mode for a running stunt server: you declare what the
world looks like when things go wrong, then flip that world on and off with one command
— no YAML edits, no restart, no asking a real API to have an outage on cue.

```bash
$ stunt profile activate payment-outage     # the world changes
$ run_my_retry_tests
$ stunt profile deactivate                  # and back
```

This guide is use-cases. For the field-by-field reference see the
[README's Profiles section](../README.md#profiles--runtime-activatable-behavior) and
`AGENTS.md`.

## The three shapes a profile can take

1. **A rule bundle per service** — declared in `stunt.yaml`, next to the service:

   ```yaml
   services:
     stripe:
       adapter: embedded:stripe-style
       profiles:
         payment-outage:
           description: every /v1 call fails
           rules:
             - match: { path: /v1/** }
               respond: { status: 500, body: { inline: { error: server_error } } }
   ```

   While active, the rules run as a **pre-dispatch override**: they intercept requests
   *before* handlers and base rules, so they reach routes the adapter owns — which is
   the whole point (a fault that can't touch `/v1/charges` can't test anything).

2. **A mode the adapter authors** — the adapter ships its own degraded behaviors, and
   its handlers read `profile_active()` (see use case 5).

3. **A global preset** — one activation assigns profiles across services, so a whole
   scenario flips at once:

   ```yaml
   profiles:
     launch-day:
       description: both dependencies degraded
       set:
         stripe: payment-outage
         sqs: throttled          # authored by the sqs-style adapter
   ```

## Use case 1 — chaos testing: "launch day"

You want the system under load-with-degradation: some percentage of calls fail, across
more than one dependency, and you want it reproducible in CI.

```yaml
services:
  stripe:
    adapter: embedded:stripe-style
    profiles:
      degraded:
        description: occasional 429s
        rules:
          - match: { path: /v1/** }
            when: { chance: 30 }                # exactly 30%, from rng_seed
            respond: { status: 429, body: { inline: { error: rate_limit_error } } }
```

```bash
$ stunt profile activate degraded
activated "degraded" on stripe
(runtime-only — resets on restart; `stunt up --profile` boots with one)

$ for i in $(seq 1 12); do curl -s -o /dev/null -w '%{http_code} ' \
    http://127.0.0.1:8000/v1/charges -H "Authorization: Bearer sk_test_demo"; done
429 200 200 200 200 429 200 200 200 200 429 429
```

Four failures in twelve — that's the 30% rate, and with a fixed `rng_seed` it is the
*same* four every run. That's what makes retry/backoff tuning a unit test instead of a
dice game: "given a 30% failure rate, the client must converge in ≤5 attempts".

## Use case 2 — revoked credentials, on demand

Real tokens expire on their own schedule (an hour, usually) and revoking a real key
means going to a dashboard and breaking other tests. With a profile it's a switch:

```yaml
services:
  stripe:
    adapter: embedded:stripe-style
    profiles:
      revoked-keys:
        description: every key suddenly invalid — exercise the 401/refresh path
        rules:
          - match: { path: /v1/** }
            respond: { status: 401, body: { inline: { error: invalid_api_key } } }
```

```bash
$ curl -s -o /dev/null -w '%{http_code}\n' .../v1/charges -H "Authorization: Bearer sk_test_demo"
201                                        # healthy: the key is fine
$ stunt profile activate revoked-keys
$ curl -s .../v1/charges -H "Authorization: Bearer sk_test_demo"
{"error":"invalid_api_key"}                # same key, same YAML — now rejected
```

The 401 fires **before** the adapter's auth logic sees the request, so this works on
any service, not just ones with token-expiry built in. Ideal for: refresh-token paths,
"re-authenticate" UX, monitoring alerts on auth error rates.

## Use case 3 — the one broken customer

Global chaos is easy; the *surgical* failure is the one real APIs never give you: this
one specific request shape fails, everything else is fine. `when.expr` matches on the
request itself:

```yaml
# on the service (services.stripe.profiles):
big-charges-fail:
  description: only charges over 1000 fail — the one broken customer
  rules:
    - match: { method: POST, path: /v1/charges }
      when: { expr: "request.body.amount > 1000" }
      respond: { status: 402, body: { inline: { error: card_declined } } }
```

```bash
$ stunt profile activate big-charges-fail
$ curl -s -o /dev/null -w '%{http_code}\n' .../v1/charges -d '{"amount":500,...}'
201                                        # small charge: untouched
$ curl -s .../v1/charges -d '{"amount":2000,...}'
{"error":"card_declined"}                  # 402 — only the targeted shape
```

`expr` sees `request.method`, `request.path`, `request.headers`, and the parsed
`request.body` — so "only requests with header X-Test-Canary", "only this one path",
"only amounts over the limit" are all one-liners.

## Use case 4 — the hanging dependency

Circuit breakers and deadlines need a dependency that *never answers* — not one that
404s quickly. `behavior: timeout` holds the connection and drops it:

```yaml
# on the service (services.stripe.profiles):
hanging:
  description: the dependency never answers — circuit-breaker food
  rules:
    - match: { path: /v1/** }
      respond: { behavior: timeout, latency_ms: 700 }
melting:
  description: launch day — everything 1.5s slower (but succeeds)
  rules:
    - match: { path: /v1/** }
      respond: { status: 200, latency_ms: 1500 }
```

```bash
$ stunt profile activate hanging
$ curl .../v1/charges
curl: (52) Empty reply from server          # dropped at ~700ms — the hang fired

$ stunt profile activate melting
$ curl -s -o /dev/null -w '%{http_code} in %{time_total}s\n' .../v1/charges
200 in 1.501475s                            # slow-but-healthy, for timeout budgets
```

`latency_ms` alone gives you slow-success scenarios (SLA warnings, spinner UX);
`behavior: timeout` gives you silence. Omit `latency_ms` on a timeout and it hangs for
30 seconds before dropping.

## Use case 5 — degraded modes the adapter authors itself

Rule bundles are generic (status/latency/body). When the *provider* has a specific
degraded behavior worth modeling — SQS returning empty receives so consumers exercise
retry/backoff — the adapter ships it, and its handlers implement it via
`profile_active()`:

```yaml
# adapters/sqs-style/adapter.yaml
profiles:
  throttled: "alternate ReceiveMessage calls return empty — exercise consumer retry/backoff paths"
```

```python
# inside the adapter's handler:
if profile_active() == "throttled":
    return respond(200, {"Messages": []})   # this receive yields nothing
```

```bash
$ stunt profile activate throttled         # unique name → auto-targeted to sqs
```

Unlike rule bundles, authored modes can carry *sequence* (every other call), not just
probability. One caveat: the sequence counter lives in service state, so a restart
resets the activation but not the counter — `stunt reset sqs` for a fresh alternation.
If you're building your own adapter, this is the pattern: name the mode in
`adapter.yaml`, branch on `profile_active()` in the handler, document it in the
description — users activate it without reading your Starlark.

## Use case 6 — in tests: flip worlds between cases

Profiles shine in CI, where each test case can pick its world:

```bash
stunt up &                                # healthy world
run_test "happy path: order completes"

stunt profile activate revoked-keys
run_test "auth failure: user re-authenticates"

stunt profile activate launch-day         # preset: several services at once
run_test "degraded: order parks, retries converge"

stunt profile deactivate
run_test "recovery: parked orders drain"
```

`stunt up --profile <name>` boots with one active (unknown names fail before serving),
the dashboard's **profiles** panel flips them with one click while you click through
the app manually, and `stunt profile list --json` reports state for scripts.

## The fine print

- **Runtime-only by design.** Activation is server state; restart resets the world
  (`stunt up --profile` restores a default if you want one).
- **Precedence.** Active profile rules run before handler/base-rule dispatch — that's
  how they reach adapter-owned routes. WebSocket and GraphQL dispatch earlier still,
  so profiles don't intercept those two transports.
- **Determinism.** `chance` draws the same per-service stream as base chance rules:
  fixed `rng_seed` → the same failures on every run, for serial traffic from a fresh
  boot. Parallel traffic preserves failure *counts*, not per-request order.
- **Names.** A name defined in both the manifest and an adapter activates both layers
  together — one name, one world. A bare `activate <name>` resolves a global preset
  first, then a name exactly one service defines (`--service` disambiguates the rest).
