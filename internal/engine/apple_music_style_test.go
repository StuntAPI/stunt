package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// makeAppleMusicJWT creates a structurally-valid ES256 JWT (header.payload.sig)
// with the kid claim, sufficient for the adapter's structural validation.
func makeAppleMusicJWT() string {
	header := `{"alg":"ES256","kid":"TESTKEY123","typ":"JWT"}`
	payload := `{"iss":"TEAMID123","iat":1700000000,"exp":1900000000}`
	h := base64.RawURLEncoding.EncodeToString([]byte(header))
	p := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return h + "." + p + ".c3ludGhldGljLXNpZ25hdHVyZQ"
}

// Seeded catalog ids (assembled at runtime inside the adapter).
const (
	amSong1  = "1440818839"
	amSong3  = "1440818841"
	amAlbum1 = "1440818830"
)

// TestAppleMusicStyleAdapter exercises the apple-music-style adapter:
//
//   - JWT required: 401 without auth
//   - GET song / album / artist → resource with id, type, attributes
//   - Search → real grouped envelope (results.songs.data) + next links
//   - Library endpoints require Music-User-Token
//   - Library seed: songs, playlists, recently-added
//   - POST /v1/me/library (add), DELETE /v1/me/library/{type}/{id}
//   - POST /v1/me/played bumps playCount
//   - Ratings GET/PUT with -1|0|1 validation
func TestAppleMusicStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "apple-music-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"music": {Adapter: absAdapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(50 * time.Millisecond)

	base := addrs["music"]
	jwt := makeAppleMusicJWT()

	// ===== 401 without auth =====

	body, status := appleMusicGet(t, base+"/v1/catalog/us/songs/"+amSong1, "", "")
	if status != 401 {
		t.Fatalf("get song without auth -> status %d, want 401; body %s", status, body)
	}

	// ===== GET song → resource =====

	body, status = appleMusicGet(t, base+"/v1/catalog/us/songs/"+amSong1, jwt, "")
	if status != 200 {
		t.Fatalf("get song -> status %d, want 200; body %s", status, body)
	}
	song := appleMusicData1(t, body)
	if song["type"] != "songs" {
		t.Fatalf("type = %v, want songs", song["type"])
	}
	if song["id"] != amSong1 {
		t.Fatalf("id = %v, want %s", song["id"], amSong1)
	}
	if song["href"] != "/v1/catalog/us/songs/"+amSong1 {
		t.Fatalf("href = %v, want storefront-derived href", song["href"])
	}
	attrs, ok := song["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("attributes = %v, want object", song["attributes"])
	}
	if _, ok := attrs["name"].(string); !ok {
		t.Fatalf("name = %v, want string", attrs["name"])
	}
	if _, ok := attrs["durationInMillis"].(float64); !ok {
		t.Fatalf("durationInMillis = %v, want number", attrs["durationInMillis"])
	}
	artwork, ok := attrs["artwork"].(map[string]any)
	if !ok {
		t.Fatalf("artwork = %v, want object", attrs["artwork"])
	}
	if _, ok := artwork["url"].(string); !ok {
		t.Fatalf("artwork.url = %v, want string", artwork["url"])
	}

	// ===== GET unknown song → 404 =====

	body, status = appleMusicGet(t, base+"/v1/catalog/us/songs/999", jwt, "")
	if status != 404 {
		t.Fatalf("unknown song -> status %d, want 404; body %s", status, body)
	}

	// ===== GET album → resource =====

	body, status = appleMusicGet(t, base+"/v1/catalog/us/albums/"+amAlbum1, jwt, "")
	if status != 200 {
		t.Fatalf("get album -> status %d, want 200; body %s", status, body)
	}
	album := appleMusicData1(t, body)
	if album["type"] != "albums" {
		t.Fatalf("album type = %v, want albums", album["type"])
	}

	// ===== List artists → paged collection =====

	body, status = appleMusicGet(t, base+"/v1/catalog/us/artists?limit=1", jwt, "")
	if status != 200 {
		t.Fatalf("list artists -> status %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal artists: %v (body %s)", err, body)
	}
	artistData, ok := listResp["data"].([]any)
	if !ok || len(artistData) != 1 {
		t.Fatalf("artists data = %v, want 1 (limit=1)", listResp["data"])
	}
	if artistData[0].(map[string]any)["type"] != "artists" {
		t.Fatalf("artist type = %v, want artists", artistData[0])
	}
	next, ok := listResp["next"].(string)
	if !ok || next == "" {
		t.Fatalf("next = %v, want a next link with limit=1", listResp["next"])
	}

	// ===== Charts over the seed =====

	body, status = appleMusicGet(t, base+"/v1/catalog/us/charts?types=songs", jwt, "")
	if status != 200 {
		t.Fatalf("charts -> status %d, want 200; body %s", status, body)
	}
	var charts map[string]any
	if err := json.Unmarshal([]byte(body), &charts); err != nil {
		t.Fatalf("unmarshal charts: %v (body %s)", err, body)
	}
	results, ok := charts["results"].(map[string]any)
	if !ok {
		t.Fatalf("charts.results = %v, want object", charts["results"])
	}
	songCharts, ok := results["songs"].([]any)
	if !ok || len(songCharts) != 1 {
		t.Fatalf("results.songs = %v, want one chart", results["songs"])
	}
	chart := songCharts[0].(map[string]any)
	if chart["name"] != "Top Songs" {
		t.Fatalf("chart name = %v, want Top Songs", chart["name"])
	}
	if _, ok := chart["chart"].([]any); !ok {
		t.Fatalf("chart.chart = %v, want array", chart["chart"])
	}

	// ===== Search → real grouped envelope =====

	body, status = appleMusicGet(t, base+"/v1/catalog/us/search?term=Neon&types=songs&limit=1", jwt, "")
	if status != 200 {
		t.Fatalf("search -> status %d, want 200; body %s", status, body)
	}
	var searchResp map[string]any
	if err := json.Unmarshal([]byte(body), &searchResp); err != nil {
		t.Fatalf("unmarshal search: %v (body %s)", err, body)
	}
	searchResults, ok := searchResp["results"].(map[string]any)
	if !ok {
		t.Fatalf("search results = %v, want results.{type} groups", searchResp["results"])
	}
	songGroup, ok := searchResults["songs"].(map[string]any)
	if !ok {
		t.Fatalf("results.songs = %v, want group object", searchResults["songs"])
	}
	groupData, ok := songGroup["data"].([]any)
	if !ok || len(groupData) != 1 {
		t.Fatalf("results.songs.data = %v, want 1 (limit=1)", songGroup["data"])
	}
	if groupData[0].(map[string]any)["type"] != "songs" {
		t.Fatalf("search result type = %v, want songs", groupData[0])
	}
	if next, ok := songGroup["next"].(string); !ok || next == "" {
		t.Fatalf("results.songs.next = %v, want offset next link", songGroup["next"])
	}
	meta, ok := searchResp["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta = %v, want object", searchResp["meta"])
	}
	if _, ok := meta["results"].(map[string]any); !ok {
		t.Fatalf("meta.results = %v, want order object", meta["results"])
	}

	// ===== Search without term → 400 =====

	body, status = appleMusicGet(t, base+"/v1/catalog/us/search?types=songs", jwt, "")
	if status != 400 {
		t.Fatalf("search without term -> status %d, want 400; body %s", status, body)
	}

	// ===== Library songs without Music-User-Token → 401 =====

	body, status = appleMusicGet(t, base+"/v1/me/library/songs", jwt, "")
	if status != 401 {
		t.Fatalf("library songs without Music-User-Token -> status %d, want 401; body %s", status, body)
	}

	umt := "test-user-token-123"

	// ===== Library songs with Music-User-Token → seeded library =====

	body, status = appleMusicGet(t, base+"/v1/me/library/songs", jwt, umt)
	if status != 200 {
		t.Fatalf("library songs -> status %d, want 200; body %s", status, body)
	}
	var libResp map[string]any
	if err := json.Unmarshal([]byte(body), &libResp); err != nil {
		t.Fatalf("unmarshal library: %v (body %s)", err, body)
	}
	libData, ok := libResp["data"].([]any)
	if !ok || len(libData) < 2 {
		t.Fatalf("library data = %v, want >=2 seeded songs", libResp["data"])
	}
	first := libData[0].(map[string]any)
	if first["type"] != "library-songs" {
		t.Fatalf("library song type = %v, want library-songs", first["type"])
	}
	libMeta, ok := libResp["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta = %v, want total", libResp["meta"])
	}
	if _, ok := libMeta["total"].(float64); !ok {
		t.Fatalf("meta.total = %v, want number", libMeta["total"])
	}

	// ===== Library playlists → tracks relationship =====

	body, status = appleMusicGet(t, base+"/v1/me/library/playlists", jwt, umt)
	if status != 200 {
		t.Fatalf("library playlists -> status %d, want 200; body %s", status, body)
	}
	var plResp map[string]any
	if err := json.Unmarshal([]byte(body), &plResp); err != nil {
		t.Fatalf("unmarshal playlists: %v (body %s)", err, body)
	}
	plData, ok := plResp["data"].([]any)
	if !ok || len(plData) < 2 {
		t.Fatalf("playlists = %v, want >=2 seeded playlists", plResp["data"])
	}
	pl := plData[0].(map[string]any)
	if pl["type"] != "library-playlists" {
		t.Fatalf("playlist type = %v, want library-playlists", pl["type"])
	}
	rels, ok := pl["relationships"].(map[string]any)
	if !ok {
		t.Fatalf("relationships = %v, want tracks", pl["relationships"])
	}
	tracks, ok := rels["tracks"].(map[string]any)
	if !ok {
		t.Fatalf("relationships.tracks = %v, want object", rels["tracks"])
	}
	if _, ok := tracks["data"].([]any); !ok {
		t.Fatalf("tracks.data = %v, want array", tracks["data"])
	}

	// ===== Recently-added → mixed resource types =====

	body, status = appleMusicGet(t, base+"/v1/me/library/recently-added", jwt, umt)
	if status != 200 {
		t.Fatalf("recently-added -> status %d, want 200; body %s", status, body)
	}
	var raResp map[string]any
	if err := json.Unmarshal([]byte(body), &raResp); err != nil {
		t.Fatalf("unmarshal recently-added: %v (body %s)", err, body)
	}
	raData, ok := raResp["data"].([]any)
	if !ok || len(raData) < 3 {
		t.Fatalf("recently-added = %v, want mixed seed", raResp["data"])
	}
	kinds := map[string]bool{}
	for _, item := range raData {
		kinds[item.(map[string]any)["type"].(string)] = true
	}
	if !kinds["library-songs"] || !kinds["library-playlists"] {
		t.Fatalf("recently-added kinds = %v, want songs + playlists", kinds)
	}

	// ===== POST /v1/me/library adds a catalog song (204) =====

	body, status = appleMusicPost(t, base+"/v1/me/library?ids%5Bsongs%5D="+amSong3, jwt, umt, nil)
	if status != 204 {
		t.Fatalf("add to library -> status %d, want 204; body %s", status, body)
	}

	body, status = appleMusicGet(t, base+"/v1/me/library/songs", jwt, umt)
	if status != 200 {
		t.Fatalf("library after add -> status %d, want 200", status)
	}
	if err := json.Unmarshal([]byte(body), &libResp); err != nil {
		t.Fatalf("unmarshal library after add: %v", err)
	}
	found := false
	for _, item := range libResp["data"].([]any) {
		attrs := item.(map[string]any)["attributes"].(map[string]any)
		if attrs["name"] == "Acoustic Dawn" {
			found = true
		}
	}
	if !found {
		t.Fatalf("added song 'Acoustic Dawn' not in library: %v", libResp["data"])
	}

	// ===== POST /v1/me/library with an unknown id → 404 =====

	body, status = appleMusicPost(t, base+"/v1/me/library?ids%5Bsongs%5D=999", jwt, umt, nil)
	if status != 404 {
		t.Fatalf("add unknown -> status %d, want 404; body %s", status, body)
	}

	// ===== POST /v1/me/played bumps playCount =====

	body, status = appleMusicPost(t, base+"/v1/me/played", jwt, umt, map[string]any{"id": amSong3, "type": "songs"})
	if status != 204 {
		t.Fatalf("played -> status %d, want 204; body %s", status, body)
	}

	body, status = appleMusicGet(t, base+"/v1/me/library/songs", jwt, umt)
	if status != 200 {
		t.Fatalf("library after played -> status %d, want 200", status)
	}
	if err := json.Unmarshal([]byte(body), &libResp); err != nil {
		t.Fatalf("unmarshal library after played: %v", err)
	}
	playedCount := -1.0
	for _, item := range libResp["data"].([]any) {
		attrs := item.(map[string]any)["attributes"].(map[string]any)
		if attrs["name"] == "Acoustic Dawn" {
			playedCount, _ = attrs["playCount"].(float64)
		}
	}
	if playedCount < 1 {
		t.Fatalf("playCount after played = %v, want >= 1", playedCount)
	}

	// ===== POST /v1/me/played with an unknown song → 404 =====

	body, status = appleMusicPost(t, base+"/v1/me/played", jwt, umt, map[string]any{"id": "does-not-exist"})
	if status != 404 {
		t.Fatalf("played unknown -> status %d, want 404; body %s", status, body)
	}

	// ===== DELETE /v1/me/library/songs/{id} (by catalog id) → 204 then 404 =====

	body, status = appleMusicDo(t, "DELETE", base+"/v1/me/library/songs/"+amSong1, jwt, umt, nil)
	if status != 204 {
		t.Fatalf("delete library song -> status %d, want 204; body %s", status, body)
	}
	body, status = appleMusicDo(t, "DELETE", base+"/v1/me/library/songs/"+amSong1, jwt, umt, nil)
	if status != 404 {
		t.Fatalf("delete again -> status %d, want 404; body %s", status, body)
	}

	// ===== Ratings: GET unrated (0), PUT love (201), GET (1), invalid (400) =====

	body, status = appleMusicGet(t, base+"/v1/me/ratings/songs/"+amSong1, jwt, umt)
	if status != 200 {
		t.Fatalf("get rating -> status %d, want 200; body %s", status, body)
	}
	rating := appleMusicData1(t, body)
	if rating["type"] != "ratings" {
		t.Fatalf("rating type = %v, want ratings", rating["type"])
	}
	if rating["attributes"].(map[string]any)["value"] != float64(0) {
		t.Fatalf("unrated value = %v, want 0", rating["attributes"])
	}

	body, status = appleMusicDo(t, "PUT", base+"/v1/me/ratings/songs/"+amSong1, jwt, umt,
		map[string]any{"type": "ratings", "id": amSong1, "attributes": map[string]any{"value": 1}})
	if status != 201 {
		t.Fatalf("put rating -> status %d, want 201; body %s", status, body)
	}
	rating = appleMusicData1(t, body)
	if rating["attributes"].(map[string]any)["value"] != float64(1) {
		t.Fatalf("put rating value = %v, want 1", rating["attributes"])
	}

	body, status = appleMusicGet(t, base+"/v1/me/ratings/songs/"+amSong1, jwt, umt)
	if status != 200 {
		t.Fatalf("get rating after put -> status %d, want 200", status)
	}
	rating = appleMusicData1(t, body)
	if rating["attributes"].(map[string]any)["value"] != float64(1) {
		t.Fatalf("rating after put = %v, want 1", rating["attributes"])
	}

	body, status = appleMusicDo(t, "PUT", base+"/v1/me/ratings/songs/"+amSong1, jwt, umt,
		map[string]any{"type": "ratings", "id": amSong1, "attributes": map[string]any{"value": 7}})
	if status != 400 {
		t.Fatalf("put invalid rating -> status %d, want 400; body %s", status, body)
	}

	// ===== Rating an unknown song → 404 =====

	body, status = appleMusicGet(t, base+"/v1/me/ratings/songs/999", jwt, umt)
	if status != 404 {
		t.Fatalf("rate unknown song -> status %d, want 404; body %s", status, body)
	}

	// ===== Undecodable body → 400 (not 500) =====

	req, err := http.NewRequest("POST", base+"/v1/me/library?ids%5Bsongs%5D=x", bytes.NewReader([]byte("not json")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Music-User-Token", umt)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	b, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != 400 {
		t.Fatalf("undecodable body -> status %d, want 400; body %s", resp2.StatusCode, string(b))
	}
}

// appleMusicData1 asserts the response has exactly one resource in
// {data:[…]} and returns it.
func appleMusicData1(t *testing.T, body string) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	data, ok := resp["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("data = %v, want array of 1", resp["data"])
	}
	return data[0].(map[string]any)
}

// === Apple Music test helpers ===

func appleMusicGet(t *testing.T, rawurl, token, umt string) (string, int) {
	t.Helper()
	return appleMusicDo(t, "GET", rawurl, token, umt, nil)
}

func appleMusicPost(t *testing.T, rawurl, token, umt string, payload map[string]any) (string, int) {
	t.Helper()
	return appleMusicDo(t, "POST", rawurl, token, umt, payload)
}

func appleMusicDo(t *testing.T, method, rawurl, token, umt string, payload map[string]any) (string, int) {
	t.Helper()
	var reader io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, rawurl, reader)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if umt != "" {
		req.Header.Set("Music-User-Token", umt)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
