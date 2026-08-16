# Catalog handlers — Apple Music API catalog endpoints.
#
# GET /v1/catalog/{storefront}/songs            → list songs (limit/offset)
# GET /v1/catalog/{storefront}/songs/{id}       → song resource
# GET /v1/catalog/{storefront}/albums           → list albums
# GET /v1/catalog/{storefront}/albums/{id}      → album resource
# GET /v1/catalog/{storefront}/artists          → list artists
# GET /v1/catalog/{storefront}/artists/{id}     → artist resource
# GET /v1/catalog/{storefront}/playlists        → list editorial playlists
# GET /v1/catalog/{storefront}/charts           → charts (songs/albums)
# GET /v1/catalog/{storefront}/search           → grouped search results

# _resource_of renders one catalog doc as its public resource shape.
def _resource_of(kind, doc, storefront, with_tracks):
    if kind == "songs":
        return _song_resource(doc, storefront)
    if kind == "albums":
        return _album_resource(doc, storefront)
    if kind == "artists":
        return _artist_resource(doc, storefront)
    return _catalog_playlist_resource(doc, storefront, with_tracks)

# _catalog_docs returns the collection docs for a catalog kind.
def _catalog_docs(kind):
    if kind == "songs":
        return store_collection("songs").list()
    if kind == "albums":
        return store_collection("albums").list()
    if kind == "artists":
        return store_collection("artists").list()
    return store_collection("catalog_playlists").list()

# on_list_songs lists catalog songs (real limit/offset paging + next link).
def on_list_songs(req):
    return _on_list_kind(req, "songs")

# on_list_albums lists catalog albums.
def on_list_albums(req):
    return _on_list_kind(req, "albums")

# on_list_artists lists catalog artists.
def on_list_artists(req):
    return _on_list_kind(req, "artists")

# on_list_playlists lists the editorial playlists (tracks embedded with
# ?include=tracks, like the real API).
def on_list_playlists(req):
    return _on_list_kind(req, "playlists")

# _on_list_kind serves GET /v1/catalog/{storefront}/<kind> with the real
# paged-collection envelope {href, data, next?}.
def _on_list_kind(req, kind):
    token, err = _require_jwt(req)
    if err != None:
        return err
    sf, err = _storefront(req)
    if err != None:
        return err
    _seed()

    with_tracks = _contains(_get_query(req, "include"), "tracks")
    docs = _catalog_docs(kind)
    limit, offset = _limit_offset(req, 20, 100)
    resources = []
    for d in docs:
        resources.append(_resource_of(kind, d, sf, with_tracks))
    page, next_off = _page_via_paginate(resources, limit, offset)

    path = "/v1/catalog/" + sf + "/" + kind
    out = {
        "href": path + "?limit=" + str(limit) + "&offset=" + str(offset),
        "data": page,
    }
    if next_off != None:
        out["next"] = path + "?limit=" + str(limit) + "&offset=" + str(next_off)
    return respond(200, out)

# on_get_song returns a single song resource by id.
def on_get_song(req):
    token, err = _require_jwt(req)
    if err != None:
        return err
    sf, err = _storefront(req)
    if err != None:
        return err
    _seed()

    song_id = req["params"]["id"]
    song = _find_catalog_song(song_id)
    if song == None:
        return _not_found("songs", song_id)
    return _ok([_song_resource(song, sf)])

# on_get_album returns a single album resource by id.
def on_get_album(req):
    token, err = _require_jwt(req)
    if err != None:
        return err
    sf, err = _storefront(req)
    if err != None:
        return err
    _seed()

    album_id = req["params"]["id"]
    album = _find_catalog_album(album_id)
    if album == None:
        return _not_found("albums", album_id)
    return _ok([_album_resource(album, sf)])

# on_get_artist returns a single artist resource by id.
def on_get_artist(req):
    token, err = _require_jwt(req)
    if err != None:
        return err
    sf, err = _storefront(req)
    if err != None:
        return err
    _seed()

    artist_id = req["params"]["id"]
    for artist in store_collection("artists").list():
        if artist.get("id") == artist_id:
            return _ok([_artist_resource(artist, sf)])
    return _not_found("artists", artist_id)

# on_charts returns the storefront charts (songs and albums over the seed),
# shaped like the real charts response: results.<type> = [{chart, name, href,
# order}] with a next link per chart when more entries exist.
def on_charts(req):
    token, err = _require_jwt(req)
    if err != None:
        return err
    sf, err = _storefront(req)
    if err != None:
        return err
    _seed()

    types = _get_query(req, "types")
    if types == "":
        types = "songs,albums"
    wanted = []
    for part in types.split(","):
        part = part.strip()
        if part != "":
            if part != "songs" and part != "albums":
                return _bad_request("Unsupported chart type: " + part)
            wanted.append(part)
    if len(wanted) == 0:
        wanted = ["songs", "albums"]

    limit, offset = _limit_offset(req, 10, 100)

    results = {}
    order = []
    for t in wanted:
        docs = _catalog_docs(t)
        # Charts order deterministically: newest release date first.
        dicts = []
        for d in docs:
            dicts.append({
                "resource": d,
                "_date": d.get("attributes", {}).get("releaseDate", ""),
            })
        dicts = query_select(dicts, None, "_date", "desc", None, None, None)
        page, next_off = _page_via_paginate(dicts, limit, offset)
        chart = []
        for d in page:
            chart.append(_resource_of(t, d["resource"], sf, False))
        entry = {
            "chart": chart,
            "name": "Top Songs" if t == "songs" else "Top Albums",
            "href": "/v1/catalog/" + sf + "/charts?types=" + t,
            "order": {"groupOrdinal": 0, "position": 0 if t == "songs" else 1},
        }
        if next_off != None:
            entry["next"] = ("/v1/catalog/" + sf + "/charts?types=" + t +
                             "&limit=" + str(limit) + "&offset=" + str(next_off))
        results[t] = [entry]
        order.append(t)

    return respond(200, {
        "results": results,
        "meta": {"results": {"order": order}},
    })

# on_search searches the catalog by term. The real response groups results by
# resource type: results.<type> = {href, next?, data}, with meta.results.order
# listing the returned groups. Real paging params: limit (default 5, max 25)
# and offset, applied per type group.
def on_search(req):
    token, err = _require_jwt(req)
    if err != None:
        return err
    sf, err = _storefront(req)
    if err != None:
        return err
    _seed()

    term = _get_query(req, "term")
    if term == "":
        return _bad_request("term is required.")
    term_lower = term.lower()

    types = _get_query(req, "types")
    if types == "":
        types = "songs"
    wanted = []
    for part in types.split(","):
        part = part.strip()
        if part != "":
            if part != "songs" and part != "albums" and part != "artists" and part != "playlists":
                return _bad_request("Unsupported search type: " + part)
            wanted.append(part)
    if len(wanted) == 0:
        wanted = ["songs"]

    limit, offset = _limit_offset(req, 5, 25)
    with_tracks = _contains(_get_query(req, "include"), "tracks")

    results = {}
    order = []
    for t in wanted:
        matches = []
        for d in _catalog_docs(t):
            if _matches_kind(t, d, term_lower):
                matches.append(_resource_of(t, d, sf, with_tracks))
        page, next_off = _page_via_paginate(matches, limit, offset)
        group = {
            "href": ("/v1/catalog/" + sf + "/search?term=" + term + "&types=" + t +
                     "&limit=" + str(limit) + "&offset=" + str(offset)),
            "data": page,
        }
        if next_off != None:
            group["next"] = ("/v1/catalog/" + sf + "/search?term=" + term + "&types=" + t +
                             "&limit=" + str(limit) + "&offset=" + str(next_off))
        results[t] = group
        order.append(t)

    return respond(200, {
        "results": results,
        "meta": {"results": {"order": order}},
    })

# _matches_kind reports whether a catalog doc of the given kind matches the
# lowercased term (name, artist, or album text).
def _matches_kind(kind, doc, term_lower):
    attrs = doc.get("attributes", {})
    name = attrs.get("name", "").lower()
    if kind == "songs":
        return (_contains(name, term_lower) or
                _contains(attrs.get("artistName", "").lower(), term_lower) or
                _contains(attrs.get("albumName", "").lower(), term_lower))
    if kind == "albums" or kind == "artists":
        return _contains(name, term_lower) or _contains(attrs.get("artistName", "").lower(), term_lower)
    return _contains(name, term_lower) or _contains(attrs.get("curatorName", "").lower(), term_lower)
