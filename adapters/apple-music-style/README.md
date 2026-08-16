# apple-music-style

A stunt adapter simulating the **Apple Music API** with the developer JWT
(ES256) auth model, for local testing.

## Simulated API

- **Name:** Apple Music API
- **Version:** `1.0`

## Why this adapter?

Apple Music requires a developer token (ES256 JWT signed with a private key,
iss=teamId, kid=keyId) plus an optional user music token for library access.
Generating the JWT and getting the signing workflow right is half the battle
of integrating with Apple Music. This adapter lets you test your client code
locally without provisioning an Apple Developer account or managing keys.

## Auth

- **Developer JWT:** `Authorization: Bearer <jwt>` — structural validation:
  JWT must have 3 dot-separated segments; the JOSE header must contain
  `ES256`. The exact token must also be registered in the token store: the
  deterministic test credential below is seeded automatically on first use;
  any other forged ES256-header JWT is rejected with 401.

  ```
  header:  {"alg":"ES256","kid":"TESTKEY123","typ":"JWT"}
  payload: {"iss":"TEAMID123","iat":1700000000,"exp":1900000000}
  signature: any (not verified) — e.g. "c3ludGhldGljLXNpZ25hdHVyZQ"
  ```

- **User Music Token:** `Music-User-Token: <token>` header, required (any
  non-empty value) for every `/v1/me/*` endpoint.

## Endpoints

### Catalog (developer JWT)

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/v1/catalog/{storefront}/songs` | List songs (`limit` default 20/max 100, `offset`, `next` links). |
| GET | `/v1/catalog/{storefront}/songs/{id}` | Get a song by id (404 when unknown). |
| GET | `/v1/catalog/{storefront}/albums` | List albums. |
| GET | `/v1/catalog/{storefront}/albums/{id}` | Get an album by id. |
| GET | `/v1/catalog/{storefront}/artists` | List artists. |
| GET | `/v1/catalog/{storefront}/artists/{id}` | Get an artist by id. |
| GET | `/v1/catalog/{storefront}/playlists` | List editorial playlists (`include=tracks` embeds tracks). |
| GET | `/v1/catalog/{storefront}/charts?types=songs,albums` | Charts over the seed: `results.<type>=[{chart,name,href,order,next?}]`. |
| GET | `/v1/catalog/{storefront}/search?term=&types=songs,albums&limit=&offset=` | Grouped search: `results.<type>={href,next?,data}` (limit default 5/max 25 per group). |

### Personal library (developer JWT **and** Music-User-Token)

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/v1/me/library/songs` | Library songs (`limit` default 100, `offset`, `fields[library-songs]`). |
| GET | `/v1/me/library/albums` | Saved albums. |
| GET | `/v1/me/library/playlists` | Personal playlists (tracks relationship). |
| GET | `/v1/me/library/recently-added` | Newest additions across resource types. |
| POST | `/v1/me/library?ids[songs]=a,b&ids[albums]=…` | Add catalog resources to the library (also accepts body `{"ids":{"songs":[…]}}`). 204. |
| DELETE | `/v1/me/library/{type}/{id}` | Remove a library resource (`songs` \| `albums` \| `playlists`; id may be the library id or the catalog id). 204. |
| POST | `/v1/me/played` | Mark a song played (`{"id":…,"type":"songs"}`); bumps `playCount` and stamps `lastPlayedDate`. 204. |
| GET | `/v1/me/ratings/{type}/{id}` | Rating for a resource (`value` 0 when unrated). |
| PUT | `/v1/me/ratings/{type}/{id}` | Love (1) / clear (0) / dislike (-1). Body `{"type":"ratings","id":…,"attributes":{"value":1}}` → 201. |

## Key shapes

- Catalog resource: `{id, type:"songs", href:"/v1/catalog/{storefront}/songs/{id}", attributes:{name, artistName, albumName, artwork:{url,width,height}, durationInMillis, genreNames, trackNumber, releaseDate, isrc}}`.
- Search: `{results:{songs:{href, next?, data:[…]}}, meta:{results:{order:["songs"]}}}`.
- Charts: `{results:{songs:[{chart:[…], name:"Top Songs", href, order:{groupOrdinal, position}}]}}`.
- Library song: `{id:"i.…", type:"library-songs", href:"/v1/me/library/songs/{id}", attributes:{…catalog attrs, playCount, dateAdded, lastPlayedDate?}}`.
- Library playlist: `{id:"p.…", type:"library-playlists", attributes:{name, canEdit, description, dateAdded}, relationships:{tracks:{href, data:[{id, type:"library-songs"}]}}}`.
- Rating: `{data:[{id, type:"ratings", href, attributes:{value}}]}`.
- Errors: `{errors:[{status:"400", code:"invalid_parameter", title, detail?}]}`.

## Data model

All data is **synthetic**. The catalog (3 songs, 2 albums, 2 artists, 2
editorial playlists) and a personal library (2 saved songs, 1 saved album,
2 playlists) are seeded on first access and are stateful: adding/removing
library resources, play counts, and ratings persist for the lifetime of the
`stunt up` session. Catalog ids are assembled at runtime from short digit
groups so the fixture source stays lint-clean.
