# WalletConnect v2 relay handler — pairing + session lifecycle.
#
# This models the dApp-facing surface of the WC relay. The KEY VALUE is
# auto-pairing and auto-approving: a dApp can test its WC integration
# WITHOUT a second device or QR scan.
#
# POST   /v1/pairings                        -> establish pairing
# POST   /v1/sessions                        -> propose session
# GET    /v1/sessions                        -> list sessions
# POST   /v1/sessions/{topic}/approve        -> acknowledge session
# POST   /v1/sessions/{topic}/request        -> wallet RPC request
# POST   /v1/sessions/{topic}/extend         -> refresh expiry
# DELETE /v1/sessions/{topic}                -> disconnect

# Shared helpers are preloaded from scripts/lib.star.

# =====================================================================
# PAIRING
# =====================================================================

# on_create_pairing establishes a pairing from either a wc: URI or an
# explicit topic. Returns the pairing object.
def on_create_pairing(req):
    body = req.get("body")
    if body == None:
        body = {}

    uri = body.get("uri", None)

    if uri != None and uri != "":
        # Parse the wc: URI to extract the topic and symKey.
        parsed = _parse_wc_uri(uri)
        if parsed == None:
            return respond(400, {"error": "invalid_uri", "message": "could not parse wc: URI"})
        topic = parsed["topic"]
        sym_key = parsed["symKey"]
    else:
        # No URI — generate a topic from a sequence number.
        seq = store_kv_incr("wc", "pairing_seq")
        topic = _topic("pairing-" + str(seq))
        sym_key = ""

    pc = store_collection("pairings")
    doc = {
        "topic": topic,
        "relay": {"protocol": "irn"},
        "expiry": PAIRING_EXPIRY,
        "state": {"symKey": sym_key},
    }
    for k in doc:
        pass
    pc.insert(doc)

    return respond(200, doc)

# =====================================================================
# SESSIONS
# =====================================================================

# on_propose_session proposes a new session from a pairing.
def on_propose_session(req):
    body = req.get("body")
    if body == None:
        body = {}

    pairing_topic = body.get("pairingTopic", None)
    if pairing_topic == None or pairing_topic == "":
        return respond(400, {"error": "missing_pairingTopic", "message": "pairingTopic is required"})

    required_namespaces = body.get("requiredNamespaces", {})
    if required_namespaces == None:
        required_namespaces = {}

    # Generate a session topic.
    seq = store_kv_incr("wc", "session_seq")
    topic = _topic("session-" + str(seq))

    session = {
        "topic": topic,
        "acknowledged": False,
        "pairingTopic": pairing_topic,
        "requiredNamespaces": required_namespaces,
        "namespaces": {},
        "expiry": SESSION_EXPIRY,
    }

    sc = store_collection("sessions")
    sc.insert(session)

    return respond(200, {
        "topic": topic,
        "acknowledged": False,
        "pairingTopic": pairing_topic,
        "namespaces": {},
        "expiry": SESSION_EXPIRY,
    })

# on_list_sessions returns all active sessions as an array.
def on_list_sessions(req):
    sc = store_collection("sessions")
    result = []
    for s in sc.list():
        result.append(_session_view(s))
    return respond(200, result)

# on_approve_session acknowledges (approves) a session, simulating the
# wallet's approval response.
def on_approve_session(req):
    topic = req["params"]["topic"]

    sc = store_collection("sessions")
    session = _find_by_topic(sc, topic)
    if session == None:
        return respond(404, {"error": "session_not_found", "message": "session " + topic + " not found"})

    # Build the approved namespaces with mock accounts.
    namespaces = _build_namespaces(session)

    sc.update(session["id"], {
        "topic": topic,
        "acknowledged": True,
        "pairingTopic": session.get("pairingTopic", ""),
        "requiredNamespaces": session.get("requiredNamespaces", {}),
        "namespaces": namespaces,
        "expiry": session.get("expiry", SESSION_EXPIRY),
    })

    return respond(200, {
        "topic": topic,
        "acknowledged": True,
        "namespaces": namespaces,
    })

# on_session_request handles a wallet JSON-RPC request (e.g.
# eth_requestAccounts, personal_sign, eth_sendTransaction).
def on_session_request(req):
    topic = req["params"]["topic"]

    sc = store_collection("sessions")
    session = _find_by_topic(sc, topic)
    if session == None:
        return respond(404, {"error": "session_not_found", "message": "session " + topic + " not found"})

    body = req.get("body")
    if body == None:
        body = {}

    request_obj = body.get("request", {})
    if request_obj == None:
        request_obj = {}

    method = request_obj.get("method", "")
    if method == None:
        method = ""
    params = request_obj.get("params", [])
    if params == None:
        params = []

    # Generate a deterministic request ID.
    req_id = store_kv_incr("wc", "request_seq")

    # Auto-approve based on method.
    if method == "eth_requestAccounts":
        result = [WALLET_ADDRESS]
    elif method == "personal_sign":
        result = _deterministic_hash("personal_sign_" + str(req_id))
    elif method == "eth_sendTransaction":
        result = _deterministic_hash("eth_sendTransaction_" + str(req_id))
    elif method == "eth_accounts":
        result = [WALLET_ADDRESS]
    elif method == "eth_sign":
        result = _deterministic_hash("eth_sign_" + str(req_id))
    else:
        # Default: return a synthetic result.
        result = _deterministic_hash("result_" + str(req_id))

    return respond(200, {
        "topic": topic,
        "id": req_id,
        "jsonrpc": "2.0",
        "result": result,
    })

# on_extend_session refreshes the session expiry.
def on_extend_session(req):
    topic = req["params"]["topic"]

    sc = store_collection("sessions")
    session = _find_by_topic(sc, topic)
    if session == None:
        return respond(404, {"error": "session_not_found", "message": "session " + topic + " not found"})

    return respond(200, {
        "topic": topic,
        "expiry": SESSION_EXPIRY,
    })

# on_disconnect_session disconnects (deletes) a session.
def on_disconnect_session(req):
    topic = req["params"]["topic"]

    sc = store_collection("sessions")
    session = _find_by_topic(sc, topic)
    if session == None:
        return respond(404, {"error": "session_not_found", "message": "session " + topic + " not found"})

    sc.delete(session["id"])

    return respond(200, {
        "topic": topic,
        "acknowledged": False,
        "message": "session disconnected",
    })

# =====================================================================
# HELPERS
# =====================================================================

# _find_by_topic finds a session (or pairing) document by its topic.
def _find_by_topic(collection, topic):
    for s in collection.list():
        if s.get("topic", "") == topic:
            return s
    return None

# _session_view builds the public view of a session for listing.
def _session_view(s):
    return {
        "topic": s.get("topic", ""),
        "acknowledged": s.get("acknowledged", False),
        "pairingTopic": s.get("pairingTopic", ""),
        "namespaces": s.get("namespaces", {}),
        "expiry": s.get("expiry", SESSION_EXPIRY),
    }

# _build_namespaces constructs the approved namespaces object with mock
# accounts, methods, and events for the eip155 namespace.
def _build_namespaces(session):
    rn = session.get("requiredNamespaces", {})
    chains = ["eip155:1"]
    methods = ["eth_sendTransaction", "personal_sign"]
    events = ["chainChanged", "accountsChanged"]

    if "eip155" in rn:
        eip = rn["eip155"]
        if eip != None:
            c = eip.get("chains", [])
            if c != None and len(c) > 0:
                chains = c
            m = eip.get("methods", [])
            if m != None and len(m) > 0:
                methods = m
            ev = eip.get("events", [])
            if ev != None and len(ev) > 0:
                events = ev

    # Build accounts list: eip155:chainId:address
    accounts = []
    for chain in chains:
        accounts.append(chain + ":" + WALLET_ADDRESS)

    return {
        "eip155": {
            "accounts": accounts,
            "methods": methods,
            "events": events,
        }
    }
