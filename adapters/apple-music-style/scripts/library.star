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

    return respond(200, {"data": data, "meta": {"total": len(data)}})
