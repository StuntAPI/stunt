# Blog-style adapter (GraphQL example)

A stunt adapter demonstrating a **GraphQL blog API** — a small but realistic
domain with users, posts, and comments, showcasing queries, mutations, nested
relations, enums, and a custom scalar. This is a reference example for writing
GraphQL-backed adapters; it does **not** mimic any specific company's API
(no DISCLAIMER needed). All data is synthetic.

## What it serves

A GraphQL endpoint at `POST /graphql` with the following operations:

| Type | Operation | Description |
|------|-----------|-------------|
| Query | `user(id)` | Fetch a single user by ID. |
| Query | `users` | List all users. |
| Query | `post(id)` | Fetch a single post by ID. |
| Query | `posts(status?)` | List posts, optionally filtered by `PostStatus`. |
| Mutation | `createUser(name, bio?)` | Create a new user. |
| Mutation | `createPost(userId, title, body, status?)` | Create a new post. |
| Mutation | `addComment(postId, author, body)` | Add a comment to a post. |

### Nested relations

The schema models a blog domain with relations resolved by table-backed
Starlark resolvers:

- **`User.posts`** → posts where `user_id` matches the user.
- **`Post.comments`** → comments where `post_id` matches the post.
- **`Post.author`** → the user whose `id` matches the post's `user_id`.

Scalar fields (`id`, `name`, `title`, `body`, …) use the default resolver
(`parent[fieldName]`) — no Starlark function needed.

### Enums and custom scalars

- **`PostStatus`** enum (`PUBLISHED` / `DRAFT`) round-trips through queries
  and mutations.
- **`DateTime`** custom scalar passes through as an ISO-8601 string.

## Data

Three SQLite-backed collections are seeded from JSONL fixtures at startup:

| Collection | Fixture | Seed |
|------------|---------|------|
| `users` | `fixtures/users.jsonl` | 3 users |
| `posts` | `fixtures/posts.jsonl` | 3 posts |
| `comments` | `fixtures/comments.jsonl` | 3 comments |

State persists in-process, so data created via mutations is visible in
subsequent queries within the same `stunt up` session.

## Usage

Point a `stunt.yaml` service at the adapter directory:

```yaml
services:
  blog:
    adapter: ./adapters/blog-style
```

Then `stunt up` serves it. Query the GraphQL endpoint:

```bash
# Nested query: user → their posts → each post's comments
curl -s http://127.0.0.1:8000/graphql \
  -H 'Content-Type: application/json' \
  -d '{"query":"{ user(id:\"u1\") { name posts { title comments { author body } } } }"}'
```

## How it works

The `graphql:` section in [`adapter.yaml`](adapter.yaml) points to the SDL
schema ([`schemas/schema.graphql`](schemas/schema.graphql)) and the Starlark
resolver script ([`scripts/resolvers.star`](scripts/resolvers.star)). Root
fields (`Query`/`Mutation`) are routed to `on_<field>(callArg)` functions;
object fields use `resolve_<Type>_<field>(callArg)` where `callArg` is a dict
with keys `parent` and `args`. See
[`adapters/README.md`](../README.md) for the full GraphQL authoring guide.
