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

// blogAdapterDir is the committed blog-style GraphQL adapter directory,
// relative to the engine package. Using the real adapter verifies the
// checked-in schema/resolvers/fixtures are correct end to end.
const blogAdapterDir = "../../adapters/blog-style"

// setupBlogGraphqlEngine loads the committed blog-style adapter, serves it
// on a free port, and returns the HTTP base URL + cleanup.
func setupBlogGraphqlEngine(t *testing.T) (string, func()) {
	t.Helper()

	absDir, err := filepath.Abs(blogAdapterDir)
	if err != nil {
		t.Fatalf("resolve adapter dir: %v", err)
	}

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"blog": {Adapter: absDir},
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

	url := addrs["blog"]
	cleanup := func() {
		cancel()
		e.Close()
	}
	return url, cleanup
}

// blogGqlPost sends a GraphQL POST request and returns the decoded result + status.
func blogGqlPost(t *testing.T, url, query string, variables map[string]any) (map[string]any, int) {
	t.Helper()
	body := map[string]any{"query": query}
	if variables != nil {
		body["variables"] = variables
	}
	bodyBytes, _ := json.Marshal(body)
	resp, err := http.Post(url+"/graphql", "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("POST /graphql: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var result map[string]any
	json.Unmarshal(data, &result)
	return result, resp.StatusCode
}

// TestBlogGraphqlNestedQuery verifies that a deeply nested query resolves
// relations across three collections: user → posts → comments.
func TestBlogGraphqlNestedQuery(t *testing.T) {
	url, cleanup := setupBlogGraphqlEngine(t)
	defer cleanup()

	query := `{
		user(id: "u1") {
			name
			posts {
				id
				title
				status
				comments {
					id
					author
					body
				}
			}
		}
	}`

	result, status := blogGqlPost(t, url, query, nil)
	if status != 200 {
		t.Fatalf("status = %d, want 200; body %v", status, result)
	}
	if errs, ok := result["errors"]; ok && errs != nil {
		t.Fatalf("unexpected errors: %v", errs)
	}

	data := result["data"].(map[string]any)
	user := data["user"].(map[string]any)
	if user["name"] != "Alice Synth" {
		t.Errorf("name = %v, want Alice Synth", user["name"])
	}

	posts := user["posts"].([]any)
	if len(posts) != 2 {
		t.Fatalf("expected 2 posts for u1, got %d", len(posts))
	}

	// Find the published post (p1) and check it has 2 comments.
	var p1 map[string]any
	for _, p := range posts {
		pm := p.(map[string]any)
		if pm["id"] == "p1" {
			p1 = pm
		}
	}
	if p1 == nil {
		t.Fatal("expected to find post p1")
	}
	if p1["status"] != "PUBLISHED" {
		t.Errorf("p1 status = %v, want PUBLISHED", p1["status"])
	}
	comments := p1["comments"].([]any)
	if len(comments) != 2 {
		t.Fatalf("expected 2 comments on p1, got %d", len(comments))
	}
	c0 := comments[0].(map[string]any)
	if c0["author"] != "Bob Synth" {
		t.Errorf("comment[0] author = %v, want Bob Synth", c0["author"])
	}
}

// TestBlogGraphqlPostAuthorRelation verifies that Post.author resolves to
// the correct User via the user_id foreign key.
func TestBlogGraphqlPostAuthorRelation(t *testing.T) {
	url, cleanup := setupBlogGraphqlEngine(t)
	defer cleanup()

	query := `{
		post(id: "p3") {
			title
			author {
				id
				name
			}
		}
	}`

	result, status := blogGqlPost(t, url, query, nil)
	if status != 200 {
		t.Fatalf("status = %d; body %v", status, result)
	}
	data := result["data"].(map[string]any)
	post := data["post"].(map[string]any)
	author := post["author"].(map[string]any)
	if author["id"] != "u2" {
		t.Errorf("author id = %v, want u2", author["id"])
	}
	if author["name"] != "Bob Synth" {
		t.Errorf("author name = %v, want Bob Synth", author["name"])
	}
}

// TestBlogGraphqlEnumRoundTrip verifies that a PostStatus enum value
// round-trips through a query and through the posts(status:) argument.
func TestBlogGraphqlEnumRoundTrip(t *testing.T) {
	url, cleanup := setupBlogGraphqlEngine(t)
	defer cleanup()

	// Query posts with status: PUBLISHED — should filter to 2 posts.
	queryWithVar := `query($status: PostStatus!) {
		posts(status: $status) {
			id
			status
		}
	}`
	result, status := blogGqlPost(t, url, queryWithVar, map[string]any{"status": "PUBLISHED"})
	if status != 200 {
		t.Fatalf("status = %d; body %v", status, result)
	}
	data := result["data"].(map[string]any)
	posts := data["posts"].([]any)
	if len(posts) != 2 {
		t.Fatalf("expected 2 PUBLISHED posts, got %d", len(posts))
	}
	for _, p := range posts {
		if p.(map[string]any)["status"] != "PUBLISHED" {
			t.Errorf("expected PUBLISHED, got %v", p.(map[string]any)["status"])
		}
	}

	// Query without filter — should return all 3.
	resultAll, _ := blogGqlPost(t, url, `{ posts { id } }`, nil)
	allPosts := resultAll["data"].(map[string]any)["posts"].([]any)
	if len(allPosts) != 3 {
		t.Fatalf("expected 3 total posts, got %d", len(allPosts))
	}
}

// TestBlogGraphqlMutationCreateUser verifies that createUser persists to the
// collection and a subsequent query sees the new user.
func TestBlogGraphqlMutationCreateUser(t *testing.T) {
	url, cleanup := setupBlogGraphqlEngine(t)
	defer cleanup()

	// Create a user.
	mutQuery := `mutation {
		createUser(name: "Dave Synth", bio: "New user from mutation test") {
			id
			name
			bio
		}
	}`
	result, status := blogGqlPost(t, url, mutQuery, nil)
	if status != 200 {
		t.Fatalf("status = %d; body %v", status, result)
	}
	data := result["data"].(map[string]any)
	created := data["createUser"].(map[string]any)
	if created["name"] != "Dave Synth" {
		t.Errorf("name = %v, want Dave Synth", created["name"])
	}
	createdID := created["id"].(string)
	if createdID == "" {
		t.Fatal("expected non-empty created user id")
	}

	// Query for the new user by id.
	queryResult, _ := blogGqlPost(t, url, `query($id: ID!) { user(id: $id) { id name bio } }`, map[string]any{"id": createdID})
	queryData := queryResult["data"].(map[string]any)
	fetched := queryData["user"].(map[string]any)
	if fetched["name"] != "Dave Synth" {
		t.Errorf("fetched name = %v, want Dave Synth", fetched["name"])
	}
	if fetched["bio"] != "New user from mutation test" {
		t.Errorf("fetched bio = %v", fetched["bio"])
	}
}

// TestBlogGraphqlMutationAddComment verifies that addComment creates a
// comment visible via a subsequent nested query.
func TestBlogGraphqlMutationAddComment(t *testing.T) {
	url, cleanup := setupBlogGraphqlEngine(t)
	defer cleanup()

	// Add a comment to post p1 (which initially has 2 comments).
	mutQuery := `mutation {
		addComment(postId: "p1", author: "Eve Synth", body: "Added via mutation!") {
			id
			author
			body
		}
	}`
	result, status := blogGqlPost(t, url, mutQuery, nil)
	if status != 200 {
		t.Fatalf("status = %d; body %v", status, result)
	}
	created := result["data"].(map[string]any)["addComment"].(map[string]any)
	if created["author"] != "Eve Synth" {
		t.Errorf("author = %v, want Eve Synth", created["author"])
	}

	// Query post p1 and verify it now has 3 comments.
	queryResult, _ := blogGqlPost(t, url, `{ post(id: "p1") { comments { author body } } }`, nil)
	post := queryResult["data"].(map[string]any)["post"].(map[string]any)
	comments := post["comments"].([]any)
	if len(comments) != 3 {
		t.Fatalf("expected 3 comments on p1 after addComment, got %d", len(comments))
	}
}

// TestBlogGraphqlTypename verifies that __typename works on object types.
func TestBlogGraphqlTypename(t *testing.T) {
	url, cleanup := setupBlogGraphqlEngine(t)
	defer cleanup()

	result, _ := blogGqlPost(t, url, `{ user(id: "u1") { __typename posts { __typename comments { __typename } } } }`, nil)
	data := result["data"].(map[string]any)
	user := data["user"].(map[string]any)
	if user["__typename"] != "User" {
		t.Errorf("__typename = %v, want User", user["__typename"])
	}
	posts := user["posts"].([]any)
	post := posts[0].(map[string]any)
	if post["__typename"] != "Post" {
		t.Errorf("__typename = %v, want Post", post["__typename"])
	}
	comments := post["comments"].([]any)
	comment := comments[0].(map[string]any)
	if comment["__typename"] != "Comment" {
		t.Errorf("__typename = %v, want Comment", comment["__typename"])
	}
}

// TestBlogGraphqlIntrospection verifies that __schema introspection works.
func TestBlogGraphqlIntrospection(t *testing.T) {
	url, cleanup := setupBlogGraphqlEngine(t)
	defer cleanup()

	result, status := blogGqlPost(t, url, `{ __schema { queryType { name } types { name } } }`, nil)
	if status != 200 {
		t.Fatalf("status = %d; body %v", status, result)
	}
	data := result["data"].(map[string]any)
	schema := data["__schema"].(map[string]any)
	if schema["queryType"].(map[string]any)["name"] != "Query" {
		t.Errorf("queryType name = %v, want Query", schema["queryType"])
	}
	types := schema["types"].([]any)
	// Verify our domain types are present.
	typeNames := make(map[string]bool)
	for _, t := range types {
		typeNames[t.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"User", "Post", "Comment", "PostStatus"} {
		if !typeNames[want] {
			t.Errorf("expected type %s in __schema types", want)
		}
	}
}

// TestBlogGraphqlVariables verifies that query variables are passed through
// to resolvers as args.
func TestBlogGraphqlVariables(t *testing.T) {
	url, cleanup := setupBlogGraphqlEngine(t)
	defer cleanup()

	result, _ := blogGqlPost(t, url,
		`query($id: ID!) { user(id: $id) { name } }`,
		map[string]any{"id": "u2"})
	data := result["data"].(map[string]any)
	user := data["user"].(map[string]any)
	if user["name"] != "Bob Synth" {
		t.Errorf("name = %v, want Bob Synth", user["name"])
	}
}

// TestBlogGraphqlUsersList verifies the list root field.
func TestBlogGraphqlUsersList(t *testing.T) {
	url, cleanup := setupBlogGraphqlEngine(t)
	defer cleanup()

	result, _ := blogGqlPost(t, url, `{ users { id name } }`, nil)
	data := result["data"].(map[string]any)
	users := data["users"].([]any)
	if len(users) != 3 {
		t.Fatalf("expected 3 seeded users, got %d", len(users))
	}
}
