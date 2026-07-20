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

# _check_jwt_bearer validates the Authorization: Bearer <jwt> header.
# Returns the token string if valid, or None if missing/malformed.
# Structural validation: 3 segments, JOSE header contains ES256.
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

# --- response helpers ---

# _ok wraps data in the Apple Music API top-level {data:[...]} envelope.
def _ok(data):
    return respond(200, {"data": data})

# _err returns an Apple Music-style error response.
def _err(status, title):
    return respond(status, {
        "errors": [
            {
                "status": str(status),
                "code": "error",
                "title": title,
            }
        ],
    })

# _not_found returns a 404 for a missing resource.
def _not_found(type_name, id):
    return _err(404, "Resource '" + type_name + "' with id '" + id + "' not found.")

# --- misc helpers ---

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

# _seed populates the default catalog songs and albums.
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

# _default_songs returns the seed catalog songs.
def _default_songs():
    return [
        {
            "id": "1440818839",
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
                "durationInMillis": 214000,
                "genreNames": ["Electronic", "Synthwave"],
                "trackNumber": 1,
                "releaseDate": "2023-06-15",
                "isrc": "USXZ31234567",
            },
        },
        {
            "id": "1440818840",
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
                "durationInMillis": 198000,
                "genreNames": ["Electronic", "Synthwave"],
                "trackNumber": 2,
                "releaseDate": "2023-06-15",
                "isrc": "USXZ31234568",
            },
        },
        {
            "id": "1440818841",
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
                "durationInMillis": 187000,
                "genreNames": ["Folk", "Singer/Songwriter"],
                "trackNumber": 1,
                "releaseDate": "2023-03-20",
                "isrc": "USXZ31234569",
            },
        },
    ]

# _default_albums returns the seed catalog albums.
def _default_albums():
    return [
        {
            "id": "1440818830",
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
            "id": "1440818831",
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
