package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// setupProductHunt serves the committed producthunt-style adapter and returns
// its HTTP base URL + cleanup.
func setupProductHunt(t *testing.T) (string, func()) {
	t.Helper()

	absDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "producthunt-style"))
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"ph": {Adapter: absDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		e.Close()
		t.Fatalf("ServeForTest: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	url := addrs["ph"]
	cleanup := func() {
		cancel()
		e.Close()
	}
	return url, cleanup
}

// phGraphql sends a GraphQL POST (query + variables) to the adapter's
// endpoint and returns the decoded response + status.
func phGraphql(t *testing.T, base, query string, variables map[string]any) (map[string]any, int) {
	t.Helper()
	body := map[string]any{"query": query}
	if variables != nil {
		body["variables"] = variables
	}
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", base+"/v2/api/graphql.json", strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var result map[string]any
	json.Unmarshal(b, &result)
	return result, resp.StatusCode
}

// phGraphqlErrors returns the response errors[] joined for display, or "".
func phGraphqlErrors(result map[string]any) string {
	if result == nil {
		return "<nil response>"
	}
	if errs, ok := result["errors"]; ok && errs != nil {
		if arr, ok := errs.([]any); ok && len(arr) > 0 {
			var msgs []string
			for _, e := range arr {
				if m, ok := e.(map[string]any); ok {
					if msg, ok := m["message"].(string); ok {
						msgs = append(msgs, msg)
					}
				}
			}
			if len(msgs) > 0 {
				return strings.Join(msgs, "; ")
			}
		}
		return "<non-empty errors>"
	}
	return ""
}

// TestProductHuntStylePostCreateAndQuery exercises the reference client's
// exact postCreate mutation (variables, input object) followed by a post(id)
// query with arguments + variables + aliases — all executed by the real
// GraphQL executor against the posts collection.
func TestProductHuntStylePostCreateAndQuery(t *testing.T) {
	base, cleanup := setupProductHunt(t)
	defer cleanup()

	// The exact GraphQL mutation the reference client adapter sends.
	query := `mutation Create($name: String!, $tagline: String!, $description: String!, $url: String!) {
		postCreate(input: { name: $name, tagline: $tagline, description: $description, url: $url }) {
			post { id }
			errors { message }
		}
	}`
	vars := map[string]any{
		"name":        "Stunt",
		"tagline":     "Local API testing",
		"description": "Synthetic adapters for local dev.",
		"url":         "https://example.test/stunt",
	}
	result, status := phGraphql(t, base, query, vars)
	if status != 200 {
		t.Fatalf("postCreate -> %d, want 200", status)
	}
	if errs := phGraphqlErrors(result); errs != "" {
		t.Fatalf("postCreate errors: %s", errs)
	}

	data := result["data"].(map[string]any)
	pc := data["postCreate"].(map[string]any)
	if pcErrs := pc["errors"].([]any); len(pcErrs) != 0 {
		t.Fatalf("postCreate payload errors = %v, want []", pcErrs)
	}
	post := pc["post"].(map[string]any)
	id, ok := post["id"].(string)
	if !ok || id == "" {
		t.Fatalf("post.id = %v, want non-empty string", post["id"])
	}

	// post(id) with variables + aliases against the persisted post.
	readQuery := `query($id: ID!) {
		found: post(id: $id) { id name tagline votesCount url createdAt user { username } }
		missing: post(id: "nope") { id }
	}`
	result, status = phGraphql(t, base, readQuery, map[string]any{"id": id})
	if status != 200 {
		t.Fatalf("post query -> %d, want 200", status)
	}
	if errs := phGraphqlErrors(result); errs != "" {
		t.Fatalf("post query errors: %s", errs)
	}
	data = result["data"].(map[string]any)
	found := data["found"].(map[string]any)
	if found["name"] != "Stunt" {
		t.Fatalf("found.name = %v, want Stunt", found["name"])
	}
	if found["votesCount"] != float64(0) {
		t.Fatalf("found.votesCount = %v (%T), want Int 0", found["votesCount"], found["votesCount"])
	}
	if found["createdAt"] == nil || found["createdAt"] == "" {
		t.Fatalf("found.createdAt = %v, want the stored ISO-8601 timestamp", found["createdAt"])
	}
	user := found["user"].(map[string]any)
	if user["username"] != "stuntmaker" {
		t.Fatalf("user.username = %v, want stuntmaker", user["username"])
	}
	if miss := data["missing"]; miss != nil {
		t.Fatalf("post(id: nope) = %v, want null", miss)
	}
}

// TestProductHuntStylePostsConnection verifies the Relay-style posts
// connection: ordering (NEWEST = newest first), first/after offset cursors,
// totalCount, and pageInfo.
func TestProductHuntStylePostsConnection(t *testing.T) {
	base, cleanup := setupProductHunt(t)
	defer cleanup()

	// Create three posts in order; ids 1..3.
	mut := `mutation($name: String!) {
		postCreate(input: { name: $name, tagline: "t", description: "d", url: "https://example.test/a" }) {
			post { id }
		}
	}`
	ids := []string{}
	for _, name := range []string{"first", "second", "third"} {
		result, status := phGraphql(t, base, mut, map[string]any{"name": name})
		if status != 200 {
			t.Fatalf("postCreate(%s) -> %d, want 200", name, status)
		}
		ids = append(ids, result["data"].(map[string]any)["postCreate"].(map[string]any)["post"].(map[string]any)["id"].(string))
	}

	result, status := phGraphql(t, base,
		`{ posts(first: 2) { edges { cursor node { id name } } pageInfo { hasNextPage endCursor } totalCount } }`, nil)
	if status != 200 {
		t.Fatalf("posts -> %d, want 200", status)
	}
	posts := result["data"].(map[string]any)["posts"].(map[string]any)
	if posts["totalCount"] != float64(3) {
		t.Fatalf("totalCount = %v, want 3", posts["totalCount"])
	}
	edges := posts["edges"].([]any)
	if len(edges) != 2 {
		t.Fatalf("edges length = %d, want 2 (first: 2)", len(edges))
	}
	// NEWEST: the third post comes first.
	if got := edges[0].(map[string]any)["node"].(map[string]any)["name"]; got != "third" {
		t.Fatalf("edges[0].name = %v, want third (NEWEST ordering)", got)
	}
	pageInfo := posts["pageInfo"].(map[string]any)
	if pageInfo["hasNextPage"] != true {
		t.Fatalf("hasNextPage = %v, want true", pageInfo["hasNextPage"])
	}
	endCursor := pageInfo["endCursor"].(string)

	// Follow the cursor for the remaining page.
	result, _ = phGraphql(t, base,
		`query($after: String) { posts(first: 2, after: $after) { nodes { name } pageInfo { hasNextPage } } }`,
		map[string]any{"after": endCursor})
	posts = result["data"].(map[string]any)["posts"].(map[string]any)
	nodes := posts["nodes"].([]any)
	if len(nodes) != 1 || nodes[0].(map[string]any)["name"] != "first" {
		t.Fatalf("after=%s nodes = %v, want just 'first'", endCursor, nodes)
	}
	if posts["pageInfo"].(map[string]any)["hasNextPage"] != false {
		t.Fatalf("second page hasNextPage = true, want false")
	}
}

// TestProductHuntStyleValidationErrors verifies server-side validation is
// reported as per-field errors inside the postCreate payload (post stays
// null, no top-level GraphQL errors) — Product Hunt's mutation convention.
func TestProductHuntStyleValidationErrors(t *testing.T) {
	base, cleanup := setupProductHunt(t)
	defer cleanup()

	result, status := phGraphql(t, base,
		`mutation { postCreate(input: { name: "Only name" }) { post { id } errors { message attribute } } }`, nil)
	if status != 200 {
		t.Fatalf("invalid postCreate -> %d, want 200 (payload errors, not HTTP errors)", status)
	}
	if errs := phGraphqlErrors(result); errs != "" {
		t.Fatalf("unexpected top-level errors: %s", errs)
	}
	pc := result["data"].(map[string]any)["postCreate"].(map[string]any)
	if pc["post"] != nil {
		t.Fatalf("post = %v, want null on validation failure", pc["post"])
	}
	errs := pc["errors"].([]any)
	if len(errs) != 3 {
		t.Fatalf("errors = %v, want 3 (tagline, description, url blank)", errs)
	}
	attrs := map[string]bool{}
	for _, e := range errs {
		em := e.(map[string]any)
		if msg, _ := em["message"].(string); !strings.Contains(msg, "can't be blank") {
			t.Fatalf("error message = %v, want \"can't be blank\"", em["message"])
		}
		if attr, ok := em["attribute"].(string); ok {
			attrs[attr] = true
		}
	}
	for _, want := range []string{"tagline", "description", "url"} {
		if !attrs[want] {
			t.Errorf("expected a %s error, got attrs %v", want, attrs)
		}
	}

	// Malformed URL is a per-field error too.
	result, _ = phGraphql(t, base,
		`mutation { postCreate(input: { name: "n", tagline: "t", description: "d", url: "not-a-url" }) { errors { message attribute } } }`, nil)
	pc = result["data"].(map[string]any)["postCreate"].(map[string]any)
	errs = pc["errors"].([]any)
	if len(errs) != 1 || !strings.Contains(errs[0].(map[string]any)["message"].(string), "Url is invalid") {
		t.Fatalf("bad-url errors = %v, want one 'Url is invalid'", errs)
	}
}

// TestProductHuntStyleUnknownFieldErrors verifies REAL GraphQL validation:
// unknown fields, unknown operations, and bad variables produce errors[]
// with a non-200 status — the old matcher returned {"data": {}} silently.
func TestProductHuntStyleUnknownFieldErrors(t *testing.T) {
	base, cleanup := setupProductHunt(t)
	defer cleanup()

	result, status := phGraphql(t, base, `{ post(id: "1") { votes } }`, nil)
	if status != 400 {
		t.Fatalf("unknown Post field -> %d, want 400", status)
	}
	if errs := phGraphqlErrors(result); !strings.Contains(errs, "votes") {
		t.Fatalf("unknown Post field errors = %q, want a message naming votes", errs)
	}

	result, status = phGraphql(t, base, `{ comments(first: 5) { id } }`, nil)
	if status != 400 {
		t.Fatalf("unknown root field -> %d, want 400", status)
	}

	// Type-mismatched variable value is rejected at variable coercion
	// (200 + errors[] per the GraphQL over HTTP convention).
	result, status = phGraphql(t, base, `query($n: Int!) { posts(first: $n) { totalCount } }`, map[string]any{"n": "two"})
	if status != 200 {
		t.Fatalf("variable coercion failure -> %d, want 200 with errors[]", status)
	}
	if errs := phGraphqlErrors(result); errs == "" {
		t.Fatalf("type-mismatched variable: expected errors[], got %v", result)
	}

	// Catch-all 404 for non-GraphQL routes is intact.
	resp, err := http.Get(base + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("GET unmatched -> %d, want 404", resp.StatusCode)
	}
}
