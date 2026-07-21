package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
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

// TestAppleMusicStyleAdapter exercises the apple-music-style adapter:
//
//   - JWT required: 401 without auth
//   - GET song → resource with id, type, attributes
//   - GET album → resource
//   - Search by term → matching results
//   - Library songs require Music-User-Token
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

	body, status := appleMusicGet(t, base+"/v1/catalog/us/songs/1440818839", "")
	if status != 401 {
		t.Fatalf("get song without auth -> status %d, want 401; body %s", status, body)
	}

	// ===== GET song → resource =====

	body, status = appleMusicGet(t, base+"/v1/catalog/us/songs/1440818839", jwt)
	if status != 200 {
		t.Fatalf("get song -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	data, ok := resp["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("data = %v, want array of 1", resp["data"])
	}
	song := data[0].(map[string]any)
	if song["type"] != "songs" {
		t.Fatalf("type = %v, want songs", song["type"])
	}
	if song["id"] != "1440818839" {
		t.Fatalf("id = %v, want 1440818839", song["id"])
	}
	attrs, ok := song["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("attributes = %v, want object", song["attributes"])
	}
	if _, ok := attrs["name"].(string); !ok {
		t.Fatalf("name = %v, want string", attrs["name"])
	}
	if _, ok := attrs["artistName"].(string); !ok {
		t.Fatalf("artistName = %v, want string", attrs["artistName"])
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
	genres, ok := attrs["genreNames"].([]any)
	if !ok || len(genres) == 0 {
		t.Fatalf("genreNames = %v, want non-empty array", attrs["genreNames"])
	}

	// ===== GET album → resource =====

	body, status = appleMusicGet(t, base+"/v1/catalog/us/albums/1440818830", jwt)
	if status != 200 {
		t.Fatalf("get album -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal album: %v (body %s)", err, body)
	}
	data = resp["data"].([]any)
	album := data[0].(map[string]any)
	if album["type"] != "albums" {
		t.Fatalf("album type = %v, want albums", album["type"])
	}

	// ===== Search by term → results =====

	body, status = appleMusicGet(t, base+"/v1/catalog/us/search?term=Neon&types=songs", jwt)
	if status != 200 {
		t.Fatalf("search -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal search: %v (body %s)", err, body)
	}
	data = resp["data"].([]any)
	if len(data) < 1 {
		t.Fatalf("search results count = %d, want >= 1 for term 'Neon'", len(data))
	}
	// Verify each result is a song.
	for _, item := range data {
		s := item.(map[string]any)
		if s["type"] != "songs" {
			t.Fatalf("search result type = %v, want songs", s["type"])
		}
	}

	// ===== Library songs without Music-User-Token → 401 =====

	body, status = appleMusicGet(t, base+"/v1/me/library/songs", jwt)
	if status != 401 {
		t.Fatalf("library songs without Music-User-Token -> status %d, want 401; body %s", status, body)
	}

	// ===== Library songs with Music-User-Token → 200 =====

	req, err := http.NewRequest("GET", base+"/v1/me/library/songs", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Music-User-Token", "test-user-token-123")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	b, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != 200 {
		t.Fatalf("library songs -> status %d, want 200; body %s", resp2.StatusCode, string(b))
	}
	var libResp map[string]any
	if err := json.Unmarshal(b, &libResp); err != nil {
		t.Fatalf("unmarshal library: %v (body %s)", err, string(b))
	}
	if _, ok := libResp["data"].([]any); !ok {
		t.Fatalf("library data = %v, want array", libResp["data"])
	}
}

// === Apple Music test helpers ===

func appleMusicGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// Guard: ensure we don't accidentally import strings without using it.
var _ = strings.Contains
