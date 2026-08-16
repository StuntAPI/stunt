# Library handlers — Apple Music API user (/v1/me) surface.
#
# GET    /v1/me/library/songs              → library songs (limit/offset)
# GET    /v1/me/library/albums             → library albums
# GET    /v1/me/library/playlists          → library playlists
# GET    /v1/me/library/recently-added     → newest library additions (mixed)
# POST   /v1/me/library                    → add catalog resources (204)
# DELETE /v1/me/library/{type}/{id}        → remove a library resource (204)
# POST   /v1/me/played                     → mark a song played (204)
# GET    /v1/me/ratings/{type}/{id}        → rating for a resource
# PUT    /v1/me/ratings/{type}/{id}        → love/dislike/clear a resource
#
# All /v1/me endpoints require BOTH the developer JWT (Authorization: Bearer)
# and the Music-User-Token header, like the real API.

# on_library_songs returns the user's library songs.
def on_library_songs(req):
    umt, err = _require_user(req)
    if err != None:
        return err
    _seed()

    docs = store_collection("library_songs").list()
    wanted = _fields_param(req, "library-songs")
    limit, offset = _limit_offset(req, 100, 100)
    total = len(docs)
    resources = []
    for d in docs:
        r = _library_song_resource(d)
        r["attributes"] = _project_attributes(r.get("attributes", {}), wanted)
        resources.append(r)
    page, next_off = _page_via_paginate(resources, limit, offset)

    out = {"data": page, "meta": {"total": total}}
    if next_off != None:
        out["next"] = "/v1/me/library/songs?limit=" + str(limit) + "&offset=" + str(next_off)
    return respond(200, out)

# on_library_albums returns the user's saved albums.
def on_library_albums(req):
    umt, err = _require_user(req)
    if err != None:
        return err
    sf, sf_err = _storefront(req)
    if sf_err != None:
        return sf_err
    _seed()

    docs = store_collection("library_albums").list()
    limit, offset = _limit_offset(req, 100, 100)
    total = len(docs)
    resources = []
    for d in docs:
        resources.append(_library_album_resource(d, sf))
    page, next_off = _page_via_paginate(resources, limit, offset)

    out = {"data": page, "meta": {"total": total}}
    if next_off != None:
        out["next"] = "/v1/me/library/albums?limit=" + str(limit) + "&offset=" + str(next_off)
    return respond(200, out)

# on_library_playlists returns the user's personal playlists.
def on_library_playlists(req):
    umt, err = _require_user(req)
    if err != None:
        return err
    _seed()

    docs = store_collection("library_playlists").list()
    wanted = _fields_param(req, "library-playlists")
    limit, offset = _limit_offset(req, 100, 100)
    total = len(docs)
    resources = []
    for d in docs:
        resources.append(_library_playlist_resource(d))
    page, next_off = _page_via_paginate(resources, limit, offset)
    if len(wanted) > 0:
        projected = []
        for r in page:
            r["attributes"] = _project_attributes(r.get("attributes", {}), wanted)
            projected.append(r)
        page = projected

    out = {"data": page, "meta": {"total": total}}
    if next_off != None:
        out["next"] = "/v1/me/library/playlists?limit=" + str(limit) + "&offset=" + str(next_off)
    return respond(200, out)

# on_recently_added returns the newest library additions across resource
# types (real response: mixed library-songs/library-playlists/albums, newest
# first, with meta.total).
def on_recently_added(req):
    umt, err = _require_user(req)
    if err != None:
        return err
    sf, sf_err = _storefront(req)
    if sf_err != None:
        return sf_err
    _seed()

    flat = []
    for d in store_collection("library_songs").list():
        flat.append(d)
    for d in store_collection("library_playlists").list():
        flat.append(d)
    for d in store_collection("library_albums").list():
        flat.append(d)

    flat = query_select(flat, None, "_added_unix", "desc", None, None, None)
    resources = []
    for d in flat:
        t = d.get("type", "")
        if t == "library-songs":
            resources.append(_library_song_resource(d))
        elif t == "library-playlists":
            resources.append(_library_playlist_resource(d))
        else:
            resources.append(_library_album_resource(d, sf))

    limit, offset = _limit_offset(req, 100, 100)
    total = len(resources)
    page, next_off = _page_via_paginate(resources, limit, offset)

    out = {"data": page, "meta": {"total": total}}
    if next_off != None:
        out["next"] = "/v1/me/library/recently-added?limit=" + str(limit) + "&offset=" + str(next_off)
    return respond(200, out)

# on_add_library adds catalog resources to the library, like the real
# "Add a Resource to a Library": POST /v1/me/library?ids[songs]=<a,b>&…  The
# ids may also arrive as a JSON body {"ids": {"songs": [..]}}. 204 No Content.
def on_add_library(req):
    umt, err = _require_user(req)
    if err != None:
        return err
    sf, sf_err = _storefront(req)
    if sf_err != None:
        return sf_err
    _seed()

    body = _parse_body(req)
    if body == None:
        return _bad_request("Request body is not a valid JSON object.")

    ids_by_type = {}
    q = req.get("query")
    if q != None:
        for t in ["songs", "albums", "playlists"]:
            raw = q.get("ids[" + t + "]", "")
            if raw != None and raw != "":
                ids_by_type[t] = _split_ids(raw)
    body_ids = body.get("ids", None)
    if body_ids != None and type(body_ids) == "dict":
        for t in body_ids:
            if t == "songs" or t == "albums" or t == "playlists":
                raw = body_ids.get(t, "")
                if raw == None:
                    continue
                if type(raw) == "list":
                    vals = []
                    for v in raw:
                        vals.append(str(v))
                    ids_by_type[t] = vals
                else:
                    ids_by_type[t] = _split_ids(str(raw))

    if len(ids_by_type) == 0:
        return _bad_request("Provide catalog resource ids via ids[songs], ids[albums] or ids[playlists].")

    now = clock.now_unix()
    date_added = clock.now_rfc3339()[:10]
    for t in ids_by_type:
        for ident in ids_by_type[t]:
            if t == "songs":
                song = _find_catalog_song(ident)
                if song == None:
                    return _not_found("songs", ident)
                if _find_library_doc("library_songs", ident) == None:
                    store_collection("library_songs").insert(
                        _library_song_doc(_next_lib_id("i", "library_songs"), ident, date_added, 0))
            elif t == "albums":
                if _find_catalog_album(ident) == None:
                    return _not_found("albums", ident)
                if _find_library_doc("library_albums", ident) == None:
                    store_collection("library_albums").insert({
                        "id": _next_lib_id("i", "library_albums"),
                        "type": "library-albums",
                        "catalogId": ident,
                        "dateAdded": date_added,
                        "_added_unix": now,
                    })
            else:
                found = None
                for p in store_collection("catalog_playlists").list():
                    if p.get("id") == ident:
                        found = p
                if found == None:
                    return _not_found("playlists", ident)
                if _find_library_doc("library_playlists", ident) == None:
                    store_collection("library_playlists").insert({
                        "id": _next_lib_id("p", "library_playlists"),
                        "type": "library-playlists",
                        "name": found.get("attributes", {}).get("name", ""),
                        "description": "",
                        "canEdit": False,
                        "dateAdded": date_added,
                        "trackIds": found.get("trackIds", []),
                        "_added_unix": now,
                    })
    return respond(204)

# on_delete_library removes a library resource by type + id (204; 404 when
# the resource is not in the library).
def on_delete_library(req):
    umt, err = _require_user(req)
    if err != None:
        return err
    _seed()

    t = req["params"].get("type", "")
    ident = req["params"].get("id", "")
    collection = None
    if t == "songs":
        collection = "library_songs"
    elif t == "albums":
        collection = "library_albums"
    elif t == "playlists":
        collection = "library_playlists"
    else:
        return _bad_request("Unsupported library resource type: " + t)

    doc = _find_library_doc(collection, ident)
    if doc == None:
        return _not_found(t, ident)
    store_collection(collection).delete(doc.get("id", ""))
    return respond(204)

# on_played marks a song as played: POST /v1/me/played with
# {"id": "<catalog song id>", "type": "songs"}. Bumps playCount, stamps
# lastPlayed, and saves the song to the library if needed. 204 No Content.
def on_played(req):
    umt, err = _require_user(req)
    if err != None:
        return err
    _seed()

    body = _parse_body(req)
    if body == None:
        return _bad_request("Request body is not a valid JSON object.")

    t = body.get("type", "songs")
    if t == None:
        t = "songs"
    ident = body.get("id", "")
    if ident == None:
        ident = ""
    if ident == "":
        ident = _get_query(req, "id")
    if ident == "":
        return _bad_request("id is required.")
    if t != "songs":
        return _bad_request("Unsupported played resource type: " + t)

    song = _find_catalog_song(str(ident))
    if song == None:
        return _not_found("songs", str(ident))

    doc = _find_library_doc("library_songs", str(ident))
    now = clock.now_unix()
    if doc == None:
        doc = _library_song_doc(_next_lib_id("i", "library_songs"), str(ident), clock.now_rfc3339()[:10], 0)
        doc["playCount"] = 1
        doc["lastPlayed"] = clock.unix_to_rfc3339(now)
        doc["_added_unix"] = now
        store_collection("library_songs").insert(doc)
        return respond(204)

    doc["playCount"] = _num(doc.get("playCount", 0)) + 1
    doc["lastPlayed"] = clock.unix_to_rfc3339(now)
    store_collection("library_songs").update(doc.get("id", ""), doc)
    return respond(204)

# --- ratings ---

_RATING_TYPES = ["songs", "albums", "playlists"]

# on_get_rating returns the personal rating for a resource (200 with
# attributes.value — 0 when never rated; 404 when the resource itself does
# not exist).
def on_get_rating(req):
    umt, err = _require_user(req)
    if err != None:
        return err
    _seed()

    t = req["params"].get("type", "")
    ident = req["params"].get("id", "")
    bad = _rating_target_error(t, ident)
    if bad != None:
        return bad

    rc = store_collection("ratings")
    doc = rc.get(t + ":" + ident)
    value = 0
    if doc != None:
        value = doc.get("value", 0)
    return respond(200, {"data": [_rating_resource(t, ident, value)]})

# on_put_rating creates or updates the personal rating for a resource (real
# body: {"type": "ratings", "id": "<id>", "attributes": {"value": 1}} with
# value -1 (dislike), 0 (clear) or 1 (love); 201 Created on success).
def on_put_rating(req):
    umt, err = _require_user(req)
    if err != None:
        return err
    _seed()

    t = req["params"].get("type", "")
    ident = req["params"].get("id", "")
    bad = _rating_target_error(t, ident)
    if bad != None:
        return bad

    body = _parse_body(req)
    if body == None:
        return _bad_request("Request body is not a valid JSON object.")
    attrs = body.get("attributes", None)
    if attrs == None or type(attrs) != "dict":
        return _bad_request("attributes with a value is required.")
    value = attrs.get("value", None)
    if value == None:
        return _bad_request("attributes.value is required.")
    if type(value) == "float":
        value = int(value)
    if type(value) != "int" or (value != -1 and value != 0 and value != 1):
        return _bad_request("attributes.value must be -1, 0 or 1.")

    rc = store_collection("ratings")
    key = t + ":" + ident
    doc = rc.get(key)
    if doc == None:
        rc.insert({"id": key, "resourceType": t, "resourceId": ident, "value": value})
    else:
        doc["value"] = value
        rc.update(key, doc)
    return respond(201, {"data": [_rating_resource(t, ident, value)]})

# _rating_target_error validates the rating path target: known type, and a
# resource that exists in the catalog. Returns the error response or None.
def _rating_target_error(t, ident):
    found_type = False
    for allowed in _RATING_TYPES:
        if t == allowed:
            found_type = True
    if not found_type:
        return _bad_request("Unsupported rating resource type: " + t)
    if t == "songs" and _find_catalog_song(ident) == None:
        return _not_found("songs", ident)
    if t == "albums" and _find_catalog_album(ident) == None:
        return _not_found("albums", ident)
    if t == "playlists":
        found = None
        for p in store_collection("catalog_playlists").list():
            if p.get("id") == ident:
                found = p
        for p in store_collection("library_playlists").list():
            if p.get("id") == ident:
                found = p
        if found == None:
            return _not_found("playlists", ident)
    return None

# --- shared helpers ---

# _split_ids splits a comma-separated id list into stripped parts.
def _split_ids(raw):
    out = []
    for part in raw.split(","):
        part = part.strip()
        if part != "":
            out.append(part)
    return out
