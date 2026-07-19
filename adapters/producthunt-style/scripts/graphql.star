# GraphQL handler — single endpoint, pattern-matches the operation name.
#
# POST /v2/api/graphql.json (Bearer)
#   body: { query: "...", variables: { name, tagline, description, url } }
#
# This is NOT a full GraphQL engine. It pattern-matches the operation name
# in the query string and returns the JSON shape that ***REMOVED***'s
# producthuntAdapter parses. This is the simplest faithful approach: satisfy
# the specific mutations/queries the adapter sends.

# Shared helper (_bearer) is preloaded from scripts/lib.star.

# on_graphql dispatches based on the operation name in the GraphQL query.
def on_graphql(req):
    token = _bearer(req)
    if token == "":
        return respond(401, {
            "errors": [{"message": "You need to sign in or sign up before continuing."}],
        })

    body = req["body"]
    if body == None:
        body = {}
    query = body.get("query", "")

    # Pattern-match the operation name.
    # ***REMOVED***'s adapter sends: mutation Create(...)
    if _contains(query, "postCreate"):
        return _handle_post_create(body)
    if _contains(query, "post"):
        return _handle_post_query(body)

    return respond(200, {"data": {}})

# _handle_post_create processes the postCreate mutation.
#
# ***REMOVED***'s producthuntAdapter sends:
#   mutation Create($name, $tagline, $description, $url) {
#     postCreate(input: { name: $name, ... }) { post { id } errors { message } }
#   }
# and parses the response as:
#   { data: { postCreate: { post: { id }, errors: [] } } }
def _handle_post_create(body):
    variables = body.get("variables", {})
    if variables == None:
        variables = {}

    name = variables.get("name", "")
    tagline = variables.get("tagline", "")
    description = variables.get("description", "")
    url = variables.get("url", "")

    if name == "" or tagline == "" or description == "" or url == "":
        return respond(200, {
            "data": {
                "postCreate": {
                    "post": None,
                    "errors": [{"message": "Name, tagline, description, and url are required"}],
                },
            },
        })

    seq = store_kv_incr("producthunt", "post_seq")
    post_id = str(seq)

    pc = store_collection("posts")
    pc.insert({
        "id": post_id,
        "name": name,
        "tagline": tagline,
        "description": description,
        "url": url,
        "votes_count": 0,
    })

    return respond(200, {
        "data": {
            "postCreate": {
                "post": {"id": post_id},
                "errors": [],
            },
        },
    })

# _handle_post_query processes a post query (used by the metrics adapter:
# post(id){ votesCount }).
def _handle_post_query(body):
    variables = body.get("variables", {})
    if variables == None:
        variables = {}
    post_id = variables.get("id", "")
    if post_id == "":
        return respond(200, {"data": {"post": None}})

    pc = store_collection("posts")
    doc = pc.get(post_id)
    if doc == None:
        return respond(200, {"data": {"post": None}})

    return respond(200, {
        "data": {
            "post": {
                "id": doc.get("id", ""),
                "name": doc.get("name", ""),
                "tagline": doc.get("tagline", ""),
                "votesCount": doc.get("votes_count", 0),
                "url": doc.get("url", ""),
            },
        },
    })

def _contains(s, substr):
    return s.find(substr) >= 0
