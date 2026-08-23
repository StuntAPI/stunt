package adapters

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/events"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// These tests drive the apple-music-style adapter scripts directly
// (lib.star preloaded) over a shared store and a VIRTUAL clock: the
// developer-token gate (registered ES256 JWT, expiry via the token
// registry), catalog search/charts, include=tracks relationships,
// storefront normalization, offset paging, the Music-User-Token library
// surface (add/remove/played/ratings) and Apple's error envelope.

// Seeded catalog ids (assembled at runtime inside the adapter from digit
// groups — see lib.star _SONG_IDS and friends).
const (
	amSong1     = "1440818839" // Synthwave Sunset, Neon Dreams
	amSong2     = "1440818840" // Midnight Drive, Neon Dreams
	amSong3     = "1440818841" // Acoustic Dawn, Morning Light
	amAlbum1    = "1440818830" // Retrograde
	amAlbum2    = "1440818831" // Daybreak
	amArtist1   = "1440818701" // Neon Dreams
	amPlaylist1 = "pl.synth01" // Synth Horizons (tracks: amSong1, amSong2)
	amPlaylist2 = "pl.synth02" // Quiet Mornings (tracks: amSong3)

	amUMT = "test-user-token-123"
)

// amDevJWT is the deterministic developer token the adapter registers on
// first use (README Auth section) — the only bearer it accepts.
func amDevJWT() string {
	header := `{"alg":"ES256","kid":"TESTKEY123","typ":"JWT"}`
	payload := `{"iss":"TEAMID123","iat":1700000000,"exp":1900000000}`
	return base64.RawURLEncoding.EncodeToString([]byte(header)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(payload)) +
		".c3ludGhldGljLXNpZ25hdHVyZQ"
}

// amFixture is one shared store + virtual clock with a loaded VM per
// handler script (catalog.star and library.star each get their own VM, but
// they observe the same collections/kv state, like the engine).
type amFixture struct {
	t         *testing.T
	vc        *clock.Clock
	vmCatalog *starlark.VM
	vmLibrary *starlark.VM
}

func newAppleMusicFixture(t *testing.T, start time.Time) *amFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "apple-music-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}
	tmp := t.TempDir()
	store, _ := primitives.Open(filepath.Join(tmp, "s.db"))
	t.Cleanup(func() { store.Close() })
	kvStore, _ := kv.Open(filepath.Join(tmp, "s.kv.db"))
	t.Cleanup(func() { kvStore.Close() })
	blobStore, _ := blob.Open(filepath.Join(tmp, "blobs"))
	t.Cleanup(func() { blobStore.Close() })

	vc := clock.NewVirtualClock(start)
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: vc, ServiceName: "test", Emitter: events.NewEmitter(),
	})
	load := func(script string) *starlark.VM {
		t.Helper()
		src, err := os.ReadFile(filepath.Join(root, "scripts", script))
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
		if err != nil {
			t.Fatalf("LoadWithLib %s: %v", script, err)
		}
		return vm
	}
	return &amFixture{
		t: t, vc: vc,
		vmCatalog: load("catalog.star"),
		vmLibrary: load("library.star"),
	}
}

// call invokes handler on vm with explicit headers ("" = header absent).
func (f *amFixture) call(vm *starlark.VM, handler, method, path string, params, query map[string]string, body map[string]any, authorization, umt string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if authorization != "" {
		headers["Authorization"] = authorization
	}
	if umt != "" {
		headers["Music-User-Token"] = umt
	}
	resp, err := vm.Call(handler, starlark.Request{
		Method: method, Path: path, Host: "api.music.example.test",
		Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// catalog serves a catalog route with the valid developer token.
func (f *amFixture) catalog(handler, path string, params, query map[string]string) starlark.Response {
	f.t.Helper()
	return f.call(f.vmCatalog, handler, "GET", path, params, query, nil, "Bearer "+amDevJWT(), "")
}

// me serves a /v1/me route with both credentials the surface demands.
func (f *amFixture) me(handler, method, path string, params, query map[string]string, body map[string]any) starlark.Response {
	f.t.Helper()
	return f.call(f.vmLibrary, handler, method, path, params, query, body, "Bearer "+amDevJWT(), amUMT)
}

// callRaw is call with a raw (string) request body, exercising the
// handlers' raw-body-first decoding the way the engine delivers JSON.
func (f *amFixture) callRaw(vm *starlark.VM, handler, method, path string, params, query map[string]string, rawBody, authorization, umt string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if authorization != "" {
		headers["Authorization"] = authorization
	}
	if umt != "" {
		headers["Music-User-Token"] = umt
	}
	resp, err := vm.Call(handler, starlark.Request{
		Method: method, Path: path, Host: "api.music.example.test",
		Headers: headers, Params: params, Query: query, RawBody: rawBody,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// sfParams builds the catalog route params {storefront, id?}.
func sfParams(storefront, id string) map[string]string {
	params := map[string]string{"storefront": storefront}
	if id != "" {
		params["id"] = id
	}
	return params
}

// amNum compares a response number against want whether it arrives as an
// int (handler-built) or a float (round-tripped through a collection's
// JSON storage) — identical once serialized.
func amNum(got any, want int64) bool {
	switch n := got.(type) {
	case int64:
		return n == want
	case float64:
		return n == float64(want)
	}
	return false
}

// errOf returns the single Apple error object from an error response.
func errOf(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	errs, ok := r.Body["errors"].([]any)
	if !ok || len(errs) != 1 {
		t.Fatalf("response %d errors = %v, want exactly one", r.Status, r.Body)
	}
	e, _ := errs[0].(map[string]any)
	return e
}

// TestAppleMusicDeveloperTokenGate: only the registered deterministic ES256
// developer token passes the catalog gate — missing headers, wrong scheme,
// malformed segments, non-ES256 JOSE headers and forged ES256 tokens are
// all 401s in Apple's error envelope, and the token dies when its
// registry entry expires (virtual clock, 365 days).
func TestAppleMusicDeveloperTokenGate(t *testing.T) {
	f := newAppleMusicFixture(t, time.Unix(1_750_000_000, 0).UTC())
	get := func(auth string) starlark.Response {
		return f.call(f.vmCatalog, "on_get_song", "GET", "/v1/catalog/us/songs/"+amSong1,
			sfParams("us", amSong1), nil, nil, auth, "")
	}

	// ===== a missing bearer answers 401 in Apple's error envelope =====
	// Apple sends the HTTP status inside errors[].status as a STRING.
	r := get("")
	if r.Status != 401 {
		t.Fatalf("no bearer -> %d, want 401", r.Status)
	}
	e := errOf(t, r)
	if e["status"] != "401" || e["code"] != "error" {
		t.Fatalf("401 error object = %v, want status \"401\" code error", e)
	}
	if e["title"] != "Authentication credentials are missing or invalid." {
		t.Fatalf("401 title = %v", e["title"])
	}

	// ===== malformed schemes, segments, algs and signatures are all 401 =====
	forged := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","kid":"EVIL","typ":"JWT"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"ATTACKER"}`)) + ".c2ln"
	for _, auth := range []string{
		"Token " + amDevJWT(),             // wrong scheme
		"Bearer no-dots-here",             // not 3 segments
		"Bearer a.b",                      // 2 segments
		"Bearer !!.!!.!!",                 // undecodable header
		"Bearer " + amForgedHS256(),       // ES256 required
		"Bearer " + forged,                // valid shape, never registered
		"Bearer " + amDevJWT() + "tamper", // registered token, altered bytes
		"Bearer " + strings.TrimSuffix(amDevJWT(), "Q") + "x", // wrong signature segment
	} {
		if r := get(auth); r.Status != 401 {
			t.Fatalf("auth %q -> %d, want 401 (only the registered token passes)", auth, r.Status)
		}
	}

	// ===== the registered deterministic developer token reads the catalog =====
	ok := get("Bearer " + amDevJWT())
	if ok.Status != 200 {
		t.Fatalf("registered developer token -> %d: %v", ok.Status, ok.Body)
	}
	data, _ := ok.Body["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != amSong1 {
		t.Fatalf("song fetch data = %v", ok.Body["data"])
	}

	// ===== the developer token dies when its registry entry expires =====
	// The registry entry lives 365 days from first use; the clock is virtual,
	// so no waiting.
	f.vc.Advance(366 * 24 * time.Hour)
	if r := get("Bearer " + amDevJWT()); r.Status != 401 {
		t.Fatalf("same token after registry expiry -> %d, want 401", r.Status)
	}
}

// amForgedHS256 mints a 3-segment JWT with a non-ES256 JOSE header.
func amForgedHS256() string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"TEAMID123"}`))
	return header + "." + payload + ".c2ln"
}

// TestAppleMusicCatalogSearchAndCharts: search requires a term, groups
// matches by requested type (meta.results.order) with per-group limit/
// offset paging and next links, matches names/artists/albums
// case-insensitively, rejects unknown types, and charts order the seed by
// release date per requested type.
func TestAppleMusicCatalogSearchAndCharts(t *testing.T) {
	f := newAppleMusicFixture(t, time.Unix(1_750_000_000, 0).UTC())
	search := func(query map[string]string) starlark.Response {
		return f.catalog("on_search", "/v1/catalog/us/search", sfParams("us", ""), query)
	}

	// ===== search without a term is a 400 invalid_parameter =====
	r := search(map[string]string{"types": "songs"})
	if r.Status != 400 {
		t.Fatalf("search without term -> %d, want 400", r.Status)
	}
	e := errOf(t, r)
	if e["code"] != "invalid_parameter" || e["detail"] != "term is required." {
		t.Fatalf("termless search error = %v", e)
	}

	// ===== results group by requested type in meta order with next links =====
	r = search(map[string]string{"term": "Neon", "types": "songs,albums,artists", "limit": "1"})
	if r.Status != 200 {
		t.Fatalf("grouped search -> %d: %v", r.Status, r.Body)
	}
	meta, _ := r.Body["meta"].(map[string]any)["results"].(map[string]any)
	if got := meta["order"].([]any); len(got) != 3 || got[0] != "songs" || got[1] != "albums" || got[2] != "artists" {
		t.Fatalf("meta.results.order = %v, want the requested type order", meta["order"])
	}
	results := r.Body["results"].(map[string]any)
	songs := results["songs"].(map[string]any)
	// limit=1 over 2 Neon Dreams songs: one row + an offset next link.
	if data := songs["data"].([]any); len(data) != 1 || data[0].(map[string]any)["type"] != "songs" {
		t.Fatalf("songs group data = %v", songs["data"])
	}
	if next := songs["next"].(string); next != "/v1/catalog/us/search?term=Neon&types=songs&limit=1&offset=1" {
		t.Fatalf("songs group next = %q", next)
	}
	// Albums match too; artists group carries the artist resource.
	if data := results["albums"].(map[string]any)["data"].([]any); len(data) != 1 {
		t.Fatalf("albums group data = %v", results["albums"])
	}
	if data := results["artists"].(map[string]any)["data"].([]any); len(data) != 1 ||
		data[0].(map[string]any)["id"] != amArtist1 {
		t.Fatalf("artists group data = %v", results["artists"])
	}

	// ===== the term matches names, artists and album text case-insensitively =====
	for _, tc := range []struct {
		term, types string
		want        int
	}{
		{"neon", "songs", 2},          // artistName, lowercase term
		{"retrograde", "songs", 2},    // albumName
		{"acoustic", "songs", 1},      // name
		{"synth", "playlists", 1},     // playlist name
		{"editorial", "playlists", 2}, // curatorName
	} {
		r := search(map[string]string{"term": tc.term, "types": tc.types})
		if r.Status != 200 {
			t.Fatalf("search %q -> %d", tc.term, r.Status)
		}
		group := r.Body["results"].(map[string]any)[tc.types].(map[string]any)
		if data := group["data"].([]any); len(data) != tc.want {
			t.Fatalf("search %q types=%s -> %d rows, want %d", tc.term, tc.types, len(data), tc.want)
		}
	}
	// A term with no hits returns an empty group and no next link.
	r = search(map[string]string{"term": "zzz", "types": "songs"})
	group := r.Body["results"].(map[string]any)["songs"].(map[string]any)
	if len(group["data"].([]any)) != 0 {
		t.Fatalf("zzz search data = %v", group["data"])
	}
	if _, has := group["next"]; has {
		t.Fatalf("empty group carries next = %v", group["next"])
	}

	// ===== unknown search and chart types are 400 invalid_parameter =====
	if r := search(map[string]string{"term": "x", "types": "videos"}); r.Status != 400 ||
		errOf(t, r)["detail"] != "Unsupported search type: videos" {
		t.Fatalf("types=videos -> %d %v", r.Status, r.Body)
	}
	if r := f.catalog("on_charts", "/v1/catalog/us/charts", sfParams("us", ""),
		map[string]string{"types": "videos"}); r.Status != 400 ||
		errOf(t, r)["detail"] != "Unsupported chart type: videos" {
		t.Fatalf("charts types=videos -> %d %v", r.Status, r.Body)
	}

	// ===== charts order the seed by release date within each requested type =====
	r = f.catalog("on_charts", "/v1/catalog/us/charts", sfParams("us", ""),
		map[string]string{"types": "songs,albums", "limit": "2"})
	if r.Status != 200 {
		t.Fatalf("charts -> %d: %v", r.Status, r.Body)
	}
	chartResults := r.Body["results"].(map[string]any)
	songChart := chartResults["songs"].([]any)[0].(map[string]any)
	if songChart["name"] != "Top Songs" {
		t.Fatalf("chart name = %v", songChart["name"])
	}
	// Newest release first: both 2023-06-15 songs ahead of 2023-03-20.
	chart := songChart["chart"].([]any)
	if len(chart) != 2 {
		t.Fatalf("limit=2 chart has %d entries", len(chart))
	}
	if id := chart[0].(map[string]any)["id"]; id != amSong1 && id != amSong2 {
		t.Fatalf("chart[0] = %v, want a 2023-06-15 release", id)
	}
	if _, has := songChart["next"]; !has {
		t.Fatal("partial chart page carries no next link")
	}
	albumChart := chartResults["albums"].([]any)[0].(map[string]any)
	if albumChart["name"] != "Top Albums" {
		t.Fatalf("album chart name = %v", albumChart["name"])
	}
	if got := r.Body["meta"].(map[string]any)["results"].(map[string]any)["order"].([]any); len(got) != 2 {
		t.Fatalf("charts meta order = %v", got)
	}
}

// TestAppleMusicResourceFetch: playlist relationships embed track
// resources only with include=tracks, storefront codes are validated and
// normalized into hrefs, and unknown ids answer Apple's 404 body.
func TestAppleMusicResourceFetch(t *testing.T) {
	f := newAppleMusicFixture(t, time.Unix(1_750_000_000, 0).UTC())

	// ===== include=tracks embeds track resources; without it only an href =====
	r := f.catalog("on_list_playlists", "/v1/catalog/us/playlists", sfParams("us", ""), nil)
	if r.Status != 200 {
		t.Fatalf("list playlists -> %d: %v", r.Status, r.Body)
	}
	plain := r.Body["data"].([]any)[0].(map[string]any)
	tracks := plain["relationships"].(map[string]any)["tracks"].(map[string]any)
	if _, has := tracks["data"]; has {
		t.Fatalf("tracks relationship without include embeds data: %v", tracks)
	}
	if tracks["href"] != "/v1/catalog/us/playlists/"+amPlaylist1+"/tracks" {
		t.Fatalf("tracks href = %v", tracks["href"])
	}

	r = f.catalog("on_list_playlists", "/v1/catalog/us/playlists", sfParams("us", ""),
		map[string]string{"include": "tracks"})
	embedded := r.Body["data"].([]any)[0].(map[string]any)
	trackData := embedded["relationships"].(map[string]any)["tracks"].(map[string]any)["data"].([]any)
	if len(trackData) != 2 {
		t.Fatalf("pl.synth01 embeds %d tracks, want its 2 seed songs", len(trackData))
	}
	attrs := trackData[0].(map[string]any)["attributes"].(map[string]any)
	if trackData[0].(map[string]any)["type"] != "songs" || attrs["name"] != "Synthwave Sunset" {
		t.Fatalf("embedded track = %v", trackData[0])
	}

	// ===== storefront codes are validated and normalized (US -> us) =====
	r = f.catalog("on_get_song", "/v1/catalog/US/songs/"+amSong1, sfParams("US", amSong1), nil)
	if r.Status != 200 {
		t.Fatalf("storefront US -> %d: %v", r.Status, r.Body)
	}
	if got := r.Body["data"].([]any)[0].(map[string]any)["href"]; got != "/v1/catalog/us/songs/"+amSong1 {
		t.Fatalf("href for storefront US = %v, want lowercased us", got)
	}
	// Same catalog data under any well-formed code (storefront is not a filter
	// here, unlike the real API's per-storefront availability).
	if r := f.catalog("on_get_song", "/v1/catalog/gb/songs/"+amSong1, sfParams("gb", amSong1), nil); r.Status != 200 {
		t.Fatalf("storefront gb -> %d", r.Status)
	}
	for _, sf := range []string{"1", "u", "us!", "u s"} {
		r := f.catalog("on_list_songs", "/v1/catalog/"+sf+"/songs", sfParams(sf, ""), nil)
		if r.Status != 400 || errOf(t, r)["detail"] != "Invalid storefront code: "+sf {
			t.Fatalf("storefront %q -> %d %v, want 400 invalid_parameter", sf, r.Status, r.Body)
		}
	}

	// ===== unknown ids answer Apple's 404 error body =====
	for _, tc := range []struct {
		handler, kind, id string
	}{
		{"on_get_song", "songs", "999"},
		{"on_get_album", "albums", "999"},
		{"on_get_artist", "artists", "999"},
	} {
		r := f.catalog(tc.handler, "/v1/catalog/us/"+tc.kind+"/"+tc.id, sfParams("us", tc.id), nil)
		if r.Status != 404 {
			t.Fatalf("unknown %s -> %d, want 404", tc.kind, r.Status)
		}
		e := errOf(t, r)
		if e["status"] != "404" || e["code"] != "error" {
			t.Fatalf("404 error object = %v, want status \"404\" code error", e)
		}
		if e["title"] != "Resource '"+tc.kind+"' with id '"+tc.id+"' not found." {
			t.Fatalf("404 title = %v", e["title"])
		}
	}

	// ===== a known album and artist fetch with storefront-derived hrefs =====
	r = f.catalog("on_get_album", "/v1/catalog/us/albums/"+amAlbum1, sfParams("us", amAlbum1), nil)
	album := r.Body["data"].([]any)[0].(map[string]any)
	if album["type"] != "albums" || album["href"] != "/v1/catalog/us/albums/"+amAlbum1 {
		t.Fatalf("album resource = %v", album)
	}
	r = f.catalog("on_get_artist", "/v1/catalog/us/artists/"+amArtist1, sfParams("us", amArtist1), nil)
	if r.Body["data"].([]any)[0].(map[string]any)["attributes"].(map[string]any)["name"] != "Neon Dreams" {
		t.Fatalf("artist resource = %v", r.Body["data"])
	}
}

// TestAppleMusicOffsetPagination: catalog and library lists page by
// offset — a next link only while items remain, limits clamped to each
// endpoint's default and max, and library pages carry meta.total.
func TestAppleMusicOffsetPagination(t *testing.T) {
	f := newAppleMusicFixture(t, time.Unix(1_750_000_000, 0).UTC())
	listSongs := func(query map[string]string) starlark.Response {
		return f.catalog("on_list_songs", "/v1/catalog/us/songs", sfParams("us", ""), query)
	}

	// ===== pages walk by offset and the last partial page has no next =====
	r := listSongs(map[string]string{"limit": "2"})
	if r.Body["href"] != "/v1/catalog/us/songs?limit=2&offset=0" {
		t.Fatalf("href echo = %v", r.Body["href"])
	}
	page1 := r.Body["data"].([]any)
	if len(page1) != 2 || page1[0].(map[string]any)["id"] != amSong1 || page1[1].(map[string]any)["id"] != amSong2 {
		t.Fatalf("page 1 = %v", page1)
	}
	if r.Body["next"] != "/v1/catalog/us/songs?limit=2&offset=2" {
		t.Fatalf("next = %v", r.Body["next"])
	}
	r = listSongs(map[string]string{"limit": "2", "offset": "2"})
	if len(r.Body["data"].([]any)) != 1 || r.Body["data"].([]any)[0].(map[string]any)["id"] != amSong3 {
		t.Fatalf("page 2 = %v", r.Body["data"])
	}
	if _, has := r.Body["next"]; has {
		t.Fatalf("final page carries next = %v", r.Body["next"])
	}
	// Off the end: empty data, no next.
	r = listSongs(map[string]string{"limit": "2", "offset": "99"})
	if len(r.Body["data"].([]any)) != 0 {
		t.Fatalf("offset past end data = %v", r.Body["data"])
	}

	// ===== limits clamp to the endpoint default and max =====
	r = listSongs(nil)
	if r.Body["href"] != "/v1/catalog/us/songs?limit=20&offset=0" {
		t.Fatalf("default limit href = %v (want 20)", r.Body["href"])
	}
	if _, has := r.Body["next"]; has {
		t.Fatal("whole catalog in one page must not carry next")
	}
	if r := listSongs(map[string]string{"limit": "500"}); r.Body["href"] != "/v1/catalog/us/songs?limit=100&offset=0" {
		t.Fatalf("limit=500 clamped to %v, want 100", r.Body["href"])
	}
	// Search has its own default (5) and max (25) per type group.
	sr := f.catalog("on_search", "/v1/catalog/us/search", sfParams("us", ""),
		map[string]string{"term": "Neon", "types": "songs"})
	group := sr.Body["results"].(map[string]any)["songs"].(map[string]any)
	if !strings.Contains(group["href"].(string), "limit=5&offset=0") {
		t.Fatalf("search default limit href = %v, want limit=5", group["href"])
	}
	sr = f.catalog("on_search", "/v1/catalog/us/search", sfParams("us", ""),
		map[string]string{"term": "Neon", "types": "songs", "limit": "50"})
	group = sr.Body["results"].(map[string]any)["songs"].(map[string]any)
	if !strings.Contains(group["href"].(string), "limit=25") {
		t.Fatalf("search limit=50 clamped href = %v, want limit=25", group["href"])
	}

	// ===== library lists page with meta.total =====
	lr := f.me("on_library_songs", "GET", "/v1/me/library/songs", nil, map[string]string{"limit": "1"}, nil)
	if lr.Status != 200 {
		t.Fatalf("library songs limit=1 -> %d: %v", lr.Status, lr.Body)
	}
	if len(lr.Body["data"].([]any)) != 1 {
		t.Fatalf("library page = %v", lr.Body["data"])
	}
	if lr.Body["meta"].(map[string]any)["total"] != int64(2) {
		t.Fatalf("meta.total = %v, want the 2 seeded songs", lr.Body["meta"])
	}
	if lr.Body["next"] != "/v1/me/library/songs?limit=1&offset=1" {
		t.Fatalf("library next = %v", lr.Body["next"])
	}
}

// TestAppleMusicLibraryUserTokenSurface: /v1/me demands BOTH credentials,
// adding/removing library resources is idempotent and addressable by
// catalog or library id, played bumps playCount and stamps lastPlayedDate
// on the virtual clock, and recently-added lists newest first.
func TestAppleMusicLibraryUserTokenSurface(t *testing.T) {
	start := time.Unix(1_750_000_000, 0).UTC()
	f := newAppleMusicFixture(t, start)

	// ===== /v1/me checks the developer JWT before the Music-User-Token =====
	r := f.call(f.vmLibrary, "on_library_songs", "GET", "/v1/me/library/songs", nil, nil, nil, "", "")
	if r.Status != 401 || errOf(t, r)["title"] != "Authentication credentials are missing or invalid." {
		t.Fatalf("no credentials -> %d %v", r.Status, r.Body)
	}
	r = f.call(f.vmLibrary, "on_library_songs", "GET", "/v1/me/library/songs", nil, nil, nil, "", amUMT)
	if r.Status != 401 || errOf(t, r)["title"] != "Authentication credentials are missing or invalid." {
		t.Fatalf("user token alone -> %d %v, want the JWT gate first", r.Status, r.Body)
	}
	r = f.call(f.vmLibrary, "on_library_songs", "GET", "/v1/me/library/songs", nil, nil, nil, "Bearer "+amDevJWT(), "")
	if r.Status != 401 || errOf(t, r)["title"] != "Music-User-Token is required for library access." {
		t.Fatalf("missing Music-User-Token -> %d %v", r.Status, r.Body)
	}

	// ===== adding resources to the library is idempotent and validates ids =====
	add := func(query map[string]string, body map[string]any) starlark.Response {
		return f.me("on_add_library", "POST", "/v1/me/library", nil, query, body)
	}
	libTotal := func() int64 {
		r := f.me("on_library_songs", "GET", "/v1/me/library/songs", nil, nil, nil)
		if r.Status != 200 {
			t.Fatalf("library songs -> %d: %v", r.Status, r.Body)
		}
		return r.Body["meta"].(map[string]any)["total"].(int64)
	}
	if r := add(map[string]string{"ids[songs]": amSong3}, nil); r.Status != 204 {
		t.Fatalf("add song -> %d: %v", r.Status, r.Body)
	}
	if r := add(map[string]string{"ids[songs]": amSong3}, nil); r.Status != 204 || libTotal() != 3 {
		t.Fatalf("re-add -> %d total %d, want 204 and still 3 (idempotent)", r.Status, libTotal())
	}
	if r := add(map[string]string{"ids[songs]": "999"}, nil); r.Status != 404 ||
		errOf(t, r)["title"] != "Resource 'songs' with id '999' not found." {
		t.Fatalf("add unknown song -> %d %v", r.Status, r.Body)
	}
	if r := add(nil, map[string]any{}); r.Status != 400 ||
		errOf(t, r)["detail"] != "Provide catalog resource ids via ids[songs], ids[albums] or ids[playlists]." {
		t.Fatalf("add without ids -> %d %v", r.Status, r.Body)
	}
	// The body form {"ids": {...}} works too (a JSON list of ids).
	if r := add(nil, map[string]any{"ids": map[string]any{"albums": []any{amAlbum2}}}); r.Status != 204 {
		t.Fatalf("add album via body -> %d: %v", r.Status, r.Body)
	}
	lar := f.me("on_library_albums", "GET", "/v1/me/library/albums", nil, nil, nil)
	albums := lar.Body["data"].([]any)
	if len(albums) != 2 {
		t.Fatalf("library albums = %d, want seed + added", len(albums))
	}

	// ===== played bumps playCount and stamps lastPlayedDate at play time =====
	played := f.me("on_played", "POST", "/v1/me/played", nil, nil,
		map[string]any{"id": amSong3, "type": "songs"})
	if played.Status != 204 {
		t.Fatalf("played -> %d: %v", played.Status, played.Body)
	}
	findAdded := func() map[string]any {
		r := f.me("on_library_songs", "GET", "/v1/me/library/songs", nil, nil, nil)
		for _, item := range r.Body["data"].([]any) {
			m := item.(map[string]any)
			if m["attributes"].(map[string]any)["name"] == "Acoustic Dawn" {
				return m["attributes"].(map[string]any)
			}
		}
		t.Fatalf("Acoustic Dawn missing from library: %v", r.Body["data"])
		return nil
	}
	attrs := findAdded()
	if !amNum(attrs["playCount"], 1) {
		t.Fatalf("playCount after first play = %v (%T), want 1", attrs["playCount"], attrs["playCount"])
	}
	if attrs["lastPlayedDate"] != start.Format(time.RFC3339) {
		t.Fatalf("lastPlayedDate = %v, want the virtual clock's now", attrs["lastPlayedDate"])
	}
	f.vc.Advance(90 * time.Minute)
	if r := f.me("on_played", "POST", "/v1/me/played", nil, nil,
		map[string]any{"id": amSong3, "type": "songs"}); r.Status != 204 {
		t.Fatalf("second played -> %d", r.Status)
	}
	attrs = findAdded()
	if !amNum(attrs["playCount"], 2) {
		t.Fatalf("playCount after second play = %v (%T), want 2", attrs["playCount"], attrs["playCount"])
	}
	if attrs["lastPlayedDate"] != start.Add(90*time.Minute).Format(time.RFC3339) {
		t.Fatalf("lastPlayedDate = %v, want the advanced clock", attrs["lastPlayedDate"])
	}
	// Unknown song and non-song types keep the documented shapes.
	if r := f.me("on_played", "POST", "/v1/me/played", nil, nil,
		map[string]any{"id": "nope"}); r.Status != 404 {
		t.Fatalf("played unknown -> %d, want 404", r.Status)
	}
	if r := f.me("on_played", "POST", "/v1/me/played", nil, nil,
		map[string]any{"id": amSong1, "type": "albums"}); r.Status != 400 ||
		errOf(t, r)["detail"] != "Unsupported played resource type: albums" {
		t.Fatalf("played album -> %d %v", r.Status, r.Body)
	}

	// ===== recently-added lists newest first across mixed resource types =====
	f.vc.Advance(time.Minute) // separate the next addition's timestamp
	// Add an editorial playlist after the clock advanced: it must lead the list.
	if r := add(map[string]string{"ids[playlists]": amPlaylist1}, nil); r.Status != 204 {
		t.Fatalf("add playlist -> %d: %v", r.Status, r.Body)
	}
	ra := f.me("on_recently_added", "GET", "/v1/me/library/recently-added", nil, nil, nil)
	if ra.Status != 200 {
		t.Fatalf("recently-added -> %d: %v", ra.Status, ra.Body)
	}
	raData := ra.Body["data"].([]any)
	// Seed 5 (2 songs, 1 album, 2 playlists) + the song, album and playlist
	// added above.
	if len(raData) != 8 {
		t.Fatalf("recently-added has %d rows, want 8", len(raData))
	}
	row := func(i int) (string, string) {
		m := raData[i].(map[string]any)
		return m["type"].(string), m["attributes"].(map[string]any)["name"].(string)
	}
	if typ, name := row(0); typ != "library-playlists" || name != "Synth Horizons" {
		t.Fatalf("newest row = %s %q, want the just-added playlist", typ, name)
	}
	// The T0 additions come next — songs before albums (the adapter's flat
	// order under the stable sort) — then the T0-3d and T0-8d seed rows.
	if typ, name := row(1); typ != "library-songs" || name != "Acoustic Dawn" {
		t.Fatalf("row 1 = %s %q, want the added song", typ, name)
	}
	if typ, name := row(2); typ != "library-albums" || name != "Daybreak" {
		t.Fatalf("row 2 = %s %q, want the added album", typ, name)
	}
	if typ, name := row(7); typ != "library-albums" || name != "Retrograde" {
		t.Fatalf("oldest row = %s %q, want the seeded album", typ, name)
	}
	kinds := map[string]int{}
	for _, item := range raData {
		kinds[item.(map[string]any)["type"].(string)]++
	}
	if kinds["library-songs"] != 3 || kinds["library-playlists"] != 3 || kinds["library-albums"] != 2 {
		t.Fatalf("recently-added kinds = %v", kinds)
	}

	// ===== fields[type] projects library attributes =====
	lp := f.me("on_library_playlists", "GET", "/v1/me/library/playlists", nil,
		map[string]string{"fields[library-playlists]": "name"}, nil)
	if lp.Status != 200 {
		t.Fatalf("playlists with fields -> %d: %v", lp.Status, lp.Body)
	}
	pattrs := lp.Body["data"].([]any)[0].(map[string]any)["attributes"].(map[string]any)
	if len(pattrs) != 1 || pattrs["name"] != "Road Trip Mix" {
		t.Fatalf("projected playlist attributes = %v, want only name", pattrs)
	}
	// Unprojected playlists still expose the tracks relationship with
	// library-song ids.
	full := f.me("on_library_playlists", "GET", "/v1/me/library/playlists", nil, nil, nil)
	rels := full.Body["data"].([]any)[0].(map[string]any)["relationships"].(map[string]any)
	trackIDs := rels["tracks"].(map[string]any)["data"].([]any)
	if len(trackIDs) != 2 || trackIDs[0].(map[string]any)["id"] != "i.libsong1" {
		t.Fatalf("playlist tracks = %v, want the seeded library-song ids", trackIDs)
	}

	// ===== delete addresses the catalog id or the library id, then 404s =====
	del := func(kind, id string) starlark.Response {
		return f.me("on_delete_library", "DELETE", "/v1/me/library/"+kind+"/"+id,
			map[string]string{"type": kind, "id": id}, nil, nil)
	}
	if r := del("songs", amSong1); r.Status != 204 { // by catalog id
		t.Fatalf("delete by catalog id -> %d", r.Status)
	}
	if r := del("songs", amSong1); r.Status != 404 {
		t.Fatalf("delete again -> %d, want 404", r.Status)
	}
	if r := del("songs", "i.libsong2"); r.Status != 204 { // by library id
		t.Fatalf("delete by library id -> %d", r.Status)
	}
	if r := del("stations", "x"); r.Status != 400 ||
		errOf(t, r)["detail"] != "Unsupported library resource type: stations" {
		t.Fatalf("delete stations -> %d %v", r.Status, r.Body)
	}
	if got := libTotal(); got != 1 {
		t.Fatalf("library songs after deletes = %d, want only the added song", got)
	}
}

// TestAppleMusicRatings: unrated resources read value 0, PUT love/dislike/
// clear round-trips with 201 (float JSON values included), and invalid
// values or unknown targets keep Apple's 400/404 shapes.
func TestAppleMusicRatings(t *testing.T) {
	f := newAppleMusicFixture(t, time.Unix(1_750_000_000, 0).UTC())
	getRating := func(kind, id string) starlark.Response {
		return f.me("on_get_rating", "GET", "/v1/me/ratings/"+kind+"/"+id,
			map[string]string{"type": kind, "id": id}, nil, nil)
	}
	putRating := func(kind, id string, body map[string]any) starlark.Response {
		return f.me("on_put_rating", "PUT", "/v1/me/ratings/"+kind+"/"+id,
			map[string]string{"type": kind, "id": id}, nil, body)
	}
	ratingValue := func(r starlark.Response) any {
		data := r.Body["data"].([]any)
		return data[0].(map[string]any)["attributes"].(map[string]any)["value"]
	}

	// ===== an unrated resource reads 0; love/dislike/clear round-trip =====
	r := getRating("songs", amSong1)
	if r.Status != 200 || !amNum(ratingValue(r), 0) {
		t.Fatalf("unrated rating -> %d %v, want value 0", r.Status, r.Body)
	}
	if data := r.Body["data"].([]any)[0].(map[string]any); data["type"] != "ratings" ||
		data["href"] != "/v1/me/ratings/songs/"+amSong1 {
		t.Fatalf("rating resource = %v", data)
	}
	for _, v := range []int{1, -1, 0} {
		r := putRating("songs", amSong1, map[string]any{
			"type": "ratings", "id": amSong1, "attributes": map[string]any{"value": v},
		})
		if r.Status != 201 || !amNum(ratingValue(r), int64(v)) {
			t.Fatalf("put rating %d -> %d %v", v, r.Status, r.Body)
		}
		if got := ratingValue(getRating("songs", amSong1)); !amNum(got, int64(v)) {
			t.Fatalf("read-back after put %d = %v (%T)", v, got, got)
		}
	}
	// A JSON float value (numbers arrive as floats after a raw-body decode)
	// is coerced, not rejected.
	raw := f.callRaw(f.vmLibrary, "on_put_rating", "PUT", "/v1/me/ratings/songs/"+amSong1,
		map[string]string{"type": "songs", "id": amSong1}, nil,
		`{"type":"ratings","id":"`+amSong1+`","attributes":{"value":1.0}}`,
		"Bearer "+amDevJWT(), amUMT)
	if raw.Status != 201 || !amNum(ratingValue(raw), 1) {
		t.Fatalf("float value via raw body -> %d %v, want 201 value 1", raw.Status, raw.Body)
	}

	// ===== invalid values and unknown targets keep the documented shapes =====
	if r := putRating("songs", amSong1, map[string]any{"attributes": map[string]any{"value": 7}}); r.Status != 400 ||
		errOf(t, r)["detail"] != "attributes.value must be -1, 0 or 1." {
		t.Fatalf("value 7 -> %d %v", r.Status, r.Body)
	}
	if r := putRating("songs", amSong1, map[string]any{"attributes": map[string]any{"value": "1"}}); r.Status != 400 {
		t.Fatalf("string value -> %d, want 400", r.Status)
	}
	if r := putRating("songs", amSong1, map[string]any{"id": amSong1}); r.Status != 400 ||
		errOf(t, r)["detail"] != "attributes with a value is required." {
		t.Fatalf("no attributes -> %d %v", r.Status, r.Body)
	}
	if r := getRating("songs", "999"); r.Status != 404 {
		t.Fatalf("rate unknown song -> %d, want 404", r.Status)
	}
	if r := getRating("stations", "x"); r.Status != 400 ||
		errOf(t, r)["detail"] != "Unsupported rating resource type: stations" {
		t.Fatalf("rate stations -> %d %v", r.Status, r.Body)
	}
	// Library playlists are ratable alongside catalog playlists.
	if r := getRating("playlists", "p.libplay1"); r.Status != 200 {
		t.Fatalf("rate library playlist -> %d %v", r.Status, r.Body)
	}
	if r := getRating("playlists", "pl.nope"); r.Status != 404 {
		t.Fatalf("rate unknown playlist -> %d, want 404", r.Status)
	}
}
