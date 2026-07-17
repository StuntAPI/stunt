# Echo handlers — gRPC handlers backed by store_collection + store_kv.
#
# Each handler receives `req` with keys: method, path, headers, body.
# For gRPC calls, method is "GRPC", path is the full gRPC method path
# (e.g. "/stunt.example.Echo/Say"), and body is the decoded request map
# (field names are the protobuf field names, e.g. "message").
# Returns respond(status, body) where body is the gRPC response map.

# Say echoes the request message back and records it. Returns the echoed
# message plus a running echo_count (total Say calls including this one).
def on_say(req):
    body = req.get("body")
    if body == None:
        body = {}
    message = body.get("message", "")

    # Record this echo in the collection.
    c = store_collection("echoes")
    c.insert({"message": message})

    # Atomic counter for total Say calls.
    echo_count = store_kv_incr("echo", "say_count")

    return respond(200, {"message": message, "echo_count": echo_count})

# Add accumulates integer values and returns the running total + count.
# gRPC int32 fields arrive as numbers (float in Starlark); convert to int.
def on_add(req):
    body = req.get("body")
    if body == None:
        body = {}
    value = body.get("value", 0)
    # Convert to int — protobuf int32 comes through as a float from JSON.
    value = int(value)

    # Accumulate into the KV store (get/set for arbitrary values).
    prev = store_kv_get("echo", "add_total")
    if prev == None:
        total = value
    else:
        total = int(prev) + value
    store_kv_set("echo", "add_total", str(total))

    # Atomic counter for the number of Add calls.
    count = store_kv_incr("echo", "add_count")

    return respond(200, {"total": total, "count": count})

# ListEchoes returns all messages previously recorded by Say.
def on_list_echoes(req):
    c = store_collection("echoes")
    docs = c.list()

    echoes = []
    for doc in docs:
        echoes.append({"message": doc.get("message", "")})

    return respond(200, {"echoes": echoes})

# StreamEcho is a server-streaming RPC: it reads the request message and
# streams back N synthetic replies. Uses the store_kv counter to track the
# total number of StreamEcho calls (stateful).
def on_stream_echo(stream):
    req = stream.recv()
    message = ""
    if req != None and "message" in req:
        message = req["message"]

    # Count this StreamEcho call (stateful across all gRPC handlers).
    echo_count = store_kv_incr("echo", "say_count")

    # Stream back 3 synthetic replies.
    for i in range(3):
        stream.send({"message": message, "echo_count": echo_count + i})
