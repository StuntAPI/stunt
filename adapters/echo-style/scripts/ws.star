# WebSocket on_connect handler — synthetic echo over WebSocket.
#
# Invoked once per WS connection. Loops on ws.recv(), echoing each message
# back via ws.send() and incrementing a global kv counter (ws_echo_count) to
# demonstrate stateful builtin integration. When recv() returns None (client
# disconnect), the handler returns cleanly.
#
# All data is synthetic — the handler only echoes what the client sends.

def on_connect(ws):
    # Echo loop: read messages until the client disconnects.
    while True:
        m = ws.recv()
        if m == None:
            # Client closed the connection — exit cleanly.
            break

        # Increment the global WS echo counter (stateful across connections).
        store_kv_incr("echo", "ws_echo_count")

        # Echo the message back unchanged.
        ws.send(m)
