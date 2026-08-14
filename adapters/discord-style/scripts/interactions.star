# Interactions handler — Discord slash-command webhook (inbound).
#
# POST /interactions — Discord sends signed interactions here. The adapter
# verifies the Ed25519 signature (X-Signature-Ed25519 + X-Signature-Timestamp)
# over timestamp + raw body, returning a type 1 PONG for a ping or a deferred
# type 5 ack. A bad signature → 401.

def on_interactions(req):
    headers = req.get("headers")
    if headers == None:
        headers = {}
    sig = headers.get("X-Signature-Ed25519", "")
    ts = headers.get("X-Signature-Timestamp", "")
    if sig == None:
        sig = ""
    if ts == None:
        ts = ""
    raw = req.get("raw_body", "")
    if raw == None:
        raw = ""

    if sig == "" or ts == "":
        return respond(401, {"error": "missing signature"})

    if not crypto.ed25519_verify(_ED25519_PUBLIC_KEY, ts + raw, sig, encoding="hex"):
        return respond(401, {"error": "invalid request signature"})

    # PONG for a ping (type 1); deferred ack otherwise.
    body = req["body"]
    if body != None and body.get("type", 0) == 1:
        return respond(200, {"type": 1})
    return respond(200, {"type": 5})
