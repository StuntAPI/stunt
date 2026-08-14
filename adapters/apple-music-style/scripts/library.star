# Library handlers — Apple Music API user library endpoint.
#
# GET /v1/me/library/songs → user library songs (requires Music-User-Token)

# on_library_songs returns the user's library songs. This endpoint requires
# BOTH the developer JWT (Bearer) AND the user music token (Music-User-Token).
def on_library_songs(req):
    token, err = _require_jwt(req)
    if err != None:
        return err

    # Library endpoints also require the Music-User-Token header.
    umt = _user_music_token(req)
    if umt == None:
        return _err(401, "Music-User-Token is required for library access.")

    _seed()

    lc = store_collection("library_songs")
    items = lc.list()
    data = []
    for song in items:
        data.append({
            "id": song.get("id"),
            "type": "library-songs",
            "attributes": {
                "name": song.get("name", ""),
                "artistName": song.get("artistName", ""),
                "albumName": song.get("albumName", ""),
                "artwork": song.get("artwork", {}),
                "genreNames": song.get("genreNames", []),
            },
        })

    # Real list params: limit/offset page and fields[library-songs]
    # projects the attributes. total reflects the count before slicing.
    total = len(data)
    limit = _to_int(_get_query(req, "limit"))
    offset = _to_int(_get_query(req, "offset"))
    if limit > 0 or offset > 0:
        data = query_select(data, None, "", "", limit if limit > 0 else None, offset, None)

    fields_param = _get_query(req, "fields[library-songs]")
    if fields_param != "":
        wanted = []
        for part in fields_param.split(","):
            part = part.strip()
            if part != "":
                wanted.append(part)
        if len(wanted) > 0:
            out = []
            for it in data:
                attrs = it.get("attributes", {})
                new_attrs = {}
                for k in wanted:
                    if k in attrs:
                        new_attrs[k] = attrs[k]
                out.append({
                    "id": it.get("id"),
                    "type": it.get("type"),
                    "attributes": new_attrs,
                })
            data = out

    return respond(200, {"data": data, "meta": {"total": total}})
