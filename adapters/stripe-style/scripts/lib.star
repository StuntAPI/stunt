# Shared library for stripe-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# Mock webhook signing secret. Configure your Stripe webhook receiver with
# this exact string to verify stunt's deliveries. Public + low-entropy: local
# stunt only — never reuse outside the simulator.
_WEBHOOK_SECRET = "whsec_stunt_mock_0123456789abcdef0123456789abcdef"

# _signed_emit MACs the exact on-wire body and delivers with Stripe-Signature.
# The same (event_type, payload) feeds events_body (signing input) and
# events_emit (delivery), so the signature verifies against the bytes the sink
# receives. Stripe signs "{timestamp}.{body}" and carries t=,v1= in the header.
def _signed_emit(event_type, payload):
    t = clock.now_unix()
    body = events_body(event_type, payload)
    sig = crypto.hmac_sha256(_WEBHOOK_SECRET, str(t) + "." + body)
    events_emit(event_type, payload, {"Stripe-Signature": "t=" + str(t) + ",v1=" + sig})

# _bearer_token extracts the bearer token from the Authorization header, or
# None if absent.
def _bearer_token(req):
    headers = req.get("headers")
    if headers == None:
        return None
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return None

# _require_auth validates the bearer token.
#
# Returns None if authorized, or an error-response dict to return from the
# handler if not.
#
# Dev bypass: tokens starting with "sk_test" are accepted WITHOUT
# identity_validate, for frictionless local testing.
def _require_auth(req):
    token = _bearer_token(req)
    if token == None:
        return respond(401, {"error": {"type": "authentication_error", "message": "Missing Authorization header. Provide 'Authorization: Bearer <token>'."}})

    # Dev bypass: sk_test tokens skip real validation.
    if token.startswith("sk_test"):
        return None

    # Real validation via the identity issuer.
    claims = identity_validate(token)
    if claims == None:
        return respond(401, {"error": {"type": "authentication_error", "message": "Invalid API Key provided."}})
    return None

# _next_id returns a monotonically-increasing provider-style ID using the
# KV store as a sequence counter. Produces ids like "ch_1", "ch_2", ...
def _next_id(prefix):
    # Atomic increment via store_kv_incr (race-free under concurrent requests).
    return prefix + "_" + str(store_kv_incr("stripe", prefix + "_seq"))

# _to_int parses a decimal string to int. Returns 0 for None, empty string,
# or any non-numeric input (never crashes on None).
def _to_int(s):
    if s == None:
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return n
    return n

# _stripe_account extracts the Stripe-Account header used by Stripe Connect
# to scope requests to a connected account. Returns None if absent.
def _stripe_account(req):
    headers = req.get("headers")
    if headers == None:
        return None
    acct = headers.get("Stripe-Account", "")
    if acct == None or acct == "":
        return None
    return acct

# _get_balance returns the available balance (in cents) for a connected
# account, tracked via the KV store. Defaults to 0 for new accounts.
def _get_balance(acct_id):
    val = store_kv_get("stripe", "bal_" + acct_id)
    return _to_int(val)

# _set_balance sets the available balance (in cents) for a connected account.
def _set_balance(acct_id, amount):
    store_kv_set("stripe", "bal_" + acct_id, str(amount))

# _not_found returns a standard Stripe-style 404 error response.
def _not_found(resource, id):
    return respond(404, {"error": {"message": "No such " + resource + ": " + id, "type": "invalid_request_error"}})

# _list_page applies Stripe cursor pagination (limit + starting_after) to a list
# of docs via the paginate builtin. Returns (page, has_more). Stripe does not
# echo a cursor: the client sets starting_after to the last returned id next time.
_STRIPE_DEFAULT_LIMIT = 10
_STRIPE_MAX_LIMIT = 100

def _list_page(req, docs):
    limit = _STRIPE_DEFAULT_LIMIT
    offset = ""
    q = req.get("query")
    if q != None:
        n = _to_int(q.get("limit", ""))
        if n > 0:
            limit = n
    if limit > _STRIPE_MAX_LIMIT:
        limit = _STRIPE_MAX_LIMIT
    if q != None:
        sa = q.get("starting_after", "")
        if sa != None and sa != "":
            for i in range(len(docs)):
                if docs[i].get("id") == sa:
                    offset = str(i + 1)
                    break
    page, nxt = paginate(docs, limit, offset)
    return page, nxt != None
