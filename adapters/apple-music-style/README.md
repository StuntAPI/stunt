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
  `ES256`.
- **User Music Token:** `Music-User-Token: <token>` header, required for
  `/v1/me/library/*` endpoints.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/v1/catalog/{storefront}/songs/{id}` | Get a song by id. |
| GET | `/v1/catalog/{storefront}/albums/{id}` | Get an album by id. |
| GET | `/v1/catalog/{storefront}/search?term=&types=songs` | Search catalog. |
| GET | `/v1/me/library/songs` | Get user library songs (Music-User-Token). |

## Key shapes

- Song: `{data:[{id, type:"songs", attributes:{name, artistName, albumName, artwork:{url,width,height}, durationInMillis, genreNames, trackNumber, releaseDate, isrc}}]}`.
- Album: `{data:[{id, type:"albums", attributes:{name, artistName, artwork:{...}, genreNames, releaseDate, trackCount}}]}`.
- Search: `{data:[...], meta:{results:{order:["songs","albums"]}}}`.
- Library songs: `{data:[{id, type:"library-songs", attributes:{...}}], meta:{total}}`.

## Data model

All data is **synthetic**. Catalog songs and albums are seeded on first access
and are stateful. Library songs start empty.
