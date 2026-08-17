# Automation handlers — /automations/{automation_id}/queue.
#
#   POST /automations/{automation_id}/queue   start an automation for a contact
#
# The real API answers 204 No Content on success. Automations themselves are
# dashboard-authored (no API endpoint creates them), so this simulator
# validates the shape of the automation id (a UUID — a malformed one answers
# 404 like the real API) and that the referenced contact exists in some list,
# then records the queue entry. Body: {"contact_id": <id or email hash>}.
#
# Shared helpers are preloaded from scripts/lib.star.

# on_queue_automation answers POST /automations/{automation_id}/queue.
def on_queue_automation(req):
    err = _require_auth(req)
    if err != None:
        return err

    automation_id = _param(req, "automation_id")
    if _is_uuid_shape(automation_id) == False:
        return _not_found()

    body = _parse_body(req)
    if body == None:
        return _bad_request()

    contact_id = _str_or_none(body.get("contact_id", None))
    if contact_id == None or contact_id == "":
        return _unprocessable([_verr("/contact_id", "This value should not be blank.")])

    # The documented contact_id is "the ID of the contact, or an MD5 hash of
    # the lowercase version of the contact's email address" — which is exactly
    # how this adapter mints contact ids, so one lookup resolves both. Rows
    # are keyed per list (the same email hash can be a contact of several
    # lists), so existence is "any list carries this contact".
    found = False
    for c in store_collection("contacts").list():
        if c.get("contact_id", "") == contact_id:
            found = True
            break
    if not found:
        return _not_found()

    # Persist BEFORE emitting. Queue entries are keyed (automation, contact)
    # so a repeat request is a no-op rather than a duplicate row — the
    # automation.queued event still fires exactly once per new entry.
    qc = store_collection("automation_queue")
    key = automation_id + ":" + contact_id
    if qc.get(key) == None:
        qc.insert({
            "id": key,
            "automation_id": automation_id,
            "contact_id": contact_id,
            "queued_at": _iso_now(),
        })
        _emit("automation.queued", {
            "automation_id": automation_id,
            "contact_id": contact_id,
        })
    return respond(204)
