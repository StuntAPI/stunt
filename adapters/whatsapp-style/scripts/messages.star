# Message handlers — send text + template messages.
#
# POST /v21.0/{phone_number_id}/messages
#   {messaging_product:"whatsapp", to, type:"text", text:{body:"..."}}
#   -> {messaging_product:"whatsapp", contacts:[{input, wa_id}], messages:[{id}]}
#
# Supports:
#   - type:"text"     → {text:{body:"..."}}
#   - type:"template" → {template:{name, language:{code}}}
#
# Requires Bearer access token.
#
# 24H WINDOW RULE: Outside the 24-hour customer service window, only APPROVED
# templates are allowed (documented in lib.star). This adapter does not
# enforce the window by default.

# Shared helpers (_require_auth, _wa_unauthorized, _next_msg_id,
# _normalize_phone, _now, _seed) are preloaded from scripts/lib.star.

# on_send_message sends a WhatsApp message.
def on_send_message(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    phone_number_id = req["params"]["phone_number_id"]
    body = req["body"]
    if body == None:
        body = {}

    to = body.get("to", "")
    if to == None:
        to = ""
    msg_type = body.get("type", "text")
    if msg_type == None:
        msg_type = "text"

    msg_id = _next_msg_id()
    wa_id = _normalize_phone(to)

    # Store the message record.
    msg = {
        "id": msg_id,
        "phone_number_id": phone_number_id,
        "to": to,
        "wa_id": wa_id,
        "type": msg_type,
        "status": "sent",
        "created_at": _now(),
    }
    if msg_type == "text":
        text_body = body.get("text", {})
        if text_body == None:
            text_body = {}
        msg["text"] = text_body.get("body", "")
    elif msg_type == "template":
        tmpl = body.get("template", {})
        if tmpl == None:
            tmpl = {}
        msg["template_name"] = tmpl.get("name", "")

    mc = store_collection("messages")
    mc.insert(msg)

    # Emit inbound-message-style webhook event.
    events_emit("messages", {"from": wa_id, "id": msg_id})

    return respond(200, {
        "messaging_product": "whatsapp",
        "contacts": [{"input": to, "wa_id": wa_id}],
        "messages": [{"id": msg_id}],
    })
