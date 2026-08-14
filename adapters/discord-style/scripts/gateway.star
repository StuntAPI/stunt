# WebSocket Gateway handler — Discord gateway handshake + event dispatch.
#
# Connection lifecycle:
#   1. Server sends HELLO (op 10) with heartbeat_interval.
#   2. Client sends IDENTIFY (op 2) — awaited, validated leniently.
#   3. Server sends READY (op 0, t READY) with the bot user + session.
#   4. Server dispatches a synthetic MESSAGE_CREATE (op 0, t MESSAGE_CREATE)
#      so a gateway client immediately receives an event.

def on_connect(ws):
    # 1. HELLO.
    ws.send({"op": 10, "d": {"heartbeat_interval": 41250, "_trace": ["stunt-gateway"]}})

    # 2. Await IDENTIFY (op 2). Accept any frame; reject a missing one.
    ident = ws.recv()
    if ident == None:
        return

    # 3. READY.
    ws.send({
        "op": 0,
        "t": "READY",
        "s": 0,
        "d": {
            "v": 10,
            "user": _bot_user(),
            "guilds": [],
            "session_id": "stunt-session-1",
            "resume_gateway_url": "wss://stunt.example/?v=10&encoding=json",
            "_trace": ["stunt-gateway"],
        },
    })

    # 4. Dispatch a synthetic MESSAGE_CREATE.
    ws.send({"op": 0, "t": "MESSAGE_CREATE", "s": 1, "d": _synthetic_message()})
