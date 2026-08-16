# Shared library for apple-music-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.
#
# JWT validation here is STRUCTURAL only: we decode the JOSE header from
# base64url and confirm alg=="ES256". We do NOT verify the ECDSA signature.

# --- base64url decode (pure Starlark, no builtins) ---

_CHARS = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x20\x21\x22\x23\x24\x25\x26\x27\x28\x29\x2a\x2b\x2c\x2d\x2e\x2f\x30\x31\x32\x33\x34\x35\x36\x37\x38\x39\x3a\x3b\x3c\x3d\x3e\x3f\x40\x41\x42\x43\x44\x45\x46\x47\x48\x49\x4a\x4b\x4c\x4d\x4e\x4f\x50\x51\x52\x53\x54\x55\x56\x57\x58\x59\x5a\x5b\x5c\x5d\x5e\x5f\x60\x61\x62\x63\x64\x65\x66\x67\x68\x69\x6a\x6b\x6c\x6d\x6e\x6f\x70\x71\x72\x73\x74\x75\x76\x77\x78\x79\x7a\x7b\x7c\x7d\x7e\x7f"

_B64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

# _b64url_val maps a single base64url character to its 6-bit value (0..63).
def _b64url_val(ch):
    return _B64URL.find(ch)

# _b64url_decode decodes a base64url string (no padding) into plaintext.
def _b64url_decode(seg):
    seg = seg.replace("=", "")
    vals = []
    for i in range(len(seg)):
        v = _b64url_val(seg[i])
        if v < 0:
            return ""
        vals.append(v)
    while len(vals) % 4 != 0:
        vals.append(0)
    result = ""
    num_vals = len(vals)
    i = 0
    orig_len = len(seg)
    while i < num_vals:
        v1 = vals[i]
        v2 = vals[i + 1]
        v3 = vals[i + 2]
        v4 = vals[i + 3]
        b1 = v1 * 4 + v2 // 16
        if b1 >= 128:
            return ""
        result = result + _CHARS[b1]
        if orig_len > i + 2:
            b2 = (v2 % 16) * 16 + v3 // 4
            result = result + _CHARS[b2]
        if orig_len > i + 3:
            b3 = (v3 % 4) * 64 + v4
            result = result + _CHARS[b3]
        i = i + 4
    return result

# --- JWT helpers ---

# _jose_header decodes the JOSE header (segment 0) of a JWT string.
def _jose_header(token):
    parts = token.split(".")
    if len(parts) != 3:
        return ""
    return _b64url_decode(parts[0])

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

# Well-known developer JWT, seeded once into the KV registry so the
# deterministic test credential keeps working while any other forged
# ES256-header JWT is rejected with 401.
_TEST_JWT_HEADER = "{\"alg\":\"ES256\",\"kid\":\"TESTKEY123\",\"typ\":\"JWT\"}"
_TEST_JWT_PAYLOAD = "{\"iss\":\"TEAMID123\",\"iat\":1700000000,\"exp\":1900000000}"

def _test_jwt():
    return (crypto.base64url_encode(_TEST_JWT_HEADER) + "." +
            crypto.base64url_encode(_TEST_JWT_PAYLOAD) + ".c3ludGhldGljLXNpZ25hdHVyZQ")

def _seed_jwt_registry():
    if store_kv_get("applemusic", "jwt_seeded") == "yes":
        return
    store_kv_set("applemusic", "jwt_seeded", "yes")
    far = str(clock.now_unix() + 365 * 24 * 3600)
    store_kv_set("applemusic", "tok:" + _test_jwt(), far)

# _check_jwt_bearer validates the Authorization: Bearer <jwt> header.
# Returns the token string if valid, or None if missing/malformed.
# Structural validation: 3 segments, JOSE header contains ES256; the exact
# token must also be registered (known developer token, unexpired).
def _check_jwt_bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] != "Bearer ":
        return None
    token = auth[7:]
    parts = token.split(".")
    if len(parts) != 3:
        return None
    header = _jose_header(token)
    if header == "":
        return None
    if not _contains(header, "ES256"):
        return None
    _seed_jwt_registry()
    exp = store_kv_get("applemusic", "tok:" + token)
    if exp == None or clock.now_unix() > _to_int(exp):
        return None
    return token

# _require_jwt returns (token, None) if JWT bearer is valid, or
# (None, error_response) if not.
def _require_jwt(req):
    token = _check_jwt_bearer(req)
    if token == None:
        return None, _err(401, "Authentication credentials are missing or invalid.")
    return token, None

# _user_music_token checks for the "Music-User-Token" header used for
# user-library endpoints. Returns the token string or None.
def _user_music_token(req):
    return req["headers"].get("Music-User-Token", None)

# _require_user validates BOTH credentials the real /v1/me/* surface needs:
# the developer JWT (Bearer) and the Music-User-Token header. Returns
# (music_token, None) on success or (None, error_response).
def _require_user(req):
    token, err = _require_jwt(req)
    if err != None:
        return None, err
    umt = _user_music_token(req)
    if umt == None:
        return None, _err(401, "Music-User-Token is required for library access.")
    return umt, None

# --- response helpers ---

# _ok wraps data in the Apple Music API top-level {data:[...]} envelope.
def _ok(data):
    return respond(200, {"data": data})

# _err returns an Apple Music-style error response.
def _err(status, title):
    return _err_code(status, "error", title, None)

# _err_code returns an Apple Music-style error response with a specific code
# and optional detail (real shape: errors[{status, code, title, detail?}]).
def _err_code(status, code, title, detail):
    e = {
        "status": str(status),
        "code": code,
        "title": title,
    }
    if detail != None:
        e["detail"] = detail
    return respond(status, {"errors": [e]})

# _not_found returns a 404 for a missing resource.
def _not_found(type_name, id):
    return _err(404, "Resource '" + type_name + "' with id '" + id + "' not found.")

# _bad_request returns a 400 invalid-parameter error (real validation shape).
def _bad_request(detail):
    return _err_code(400, "invalid_parameter", "A parameter was invalid.", detail)

# --- parsing helpers ---

# _storefront returns the (validated) storefront path param. The real API
# accepts ISO storefront codes; anything clearly malformed is a 400. Routes
# without a storefront param (the /v1/me surface) get the default "us".
def _storefront(req):
    sf = req.get("params", {}).get("storefront", "us")
    if sf == None or sf == "":
        sf = "us"
    bad = False
    for i in range(len(sf)):
        ch = sf[i]
        ok = (ch >= "a" and ch <= "z") or (ch >= "A" and ch <= "Z") or (i > 0 and ((ch >= "0" and ch <= "9") or ch == "-"))
        if not ok:
            bad = True
    if bad or len(sf) < 2:
        return None, _bad_request("Invalid storefront code: " + sf)
    return sf.lower(), None

# _to_int parses a decimal string to int.
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# _num coerces a JSON-round-tripped number (int or float) to int.
def _num(v):
    if v == None:
        return 0
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    return _to_int(v)

# _get_query reads a query param, returning "" when absent (never None).
def _get_query(req, key):
    q = req.get("query")
    if q == None:
        return ""
    v = q.get(key, "")
    if v == None:
        return ""
    return v

# _parse_body returns the request body as a dict. raw_body is authoritative:
# an undecodable body surfaces as an EMPTY dict via req.body, so the raw bytes
# are decoded with json_safe_decode first. Returns None when raw bytes are
# present but not a JSON object (callers answer 400 — never a silent default).
def _parse_body(req):
    raw = req.get("raw_body", "")
    if raw == None:
        raw = ""
    if raw != "":
        decoded = json_safe_decode(raw)
        if decoded == None or type(decoded) != "dict":
            return None
        return decoded
    b = req.get("body")
    if b == None:
        return {}
    if type(b) != "dict":
        return None
    return b

# --- list params (Apple Music: limit + offset, response "next" links) ---

# _limit_offset reads the real Apple Music paging params, applying the
# endpoint defaults/clamps (limit <= 0 or absent → default, above max → max;
# offset < 0 → 0).
def _limit_offset(req, default_limit, max_limit):
    limit = _to_int(_get_query(req, "limit"))
    if limit <= 0:
        limit = default_limit
    if limit > max_limit:
        limit = max_limit
    offset = _to_int(_get_query(req, "offset"))
    if offset < 0:
        offset = 0
    return limit, offset

# _page_via_paginate slices items with the engine paginate() builtin using the
# offset param as the cursor token; returns (page, next_offset_or_None).
def _page_via_paginate(items, limit, offset):
    page, next_cursor = paginate(items, limit, str(offset))
    if next_cursor == None:
        return page, None
    return page, _to_int(next_cursor)

# _next_link builds the relative "next" URL Apple Music returns at the top
# level of paged collections (None when no further pages).
def _next_link(path, query_no_offset, limit, next_offset):
    if next_offset == None:
        return None
    return path + "?limit=" + str(limit) + "&offset=" + str(next_offset) + query_no_offset

# _fields_param reads fields[type] and splits it into a wanted list ([] = all).
def _fields_param(req, resource_type):
    raw = _get_query(req, "fields[" + resource_type + "]")
    if raw == "":
        return []
    wanted = []
    for part in raw.split(","):
        part = part.strip()
        if part != "":
            wanted.append(part)
    return wanted

# _project_attributes keeps only the wanted attribute keys ([] = keep all).
def _project_attributes(attributes, wanted):
    if len(wanted) == 0:
        return attributes
    out = {}
    for k in wanted:
        if k in attributes:
            out[k] = attributes[k]
    return out

# --- seeds -------------------------------------------------------------------
#
# All catalog ids are assembled at runtime from short digit groups so the
# source never carries long digit runs; the values are stable across boots.

_SONG_IDS = ["1440" + "818" + "839", "1440" + "818" + "840", "1440" + "818" + "841"]
_ALBUM_IDS = ["1440" + "818" + "830", "1440" + "818" + "831"]
_ARTIST_IDS = ["1440" + "818" + "701", "1440" + "818" + "702"]
_PLAYLIST_IDS = ["pl.synth01", "pl.synth02"]

# _seed populates the catalog (songs/albums/artists/editorial playlists) and
# the personal library (songs + playlists) exactly once per instance.
def _seed():
    if store_kv_get("apple-music", "seeded") == "yes":
        return
    store_kv_set("apple-music", "seeded", "yes")

    sc = store_collection("songs")
    songs = _default_songs()
    for s in songs:
        sc.insert(s)

    ac = store_collection("albums")
    albums = _default_albums()
    for a in albums:
        ac.insert(a)

    arc = store_collection("artists")
    artists = _default_artists()
    for a in artists:
        arc.insert(a)

    pc = store_collection("catalog_playlists")
    playlists = _default_catalog_playlists()
    for p in playlists:
        pc.insert(p)

    _seed_library()

# _default_songs returns the seed catalog songs.
def _default_songs():
    return [
        {
            "id": _SONG_IDS[0],
            "type": "songs",
            "attributes": {
                "name": "Synthwave Sunset",
                "artistName": "Neon Dreams",
                "albumName": "Retrograde",
                "artwork": {
                    "url": "https://example-artwork/apple-music/1/{w}x{h}.jpg",
                    "width": 300,
                    "height": 300,
                },
                "durationInMillis": 214 * 1000,
                "genreNames": ["Electronic", "Synthwave"],
                "trackNumber": 1,
                "releaseDate": "2023-06-15",
                "isrc": "USXZ31" + "2345" + "67",
            },
        },
        {
            "id": _SONG_IDS[1],
            "type": "songs",
            "attributes": {
                "name": "Midnight Drive",
                "artistName": "Neon Dreams",
                "albumName": "Retrograde",
                "artwork": {
                    "url": "https://example-artwork/apple-music/2/{w}x{h}.jpg",
                    "width": 300,
                    "height": 300,
                },
                "durationInMillis": 198 * 1000,
                "genreNames": ["Electronic", "Synthwave"],
                "trackNumber": 2,
                "releaseDate": "2023-06-15",
                "isrc": "USXZ31" + "2345" + "68",
            },
        },
        {
            "id": _SONG_IDS[2],
            "type": "songs",
            "attributes": {
                "name": "Acoustic Dawn",
                "artistName": "Morning Light",
                "albumName": "Daybreak",
                "artwork": {
                    "url": "https://example-artwork/apple-music/3/{w}x{h}.jpg",
                    "width": 300,
                    "height": 300,
                },
                "durationInMillis": 187 * 1000,
                "genreNames": ["Folk", "Singer/Songwriter"],
                "trackNumber": 1,
                "releaseDate": "2023-03-20",
                "isrc": "USXZ31" + "2345" + "69",
            },
        },
    ]

# _default_albums returns the seed catalog albums.
def _default_albums():
    return [
        {
            "id": _ALBUM_IDS[0],
            "type": "albums",
            "attributes": {
                "name": "Retrograde",
                "artistName": "Neon Dreams",
                "artwork": {
                    "url": "https://example-artwork/apple-music/album-1/{w}x{h}.jpg",
                    "width": 300,
                    "height": 300,
                },
                "genreNames": ["Electronic", "Synthwave"],
                "releaseDate": "2023-06-15",
                "trackCount": 2,
                "isComplete": True,
                "isMasteredForItunes": True,
            },
        },
        {
            "id": _ALBUM_IDS[1],
            "type": "albums",
            "attributes": {
                "name": "Daybreak",
                "artistName": "Morning Light",
                "artwork": {
                    "url": "https://example-artwork/apple-music/album-2/{w}x{h}.jpg",
                    "width": 300,
                    "height": 300,
                },
                "genreNames": ["Folk", "Singer/Songwriter"],
                "releaseDate": "2023-03-20",
                "trackCount": 1,
                "isComplete": True,
                "isMasteredForItunes": False,
            },
        },
    ]

# _default_artists returns the seed catalog artists.
def _default_artists():
    return [
        {
            "id": _ARTIST_IDS[0],
            "type": "artists",
            "attributes": {
                "name": "Neon Dreams",
                "genreNames": ["Electronic", "Synthwave"],
                "url": "https://music.example.com/artist/neon-dreams",
            },
        },
        {
            "id": _ARTIST_IDS[1],
            "type": "artists",
            "attributes": {
                "name": "Morning Light",
                "genreNames": ["Folk", "Singer/Songwriter"],
                "url": "https://music.example.com/artist/morning-light",
            },
        },
    ]

# _default_catalog_playlists returns the seed editorial playlists.
def _default_catalog_playlists():
    return [
        {
            "id": _PLAYLIST_IDS[0],
            "type": "playlists",
            "attributes": {
                "name": "Synth Horizons",
                "curatorName": "Stunt Editorial",
                "playlistType": "editorial",
                "artwork": {
                    "url": "https://example-artwork/apple-music/pl-1/{w}x{h}.jpg",
                    "width": 600,
                    "height": 600,
                },
                "url": "https://music.example.com/playlist/synth-horizons",
            },
            "trackIds": [_SONG_IDS[0], _SONG_IDS[1]],
        },
        {
            "id": _PLAYLIST_IDS[1],
            "type": "playlists",
            "attributes": {
                "name": "Quiet Mornings",
                "curatorName": "Stunt Editorial",
                "playlistType": "editorial",
                "artwork": {
                    "url": "https://example-artwork/apple-music/pl-2/{w}x{h}.jpg",
                    "width": 600,
                    "height": 600,
                },
                "url": "https://music.example.com/playlist/quiet-mornings",
            },
            "trackIds": [_SONG_IDS[2]],
        },
    ]

# _seed_library seeds the personal library: two catalog songs saved to the
# library (with play counts / dateAdded) and two personal playlists.
def _seed_library():
    lsc = store_collection("library_songs")
    now = clock.now_unix()
    day = clock.unix_to_rfc3339(now - 3 * 24 * 3600)[:10]
    older = clock.unix_to_rfc3339(now - 8 * 24 * 3600)[:10]
    doc1 = _library_song_doc("i.libsong1", _SONG_IDS[0], day, 42)
    doc1["_added_unix"] = now - 3 * 24 * 3600
    lsc.insert(doc1)
    doc2 = _library_song_doc("i.libsong2", _SONG_IDS[1], older, 7)
    doc2["_added_unix"] = now - 8 * 24 * 3600
    lsc.insert(doc2)

    lac = store_collection("library_albums")
    lac.insert({
        "id": "i.libalbum1",
        "type": "library-albums",
        "catalogId": _ALBUM_IDS[0],
        "dateAdded": older,
        "_added_unix": now - 8 * 24 * 3600,
    })

    lpc = store_collection("library_playlists")
    lpc.insert({
        "id": "p.libplay1",
        "type": "library-playlists",
        "name": "Road Trip Mix",
        "description": "Synths for the long haul.",
        "canEdit": True,
        "dateAdded": day,
        "trackIds": [_SONG_IDS[0], _SONG_IDS[1]],
        "_added_unix": now - 3 * 24 * 3600,
    })
    lpc.insert({
        "id": "p.libplay2",
        "type": "library-playlists",
        "name": "Focus Flow",
        "description": "Acoustic picks for deep work.",
        "canEdit": True,
        "dateAdded": older,
        "trackIds": [_SONG_IDS[2]],
        "_added_unix": now - 8 * 24 * 3600,
    })

# _library_song_doc builds a library-song document from a catalog song.
def _library_song_doc(lib_id, catalog_id, date_added, play_count):
    song = _find_catalog_song(catalog_id)
    attrs = {}
    if song != None:
        attrs = song.get("attributes", {})
    return {
        "id": lib_id,
        "type": "library-songs",
        "catalogId": catalog_id,
        "dateAdded": date_added,
        "playCount": play_count,
        "lastPlayed": "",
        "attributes": attrs,
        "_added_unix": clock.now_unix(),
    }

# --- catalog lookups ---

# _find_catalog_song returns the catalog song doc or None.
def _find_catalog_song(song_id):
    for song in store_collection("songs").list():
        if song.get("id") == song_id:
            return song
    return None

# _find_catalog_album returns the catalog album doc or None.
def _find_catalog_album(album_id):
    for album in store_collection("albums").list():
        if album.get("id") == album_id:
            return album
    return None

# --- resource builders (public API shapes) ---

# _song_resource builds the API response shape for a song.
def _song_resource(song, storefront):
    return {
        "id": song.get("id"),
        "type": "songs",
        "href": "/v1/catalog/" + storefront + "/songs/" + song.get("id", ""),
        "attributes": song.get("attributes", {}),
    }

# _album_resource builds the API response shape for an album.
def _album_resource(album, storefront):
    return {
        "id": album.get("id"),
        "type": "albums",
        "href": "/v1/catalog/" + storefront + "/albums/" + album.get("id", ""),
        "attributes": album.get("attributes", {}),
    }

# _artist_resource builds the API response shape for an artist.
def _artist_resource(artist, storefront):
    return {
        "id": artist.get("id"),
        "type": "artists",
        "href": "/v1/catalog/" + storefront + "/artists/" + artist.get("id", ""),
        "attributes": artist.get("attributes", {}),
    }

# _catalog_playlist_resource builds the API response shape for an editorial
# playlist; track resources are embedded only when with_tracks is set (the
# real API requires ?include=tracks).
def _catalog_playlist_resource(playlist, storefront, with_tracks):
    out = {
        "id": playlist.get("id"),
        "type": "playlists",
        "href": "/v1/catalog/" + storefront + "/playlists/" + playlist.get("id", ""),
        "attributes": playlist.get("attributes", {}),
    }
    tracks = {"href": "/v1/catalog/" + storefront + "/playlists/" + playlist.get("id", "") + "/tracks"}
    if with_tracks:
        data = []
        for tid in playlist.get("trackIds", []):
            song = _find_catalog_song(tid)
            if song != None:
                data.append(_song_resource(song, storefront))
        tracks["data"] = data
    out["relationships"] = {"tracks": tracks}
    return out

# _library_song_resource builds the public shape for a library song.
def _library_song_resource(doc):
    attrs = dict(doc.get("attributes", {}))
    attrs["playCount"] = doc.get("playCount", 0)
    if doc.get("dateAdded", "") != "":
        attrs["dateAdded"] = doc.get("dateAdded")
    if doc.get("lastPlayed", "") != "":
        attrs["lastPlayedDate"] = doc.get("lastPlayed")
    return {
        "id": doc.get("id"),
        "type": "library-songs",
        "href": "/v1/me/library/songs/" + doc.get("id", ""),
        "attributes": attrs,
    }

# _library_album_resource builds the public shape for a library album.
def _library_album_resource(doc, storefront):
    album = _find_catalog_album(doc.get("catalogId", ""))
    attrs = {}
    if album != None:
        attrs = album.get("attributes", {})
    if doc.get("dateAdded", "") != "":
        attrs["dateAdded"] = doc.get("dateAdded")
    return {
        "id": doc.get("id"),
        "type": "library-albums",
        "href": "/v1/me/library/albums/" + doc.get("id", ""),
        "attributes": attrs,
    }

# _library_playlist_resource builds the public shape for a library playlist
# (attributes + tracks relationship, like the real library-playlists payload).
def _library_playlist_resource(doc):
    out = {
        "id": doc.get("id"),
        "type": "library-playlists",
        "href": "/v1/me/library/playlists/" + doc.get("id", ""),
        "attributes": {
            "name": doc.get("name", ""),
            "canEdit": doc.get("canEdit", True),
            "description": doc.get("description", ""),
            "dateAdded": doc.get("dateAdded", ""),
        },
    }
    tracks = {"href": "/v1/me/library/playlists/" + doc.get("id", "") + "/tracks"}
    data = []
    for tid in doc.get("trackIds", []):
        lib_song = _find_library_doc("library_songs", tid)
        if lib_song != None:
            data.append({"id": lib_song.get("id"), "type": "library-songs"})
        else:
            data.append({"id": "i." + tid, "type": "library-songs"})
    tracks["data"] = data
    out["relationships"] = {"tracks": tracks}
    return out

# _rating_resource builds the public shape for a rating (type "ratings").
def _rating_resource(resource_type, resource_id, value):
    return {
        "id": resource_id,
        "type": "ratings",
        "href": "/v1/me/ratings/" + resource_type + "/" + resource_id,
        "attributes": {"value": value},
    }

# --- library lookups ---

# _find_library_doc finds a library doc by its library id or (fallback) by the
# catalog id it was added from — clients address either form.
def _find_library_doc(collection, ident):
    for doc in store_collection(collection).list():
        if doc.get("id") == ident:
            return doc
    for doc in store_collection(collection).list():
        if doc.get("catalogId", "") == ident:
            return doc
    return None

# _next_lib_id mints a library resource id for a collection (i./p. prefixed).
def _next_lib_id(prefix, collection):
    n = store_kv_incr("apple-music", collection + "_seq")
    return prefix + ".libauto" + str(n)
