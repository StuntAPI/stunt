# Segment handler — Braze REST API.
#
# GET /segments/list → list segments

def on_list_segments(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    page, next_cursor = _list_page(req, _SEGMENTS)
    body = {
        "message": "success",
        "segments": page,
    }
    if next_cursor != None:
        body["next_cursor"] = next_cursor
    return respond(200, body)
