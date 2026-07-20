# Catalog handlers — Apple Music API catalog endpoints.
#
# GET /v1/catalog/{storefront}/songs/{id}      → song resource
# GET /v1/catalog/{storefront}/albums/{id}     → album resource
# GET /v1/catalog/{storefront}/search?term=&types=songs → search results

# on_get_song returns a single song resource by id.
def on_get_song(req):
    token, err = _require_jwt(req)
    if err != None:
        return err

    _seed()

    song_id = req["params"]["id"]
    sc = store_collection("songs")
    for song in sc.list():
        if song.get("id") == song_id:
            return _ok([_song_resource(song)])

    return _not_found("songs", song_id)

# on_get_album returns a single album resource by id.
def on_get_album(req):
    token, err = _require_jwt(req)
    if err != None:
        return err

    _seed()

    album_id = req["params"]["id"]
    ac = store_collection("albums")
    for album in ac.list():
        if album.get("id") == album_id:
            return _ok([_album_resource(album)])

    return _not_found("albums", album_id)

# on_search searches the catalog by term.
def on_search(req):
    token, err = _require_jwt(req)
    if err != None:
        return err

    _seed()

    term = req["query"].get("term", "")
    if term == None:
        term = ""
    types = req["query"].get("types", "songs")
    if types == None:
        types = "songs"

    term_lower = term.lower()
    results = {"data": []}

    # Search songs.
    if types == "songs" or _contains(types, "songs"):
        sc = store_collection("songs")
        for song in sc.list():
            attrs = song.get("attributes", {})
            name = attrs.get("name", "").lower()
            artist = attrs.get("artistName", "").lower()
            album = attrs.get("albumName", "").lower()
            if term_lower == "" or _contains(name, term_lower) or _contains(artist, term_lower) or _contains(album, term_lower):
                results["data"].append(_song_resource(song))

    # Search albums.
    if types == "albums" or _contains(types, "albums"):
        ac = store_collection("albums")
        for album in ac.list():
            attrs = album.get("attributes", {})
            name = attrs.get("name", "").lower()
            artist = attrs.get("artistName", "").lower()
            if term_lower == "" or _contains(name, term_lower) or _contains(artist, term_lower):
                results["data"].append(_album_resource(album))

    results["meta"] = {"results": {"order": ["songs", "albums"]}}
    return respond(200, results)

# _song_resource builds the API response shape for a song.
def _song_resource(song):
    return {
        "id": song.get("id"),
        "type": "songs",
        "attributes": song.get("attributes", {}),
        "href": "/v1/catalog/us/songs/" + song.get("id", ""),
    }

# _album_resource builds the API response shape for an album.
def _album_resource(album):
    return {
        "id": album.get("id"),
        "type": "albums",
        "attributes": album.get("attributes", {}),
        "href": "/v1/catalog/us/albums/" + album.get("id", ""),
    }
