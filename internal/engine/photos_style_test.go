package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestPhotosStyleAdapter exercises the Google Photos Library reference adapter
// end-to-end through the full OAuth2 + two-step upload + search + albums flow:
//
//   - OAuth2 authorize → 302; token exchange → bearer pair
//   - POST /v1/uploads → uploadToken (plain text)
//   - POST /v1/mediaItems:batchCreate → mediaItem with id, baseUrl, mediaMetadata
//   - POST /v1/mediaItems:search shows the created item (STATEFUL)
//   - GET /v1/mediaItems lists the item
//   - albums: create → get → list
func TestPhotosStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "photos-style")
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
			"photos": {Adapter: absAdapterDir},
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

	base := addrs["photos"]

	// ===== OAuth2 → get access token =====

	const redirectURI = "http://localhost:8080/callback"
	const clientID = "photos-test-client-id"
	const clientSecret = "photos-test-client-secret"

	code := photosAuthorize(t, base, redirectURI, "state-1", clientID)
	accessToken := photosExchange(t, base, code, clientID, clientSecret, redirectURI)

	// ===== Step 1: POST /v1/uploads → uploadToken =====

	// The uploads endpoint takes raw octet-stream (not JSON).
	uploadReq, _ := http.NewRequest("POST", base+"/v1/uploads", bytes.NewReader([]byte("fake-binary-photo-data")))
	uploadReq.Header.Set("Content-Type", "application/octet-stream")
	uploadReq.Header.Set("Authorization", "Bearer "+accessToken)
	uploadReq.Header.Set("X-Goog-Upload-Protocol", "raw")
	uploadResp, err := http.DefaultClient.Do(uploadReq)
	if err != nil {
		t.Fatal(err)
	}
	uploadBody, _ := io.ReadAll(uploadResp.Body)
	uploadResp.Body.Close()
	if uploadResp.StatusCode != 200 {
		t.Fatalf("POST /v1/uploads -> status %d, want 200; body %s", uploadResp.StatusCode, uploadBody)
	}
	uploadToken := strings.TrimSpace(string(uploadBody))
	if uploadToken == "" {
		t.Fatalf("uploadToken is empty; body %q", uploadBody)
	}

	// ===== Step 2: POST /v1/mediaItems:batchCreate → mediaItem =====

	batchBody := map[string]any{
		"albumId": "",
		"newMediaItems": []any{
			map[string]any{
				"description": "My vacation photo",
				"simpleMediaItem": map[string]any{
					"uploadToken": uploadToken,
					"fileName":    "vacation.jpg",
				},
			},
		},
	}
	body, status := photosPostJSONAuth(t, base+"/v1/mediaItems:batchCreate", accessToken, batchBody)
	if status != 200 {
		t.Fatalf("batchCreate -> status %d, want 200; body %s", status, body)
	}
	var batchResp map[string]any
	if err := json.Unmarshal([]byte(body), &batchResp); err != nil {
		t.Fatalf("unmarshal batchCreate response: %v (body %s)", err, body)
	}
	results, ok := batchResp["newMediaItemResults"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("newMediaItemResults = %v, want list of 1", batchResp["newMediaItemResults"])
	}
	result := results[0].(map[string]any)
	mediaItem, ok := result["mediaItem"].(map[string]any)
	if !ok {
		t.Fatalf("mediaItem = %v, want dict", result["mediaItem"])
	}
	mediaID, ok := mediaItem["id"].(string)
	if !ok || !strings.HasPrefix(mediaID, "mock-media-") {
		t.Fatalf("mediaItem.id = %v, want mock-media-* prefix", mediaItem["id"])
	}
	if mediaItem["mimeType"] != "image/jpeg" {
		t.Fatalf("mimeType = %v, want image/jpeg", mediaItem["mimeType"])
	}
	if mediaItem["filename"] != "vacation.jpg" {
		t.Fatalf("filename = %v, want vacation.jpg", mediaItem["filename"])
	}
	mediaMeta, ok := mediaItem["mediaMetadata"].(map[string]any)
	if !ok || mediaMeta["creationTime"] == nil {
		t.Fatalf("mediaMetadata = %v, want dict with creationTime", mediaItem["mediaMetadata"])
	}

	// ===== STATEFUL: POST /v1/mediaItems:search shows the created item =====

	searchBody := map[string]any{
		"pageSize": 25,
	}
	body, status = photosPostJSONAuth(t, base+"/v1/mediaItems:search", accessToken, searchBody)
	if status != 200 {
		t.Fatalf("search -> status %d, want 200; body %s", status, body)
	}
	var searchResp map[string]any
	if err := json.Unmarshal([]byte(body), &searchResp); err != nil {
		t.Fatalf("unmarshal search response: %v (body %s)", err, body)
	}
	foundItems, ok := searchResp["mediaItems"].([]any)
	if !ok {
		t.Fatalf("search mediaItems = %v, want list", searchResp["mediaItems"])
	}
	if len(foundItems) != 1 {
		t.Fatalf("search found %d items, want 1 (the just-created item)", len(foundItems))
	}
	foundItem := foundItems[0].(map[string]any)
	if foundItem["id"] != mediaID {
		t.Fatalf("search item id = %v, want %v", foundItem["id"], mediaID)
	}

	// ===== GET /v1/mediaItems lists the item =====

	body, status = photosGetAuth(t, base+"/v1/mediaItems", accessToken)
	if status != 200 {
		t.Fatalf("GET mediaItems -> status %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal list response: %v (body %s)", err, body)
	}
	listItems, ok := listResp["mediaItems"].([]any)
	if !ok || len(listItems) != 1 {
		t.Fatalf("list mediaItems count = %d, want 1", len(listItems))
	}

	// ===== Albums: create → get → list =====

	// POST /v1/albums → create album
	albumBody := map[string]any{
		"album": map[string]any{
			"title": "Summer 2024",
		},
	}
	body, status = photosPostJSONAuth(t, base+"/v1/albums", accessToken, albumBody)
	if status != 200 {
		t.Fatalf("create album -> status %d, want 200; body %s", status, body)
	}
	var createdAlbum map[string]any
	if err := json.Unmarshal([]byte(body), &createdAlbum); err != nil {
		t.Fatalf("unmarshal create album: %v (body %s)", err, body)
	}
	albumID, ok := createdAlbum["id"].(string)
	if !ok || !strings.HasPrefix(albumID, "mock-album-") {
		t.Fatalf("album id = %v, want mock-album-* prefix", createdAlbum["id"])
	}
	if createdAlbum["title"] != "Summer 2024" {
		t.Fatalf("album title = %v, want Summer 2024", createdAlbum["title"])
	}

	// GET /v1/albums/{id} → album details
	body, status = photosGetAuth(t, base+"/v1/albums/"+albumID, accessToken)
	if status != 200 {
		t.Fatalf("get album -> status %d, want 200; body %s", status, body)
	}
	var albumDetail map[string]any
	if err := json.Unmarshal([]byte(body), &albumDetail); err != nil {
		t.Fatalf("unmarshal get album: %v (body %s)", err, body)
	}
	if albumDetail["id"] != albumID {
		t.Fatalf("album detail id = %v, want %v", albumDetail["id"], albumID)
	}

	// GET /v1/albums → list albums shows it
	body, status = photosGetAuth(t, base+"/v1/albums", accessToken)
	if status != 200 {
		t.Fatalf("list albums -> status %d, want 200; body %s", status, body)
	}
	var albumsList map[string]any
	if err := json.Unmarshal([]byte(body), &albumsList); err != nil {
		t.Fatalf("unmarshal list albums: %v (body %s)", err, body)
	}
	albums, ok := albumsList["albums"].([]any)
	if !ok || len(albums) != 1 {
		t.Fatalf("albums list count = %d, want 1", len(albums))
	}

	// ===== batchCreate with invalid token → error status =====

	badBatchBody := map[string]any{
		"newMediaItems": []any{
			map[string]any{
				"simpleMediaItem": map[string]any{
					"uploadToken": "invalid-token",
					"fileName":    "bad.jpg",
				},
			},
		},
	}
	body, status = photosPostJSONAuth(t, base+"/v1/mediaItems:batchCreate", accessToken, badBatchBody)
	if status != 200 {
		t.Fatalf("batchCreate bad token -> status %d, want 200; body %s", status, body)
	}
	var badBatchResp map[string]any
	if err := json.Unmarshal([]byte(body), &badBatchResp); err != nil {
		t.Fatalf("unmarshal bad batchCreate: %v (body %s)", err, body)
	}
	badResults := badBatchResp["newMediaItemResults"].([]any)
	badResult := badResults[0].(map[string]any)
	if _, ok := badResult["status"].(map[string]any); !ok {
		t.Fatalf("expected status error for invalid token, got %v", badResult)
	}

	// ===== No auth → 401 =====

	uploadReq2, _ := http.NewRequest("POST", base+"/v1/uploads", bytes.NewReader([]byte("data")))
	uploadReq2.Header.Set("Content-Type", "application/octet-stream")
	uploadResp2, err := http.DefaultClient.Do(uploadReq2)
	if err != nil {
		t.Fatal(err)
	}
	if uploadResp2.StatusCode != 401 {
		t.Fatalf("POST /v1/uploads no auth -> status %d, want 401", uploadResp2.StatusCode)
	}
	uploadResp2.Body.Close()
}

// === Helpers ===

func photosAuthorize(t *testing.T, base, redirectURI, state, clientID string) string {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(base + "/o/oauth2/auth?client_id=" + clientID +
		"&redirect_uri=" + url.QueryEscape(redirectURI) +
		"&state=" + state + "&response_type=code")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 302 {
		t.Fatalf("authorize -> status %d, want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	u, _ := url.Parse(loc)
	code := u.Query().Get("code")
	if code == "" {
		t.Fatal("authorize: no code in redirect")
	}
	return code
}

func photosExchange(t *testing.T, base, code, clientID, clientSecret, redirectURI string) string {
	t.Helper()
	resp, err := http.PostForm(base+"/o/oauth2/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("exchange -> status %d, body %s", resp.StatusCode, b)
	}
	var tokens map[string]any
	if err := json.Unmarshal(b, &tokens); err != nil {
		t.Fatalf("unmarshal tokens: %v (body %s)", err, b)
	}
	access, ok := tokens["access_token"].(string)
	if !ok {
		t.Fatalf("access_token = %v, want string", tokens["access_token"])
	}
	return access
}

func photosPostJSONAuth(t *testing.T, urlStr, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", urlStr, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func photosGetAuth(t *testing.T, urlStr, token string) (string, int) {
	t.Helper()
	return getAuth(t, urlStr, token)
}
