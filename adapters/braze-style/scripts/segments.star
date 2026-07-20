# Segment handler — Braze REST API.
#
# GET /segments/list → list segments

def on_list_segments(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    return respond(200, {
        "message": "success",
        "segments": _SEGMENTS,
    })
