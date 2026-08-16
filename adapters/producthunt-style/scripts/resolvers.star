# Product Hunt GraphQL resolvers — served by the engine's real GraphQL
# executor at POST /v2/api/graphql.json (see adapter.yaml).
#
# Root fields use on_<field>(callArg); object fields use
# resolve_<Type>_<field>(callArg). Scalar fields fall back to the default
# resolver (parent[fieldName]). Posts persist in the seeded "posts"
# collection; the connection pagination (first/after) maps onto query_select.
#
# All data is synthetic.

# The synthetic maker credited with every post.
_MAKER = {
    "id": "stunt-maker",
    "name": "Stunt Maker",
    "username": "stuntmaker",
    "created_at": clock.now_rfc3339(),
}

# ---------------------------------------------------------------------------
# Query root resolvers
# ---------------------------------------------------------------------------

# post(id) → Post | None
def on_post(args):
    pid = args["args"]["id"]
    doc = store_collection("posts").get(pid)
    return respond(200, doc)

# posts(first, after, order) → PostConnection (Relay-style, offset cursors)
def on_posts(args):
    a = args["args"]
    docs = store_collection("posts").list()
    total = len(docs)

    # NEWEST/FEATURED_AT both order by creation: ids are the store sequence,
    # so newest = highest numeric id first. Sort numerically — ids are
    # unpadded decimal strings, so lexicographic order puts "10" between
    # "1" and "2" (and GraphQL Int variables arrive as floats).
    order = a.get("order")
    if order == None or order == "":
        order = "NEWEST"
    pairs = []
    for d in docs:
        n = _to_int(d.get("id", "0"))
        if n < 0:
            n = 0
        pairs.append([n, d])
    pairs = sorted(pairs)
    docs = []
    for i in range(len(pairs) - 1, -1, -1):
        docs.append(pairs[i][1])

    first = _to_int(a.get("first"))
    if first <= 0:
        first = 20
    after = a.get("after")
    offset = 0
    if after != None and after != "":
        offset = _to_int(after)

    docs = query_select(docs, None, None, "", first, offset, None)

    end = offset + len(docs)
    has_next = end < total
    end_cursor = None
    if len(docs) > 0:
        end_cursor = str(end)

    edges = []
    for d in docs:
        edges.append({"node": d, "cursor": str(offset + len(edges) + 1)})

    return respond(200, {
        "edges": edges,
        "nodes": docs,
        "pageInfo": {
            "hasNextPage": has_next,
            "hasPreviousPage": offset > 0,
            "startCursor": str(offset + 1) if len(docs) > 0 else None,
            "endCursor": end_cursor,
        },
        "totalCount": total,
    })

# ---------------------------------------------------------------------------
# Mutation root resolvers
# ---------------------------------------------------------------------------

# postCreate(input) → PostCreatePayload. Server-side validation reports
# per-field errors in the payload (Product Hunt's mutation convention), not
# as top-level GraphQL errors: data.postCreate.errors.
def on_postCreate(args):
    input = args["args"].get("input")
    if input == None:
        input = {}

    name = input.get("name", None)
    tagline = input.get("tagline", None)
    description = input.get("description", None)
    url = input.get("url", None)

    errors = []
    if name == None or str(name).strip() == "":
        errors.append({"attribute": "name", "message": "Name can't be blank", "path": ["attributes", "name"]})
    if tagline == None or str(tagline).strip() == "":
        errors.append({"attribute": "tagline", "message": "Tagline can't be blank", "path": ["attributes", "tagline"]})
    if description == None or str(description).strip() == "":
        errors.append({"attribute": "description", "message": "Description can't be blank", "path": ["attributes", "description"]})
    if url == None or str(url).strip() == "":
        errors.append({"attribute": "url", "message": "Url can't be blank", "path": ["attributes", "url"]})
    elif not _is_http_url(str(url)):
        errors.append({"attribute": "url", "message": "Url is invalid", "path": ["attributes", "url"]})

    if len(errors) > 0:
        return respond(200, {"post": None, "errors": errors})

    seq = store_kv_incr("producthunt", "post_seq")
    post_id = str(seq)

    doc = {
        "id": post_id,
        "name": str(name),
        "tagline": str(tagline),
        "description": str(description),
        "url": str(url),
        "votes_count": 0,
        "created_at": clock.now_rfc3339(),
        "featured_at": None,
        "user_id": _MAKER["id"],
    }
    store_collection("posts").insert(doc)

    return respond(200, {"post": doc, "errors": []})

# ---------------------------------------------------------------------------
# Object resolvers
# ---------------------------------------------------------------------------

# Post.votesCount → Int (stored as votes_count).
def resolve_Post_votesCount(args):
    return respond(200, _to_int(args["parent"].get("votes_count")))

# Post.createdAt → DateTime (stored snake_case).
def resolve_Post_createdAt(args):
    return respond(200, args["parent"].get("created_at", ""))

# Post.featuredAt → DateTime | None (stored snake_case).
def resolve_Post_featuredAt(args):
    return respond(200, args["parent"].get("featured_at", None))

# Post.user → User (the synthetic maker).
def resolve_Post_user(args):
    return respond(200, _MAKER)

# User.createdAt → DateTime (stored snake_case).
def resolve_User_createdAt(args):
    return respond(200, args["parent"].get("created_at", ""))

# ---------------------------------------------------------------------------
# Local helpers
# ---------------------------------------------------------------------------

# _to_int parses a decimal string to int. Returns 0 for None, empty, or
# non-numeric input. (lib.star also has one; kept local so this resolver
# script stands alone against lib changes.)
def _to_int(s):
    if s == None:
        return 0
    if type(s) == "int":
        return s
    if type(s) == "float":
        return int(s)
    if s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# _is_http_url reports whether the url is an absolute http(s) URL.
def _is_http_url(url):
    return url.startswith("http://") or url.startswith("https://")
