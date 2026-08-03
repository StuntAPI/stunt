package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// bootPhotosService boots the photos-style reference adapter on a free port
// and returns its base URL.
func bootPhotosService(t *testing.T) string {
	t.Helper()

	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "photos-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"photos": {Adapter: adapterDir},
		},
	}
	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	t.Cleanup(cancel)
	time.Sleep(50 * time.Millisecond)
	return addrs["photos"]
}

// photosUpload POSTs raw bytes to /v1/uploads and returns the uploadToken.
func photosUpload(t *testing.T, base, token string, payload []byte, contentType string) string {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/v1/uploads", bytes.NewReader(payload))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Goog-Upload-Protocol", "raw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("POST /v1/uploads -> status %d, want 200; body %s", resp.StatusCode, body)
	}
	uploadToken := strings.TrimSpace(string(body))
	if uploadToken == "" {
		t.Fatal("empty uploadToken")
	}
	return uploadToken
}

// photosBatchCreateOne creates one media item from an uploadToken and returns
// the created mediaItem object.
func photosBatchCreateOne(t *testing.T, base, token, uploadToken, fileName string) map[string]any {
	t.Helper()
	body, status := photosPostJSONAuth(t, base+"/v1/mediaItems:batchCreate", token, map[string]any{
		"newMediaItems": []any{
			map[string]any{
				"simpleMediaItem": map[string]any{
					"uploadToken": uploadToken,
					"fileName":    fileName,
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("batchCreate -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal batchCreate: %v", err)
	}
	results, ok := resp["newMediaItemResults"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("newMediaItemResults = %v, want 1 result", resp["newMediaItemResults"])
	}
	item, ok := results[0].(map[string]any)["mediaItem"].(map[string]any)
	if !ok {
		t.Fatalf("mediaItem missing in %v", results[0])
	}
	return item
}

// getRaw GETs a URL without auth and returns status, body, and content type.
func getRaw(t *testing.T, url string) (int, []byte, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, resp.Header.Get("Content-Type")
}

// TestPhotosMediaPlane proves the media plane is real: uploaded bytes are
// stored, linked by batchCreate, addressable through a read-time baseUrl
// derived from the request host, and served back byte-exact only when the
// Google download suffix (=d / =dv) is present.
func TestPhotosMediaPlane(t *testing.T) {
	base := bootPhotosService(t)
	host := strings.TrimPrefix(base, "http://")

	code := photosAuthorize(t, base, "http://localhost:8080/callback", "state-mp", "photos-test-client-id")
	accessToken := photosExchange(t, base, code, "photos-test-client-id", "photos-test-client-secret", "http://localhost:8080/callback")

	// Byte-fidelity fixture: PNG magic + all 256 byte values.
	original := allByteValues(1024)

	uploadToken := photosUpload(t, base, accessToken, original, "image/png")
	item := photosBatchCreateOne(t, base, accessToken, uploadToken, "fixture.png")
	mediaID, _ := item["id"].(string)
	if mediaID == "" {
		t.Fatal("created mediaItem has no id")
	}

	// baseUrl must be computed from the request host at read time.
	wantBaseURL := "http://" + host + "/v1/media-dl/" + mediaID
	if item["baseUrl"] != wantBaseURL {
		t.Fatalf("batchCreate baseUrl = %v, want %s", item["baseUrl"], wantBaseURL)
	}

	// ===== GET /v1/mediaItems/{id} returns the item with read-time baseUrl =====

	body, status := photosGetAuth(t, base+"/v1/mediaItems/"+mediaID, accessToken)
	if status != 200 {
		t.Fatalf("GET mediaItems/{id} -> status %d, want 200; body %s", status, body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal item: %v", err)
	}
	if got["id"] != mediaID {
		t.Fatalf("item id = %v, want %s", got["id"], mediaID)
	}
	if got["baseUrl"] != wantBaseURL {
		t.Fatalf("get baseUrl = %v, want %s", got["baseUrl"], wantBaseURL)
	}

	// Unknown item id -> 404.
	_, status = photosGetAuth(t, base+"/v1/mediaItems/mock-media-does-not-exist", accessToken)
	if status != 404 {
		t.Fatalf("GET unknown mediaItem -> status %d, want 404", status)
	}

	// ===== media-dl strict =d semantics =====

	// =d serves the ORIGINAL bytes exactly.
	status, dlBody, ctype := getRaw(t, wantBaseURL+"=d")
	if status != 200 {
		t.Fatalf("GET baseUrl=d -> status %d, want 200", status)
	}
	if !bytes.Equal(dlBody, original) {
		t.Fatalf("=d download is not byte-equal to the upload: got %d bytes, want %d", len(dlBody), len(original))
	}
	if ctype != "image/png" {
		t.Fatalf("=d content type = %q, want the recorded image/png", ctype)
	}

	// =dv also serves the original bytes (video suffix).
	status, dvBody, _ := getRaw(t, wantBaseURL+"=dv")
	if status != 200 {
		t.Fatalf("GET baseUrl=dv -> status %d, want 200", status)
	}
	if !bytes.Equal(dvBody, original) {
		t.Fatal("=dv download is not byte-equal to the upload")
	}

	// A bare baseUrl (no suffix) serves a DERIVATIVE: 200 but clearly
	// different bytes, so a client that forgets =d fails byte-comparison.
	status, derivBody, _ := getRaw(t, wantBaseURL)
	if status != 200 {
		t.Fatalf("GET bare baseUrl -> status %d, want 200", status)
	}
	if bytes.Equal(derivBody, original) {
		t.Fatal("bare baseUrl served the original bytes; want a distinct derivative payload")
	}

	// Unknown blob id -> 404, with and without the suffix.
	status, _, _ = getRaw(t, base+"/v1/media-dl/mock-media-unknown=d")
	if status != 404 {
		t.Fatalf("media-dl unknown id =d -> status %d, want 404", status)
	}
	status, _, _ = getRaw(t, base+"/v1/media-dl/mock-media-unknown")
	if status != 404 {
		t.Fatalf("media-dl unknown id -> status %d, want 404", status)
	}
}

// TestPhotosPagination proves list and search honor pageSize/pageToken and
// emit nextPageToken while more items remain.
func TestPhotosPagination(t *testing.T) {
	base := bootPhotosService(t)

	code := photosAuthorize(t, base, "http://localhost:8080/callback", "state-pg", "photos-test-client-id")
	accessToken := photosExchange(t, base, code, "photos-test-client-id", "photos-test-client-secret", "http://localhost:8080/callback")

	const total = 5
	created := map[string]bool{}
	for i := 0; i < total; i++ {
		payload := []byte(fmt.Sprintf("photo-payload-%d-", i))
		tok := photosUpload(t, base, accessToken, payload, "image/jpeg")
		item := photosBatchCreateOne(t, base, accessToken, tok, fmt.Sprintf("p%d.jpg", i))
		id, _ := item["id"].(string)
		created[id] = true
	}

	// ===== GET list pagination: pageSize=2 -> pages of 2, 2, 1 =====

	seen := map[string]bool{}
	pageToken := ""
	pages := 0
	for {
		url := base + "/v1/mediaItems?pageSize=2"
		if pageToken != "" {
			url += "&pageToken=" + pageToken
		}
		body, status := photosGetAuth(t, url, accessToken)
		if status != 200 {
			t.Fatalf("list page -> status %d, want 200; body %s", status, body)
		}
		var resp map[string]any
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("unmarshal list page: %v", err)
		}
		items, _ := resp["mediaItems"].([]any)
		if len(items) == 0 {
			t.Fatalf("list page %d has no items", pages)
		}
		if len(items) > 2 {
			t.Fatalf("list page %d has %d items, want <= pageSize 2", pages, len(items))
		}
		for _, it := range items {
			id, _ := it.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("item %s appeared on two pages", id)
			}
			seen[id] = true
		}
		pages++
		next, _ := resp["nextPageToken"].(string)
		if next == "" {
			break
		}
		pageToken = next
		if pages > total {
			t.Fatal("pagination did not terminate")
		}
	}
	if pages != 3 {
		t.Fatalf("list pagination took %d pages, want 3 (2+2+1)", pages)
	}
	if len(seen) != total {
		t.Fatalf("list pagination returned %d distinct items, want %d", len(seen), total)
	}
	for id := range created {
		if !seen[id] {
			t.Fatalf("created item %s missing from paginated list", id)
		}
	}

	// ===== search pagination (pageSize/pageToken in the JSON body) =====

	seen = map[string]bool{}
	pageToken = ""
	pages = 0
	for {
		searchBody := map[string]any{"pageSize": 3}
		if pageToken != "" {
			searchBody["pageToken"] = pageToken
		}
		body, status := photosPostJSONAuth(t, base+"/v1/mediaItems:search", accessToken, searchBody)
		if status != 200 {
			t.Fatalf("search page -> status %d, want 200; body %s", status, body)
		}
		var resp map[string]any
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("unmarshal search page: %v", err)
		}
		items, _ := resp["mediaItems"].([]any)
		if len(items) > 3 {
			t.Fatalf("search page has %d items, want <= pageSize 3", len(items))
		}
		for _, it := range items {
			id, _ := it.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("search item %s appeared on two pages", id)
			}
			seen[id] = true
		}
		pages++
		next, _ := resp["nextPageToken"].(string)
		if next == "" {
			break
		}
		pageToken = next
		if pages > total {
			t.Fatal("search pagination did not terminate")
		}
	}
	if pages != 2 {
		t.Fatalf("search pagination took %d pages, want 2 (3+2)", pages)
	}
	if len(seen) != total {
		t.Fatalf("search pagination returned %d distinct items, want %d", len(seen), total)
	}
}
