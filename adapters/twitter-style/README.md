# X.com / Twitter-style adapter

A stunt adapter for simulating an **X.com / Twitter-style API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by X.com / Twitter. "X.com" and "Twitter" are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A broader-than-minimal MVP of an X.com / Twitter-style API: mock OAuth2 token
issuance, tweets (create, retrieve, list, delete), users (show, lookup by
username, me), and a reverse-chronological timeline.

State persists in an in-process SQLite-backed collection store, so tweets you
create in one request are visible in subsequent requests within the same
`stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/2/oauth2/token` | `auth.star#on_token` | Get a mock access token (always succeeds) |
| POST | `/2/tweets` | `tweets.star#on_create` | Create a tweet (id `twt_<seq>`) |
| GET | `/2/tweets/{id}` | `tweets.star#on_retrieve` | Retrieve a tweet |
| GET | `/2/tweets` | `tweets.star#on_list` | List all tweets (reverse-chron) |
| DELETE | `/2/tweets/{id}` | `tweets.star#on_delete` | Delete a tweet |
| GET | `/2/users/me` | `users.star#on_me` | Return the current synthetic user |
| GET | `/2/users/by/username/{username}` | `users.star#on_lookup` | Lookup user by username |
| GET | `/2/users/{id}` | `users.star#on_show` | Show a user by ID |
| GET | `/2/users/{id}/timelines/reverse_chronological` | `timeline.star#on_timeline` | Reverse-chronological timeline |

Any unmatched route returns `404 {"error":"resource_not_found"}`.

## Backing stores

| Collection | Seed fixture | Purpose |
|------------|-------------|---------|
| `tweets` | `fixtures/tweets.jsonl` | Tweet records |
| `users` | `fixtures/users.jsonl` | User records |

Tweet IDs use the `twt_` prefix; user IDs use `usr_` or `seed-user-*` (seed
data), generated via a KV-backed sequence counter.

## Layout

```
adapter.yaml              Manifest: endpoints, resources, rules, identity
DISCLAIMER                Not affiliated / synthetic-only notice
README.md                 This file
scripts/
  auth.star               Mock OAuth2 token endpoint handler
  tweets.star             Tweet CRUD handlers (stateful)
  users.star              User show / lookup / me handlers
  timeline.star           Reverse-chronological timeline handler
fixtures/
  tweets.jsonl            Seed data for the tweets collection
  users.jsonl             Seed data for the users collection
templates/
  tweet.json              Example tweet response (faker placeholders)
  user.json               Example user response (faker placeholders)
schemas/
  tweet.schema.json       JSON Schema for a tweet object
```

## Auth

The adapter declares `identity.token_scheme: bearer` as metadata. Auth is
**mock / not enforced** — any (or no) `Authorization` header is accepted. The
`POST /2/oauth2/token` endpoint always returns a fake access token so that
client libraries requiring a token flow can run locally without real
credentials.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  twitter:
    adapter: ./adapters/twitter-style
```

Then `stunt up` and make requests to the served address.
