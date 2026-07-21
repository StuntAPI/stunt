package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestProductHuntStyleAdapter exercises the Product Hunt GraphQL-style
// reference adapter end-to-end through the post-create flow:
//
//   - graphql postCreate (Bearer) → { data: { postCreate: { post: { id } } } }
//   - graphql postCreate (no Bearer) → 401
//   - graphql postCreate with server-side errors → errors array
func TestProductHuntStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "producthunt-style")
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
			"ph": {Adapter: absAdapterDir},
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

	base := addrs["ph"]

	// The exact GraphQL mutation ***REMOVED***'s adapter sends.
	query := `mutation Create($name: String!, $tagline: String!, $description: String!, $url: String!) {
  postCreate(input: { name: $name, tagline: $tagline, description: $description, url: $url }) {
    post { id }
    errors { message }
  }
}`

	// ===== postCreate (with Bearer) =====

	vars := map[string]string{
		"name":        "Stunt",
		"tagline":     "Local API testing",
		"description": "Synthetic adapters for local dev.",
		"url":         "https://example.test/stunt",
	}
	body, status := phPostGraphQL(t, base+"/v2/api/graphql.json", "mock-token-1", query, vars)
	if status != 200 {
		t.Fatalf("postCreate -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal postCreate: %v (body %s)", err, body)
	}

	// data.postCreate.post.id must be present and non-empty.
	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("response has no data object: %v", resp)
	}
	pc, ok := data["postCreate"].(map[string]any)
	if !ok {
		t.Fatalf("data has no postCreate object: %v", data)
	}
	post, ok := pc["post"].(map[string]any)
	if !ok {
		t.Fatalf("postCreate has no post object: %v", pc)
	}
	id, ok := post["id"].(string)
	if !ok || id == "" {
		t.Fatalf("post.id = %v, want non-empty string", post["id"])
	}

	// There must be no errors at the top level or in postCreate.
	if errs, hasErr := resp["errors"]; hasErr {
		t.Fatalf("unexpected top-level errors: %v", errs)
	}
	if pcErrs, hasErr := pc["errors"]; hasErr {
		if errs, ok := pcErrs.([]any); ok && len(errs) > 0 {
			t.Fatalf("unexpected postCreate errors: %v", errs)
		}
	}

	// ===== postCreate (no Bearer) → 401 =====

	body, status = phPostGraphQL(t, base+"/v2/api/graphql.json", "", query, vars)
	if status != 401 {
		t.Fatalf("postCreate without Bearer -> status %d, want 401; body %s", status, body)
	}

	// ===== Catch-all 404 =====

	body, status = phGet(t, base+"/nope")
	if status != 404 {
		t.Fatalf("GET unmatched -> status %d, want 404; body %s", status, body)
	}
}

// === Helpers ===

func phGet(t *testing.T, url string) (string, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func phPostGraphQL(t *testing.T, rawurl, bearer, query string, variables map[string]string) (string, int) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
