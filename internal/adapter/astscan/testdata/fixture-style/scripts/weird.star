# Parse-mode regression: while parses at mode 0 (the resolver would
# reject it; the VM allows it).

def on_loop(req):
    q = req["query"]
    i = 0
    while i < 3:
        i = i + 1
    return respond(200, {"q": q, "i": i})
