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

	"github.com/stunt-adapters/stunt/internal/manifest"
)

const graphqlTestSDL = `
type Query {
  user(id: ID!): User
  users: [User!]!
}
type Mutation {
  createUser(name: String!): User!
}
type User {
  id: ID!
  name: String!
  role: Role!
  posts: [Post!]!
}
type Post {
  id: ID!
  title: String!
}
enum Role {
  ADMIN
  USER
}
`

const graphqlTestStar = `
def on_user(args):
    uid = args["args"]["id"]
    users = {
        "1": {"id": "1", "name": "Alice", "role": "ADMIN"},
        "2": {"id": "2", "name": "Bob", "role": "USER"},
    }
    if uid in users:
        return respond(200, users[uid])
    return respond(200, None)

def on_users(args):
    return respond(200, [
        {"id": "1", "name": "Alice", "role": "ADMIN"},
        {"id": "2", "name": "Bob", "role": "USER"},
    ])

def on_createUser(args):
    name = args["args"]["name"]
    return respond(200, {"id": "new", "name": name, "role": "USER"})

def resolve_User_posts(args):
    parent = args["parent"]
    uid = parent["id"]
    if uid == "1":
        return respond(200, [
            {"id": "p1", "title": "Hello World"},
            {"id": "p2", "title": "GraphQL Tips"},
        ])
    return respond(200, [])
`

const graphqlAdapterYAMLTemplate = `
id: gql-blog
name: GQL Blog
graphql:
  schema: schemas/blog.graphql
  resolvers: scripts/resolvers.star
`

// setupGraphqlEngine creates an engine with a GraphQL adapter and returns
// the HTTP address + cleanup function.
func setupGraphqlEngine(t *testing.T) (string, func()) {
	t.Helper()

	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", graphqlAdapterYAMLTemplate)
	writeFile(t, adapterDir, "scripts/resolvers.star", graphqlTestStar)
	writeFile(t, adapterDir, "schemas/blog.graphql", graphqlTestSDL)

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")
	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"blog": {Adapter: adapterDir},
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

func gqlPost(t *testing.T, url, query string, variables map[string]any) (map[string]any, int) {
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

func gqlGet(t *testing.T, url, query string) (map[string]any, int) {
	t.Helper()
	resp, err := http.Get(url + "/graphql?query=" + query)
	if err != nil {
		t.Fatalf("GET /graphql: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var result map[string]any
	json.Unmarshal(data, &result)
	return result, resp.StatusCode
}

func TestGraphqlSimpleQuery(t *testing.T) {
	url, cleanup := setupGraphqlEngine(t)
	defer cleanup()

	result, status := gqlPost(t, url, `{ user(id: "1") { id name } }`, nil)
	if status != 200 {
		t.Fatalf("status = %d, want 200; body %v", status, result)
	}
	if errs, ok := result["errors"]; ok && errs != nil {
		t.Fatalf("unexpected errors: %v", errs)
	}
	data := result["data"].(map[string]any)
	user := data["user"].(map[string]any)
	if user["name"] != "Alice" {
		t.Errorf("name = %v, want Alice", user["name"])
	}
}

func TestGraphqlNestedObjectResolver(t *testing.T) {
	url, cleanup := setupGraphqlEngine(t)
	defer cleanup()

	result, status := gqlPost(t, url, `{ user(id: "1") { id name posts { id title } } }`, nil)
	if status != 200 {
		t.Fatalf("status = %d; body %v", status, result)
	}
	data := result["data"].(map[string]any)
	user := data["user"].(map[string]any)
	posts := user["posts"].([]any)
	if len(posts) != 2 {
		t.Fatalf("expected 2 posts, got %d", len(posts))
	}
	if posts[0].(map[string]any)["title"] != "Hello World" {
		t.Errorf("first post title = %v", posts[0])
	}
}

func TestGraphqlListQuery(t *testing.T) {
	url, cleanup := setupGraphqlEngine(t)
	defer cleanup()

	result, _ := gqlPost(t, url, `{ users { id name } }`, nil)
	data := result["data"].(map[string]any)
	users := data["users"].([]any)
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}
}

func TestGraphqlMutation(t *testing.T) {
	url, cleanup := setupGraphqlEngine(t)
	defer cleanup()

	result, _ := gqlPost(t, url, `mutation { createUser(name: "Carol") { id name role } }`, nil)
	data := result["data"].(map[string]any)
	user := data["createUser"].(map[string]any)
	if user["name"] != "Carol" {
		t.Errorf("name = %v", user["name"])
	}
	if user["role"] != "USER" {
		t.Errorf("role = %v", user["role"])
	}
}

func TestGraphqlVariables(t *testing.T) {
	url, cleanup := setupGraphqlEngine(t)
	defer cleanup()

	result, _ := gqlPost(t, url, `query($id: ID!) { user(id: $id) { id name } }`, map[string]any{"id": "2"})
	data := result["data"].(map[string]any)
	user := data["user"].(map[string]any)
	if user["name"] != "Bob" {
		t.Errorf("name = %v, want Bob", user["name"])
	}
}

func TestGraphqlEnum(t *testing.T) {
	url, cleanup := setupGraphqlEngine(t)
	defer cleanup()

	result, _ := gqlPost(t, url, `{ user(id: "1") { role } }`, nil)
	data := result["data"].(map[string]any)
	user := data["user"].(map[string]any)
	if user["role"] != "ADMIN" {
		t.Errorf("role = %v, want ADMIN", user["role"])
	}
}

func TestGraphqlTypename(t *testing.T) {
	url, cleanup := setupGraphqlEngine(t)
	defer cleanup()

	result, _ := gqlPost(t, url, `{ user(id: "1") { __typename } }`, nil)
	data := result["data"].(map[string]any)
	user := data["user"].(map[string]any)
	if user["__typename"] != "User" {
		t.Errorf("__typename = %v, want User", user["__typename"])
	}
}

func TestGraphqlGetRequest(t *testing.T) {
	url, cleanup := setupGraphqlEngine(t)
	defer cleanup()

	result, status := gqlGet(t, url, `%7Buser(id%3A%221%22)%7Bid+name%7D%7D`)
	if status != 200 {
		t.Fatalf("status = %d; body %v", status, result)
	}
	data, ok := result["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected data, got: %v", result)
	}
	user := data["user"].(map[string]any)
	if user["name"] != "Alice" {
		t.Errorf("name = %v", user["name"])
	}
}

func TestGraphqlFragment(t *testing.T) {
	url, cleanup := setupGraphqlEngine(t)
	defer cleanup()

	query := `
fragment UserFields on User { id name role }
{ user(id: "1") { ...UserFields } }
`
	result, _ := gqlPost(t, url, query, nil)
	data := result["data"].(map[string]any)
	user := data["user"].(map[string]any)
	if user["name"] != "Alice" {
		t.Errorf("name = %v", user["name"])
	}
	if user["role"] != "ADMIN" {
		t.Errorf("role = %v", user["role"])
	}
}

func TestGraphqlPartialResults(t *testing.T) {
	url, cleanup := setupGraphqlEngine(t)
	defer cleanup()

	// Query a user that exists and one that doesn't — both should return
	// without error (null for missing user).
	result, _ := gqlPost(t, url, `{ a: user(id: "1") { id } b: user(id: "999") { id } }`, nil)
	data := result["data"].(map[string]any)
	if data["a"] == nil {
		t.Error("a should have data")
	}
	if data["b"] != nil {
		t.Errorf("b should be null: %v", data["b"])
	}
}

func TestGraphqlParseError(t *testing.T) {
	url, cleanup := setupGraphqlEngine(t)
	defer cleanup()

	result, status := gqlPost(t, url, `{ user( }`, nil)
	if status != 400 {
		t.Fatalf("status = %d, want 400", status)
	}
	errs, ok := result["errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("expected errors, got: %v", result)
	}
}

func TestGraphqlMissingQuery(t *testing.T) {
	url, cleanup := setupGraphqlEngine(t)
	defer cleanup()

	resp, err := http.Post(url+"/graphql", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestGraphqlDefaultResolver(t *testing.T) {
	url, cleanup := setupGraphqlEngine(t)
	defer cleanup()

	// The `name` field on User uses the default resolver (parent["name"]).
	result, _ := gqlPost(t, url, `{ user(id: "1") { id name } }`, nil)
	data := result["data"].(map[string]any)
	user := data["user"].(map[string]any)
	if user["name"] != "Alice" {
		t.Errorf("name = %v, want Alice (default resolver)", user["name"])
	}
}

func TestGraphqlCustomPath(t *testing.T) {
	adapterDir := t.TempDir()
	yaml := `
id: gql-custom-path
name: GQL Custom Path
graphql:
  schema: schemas/blog.graphql
  resolvers: scripts/resolvers.star
  path: /api/graphql
`
	writeFile(t, adapterDir, "adapter.yaml", yaml)
	writeFile(t, adapterDir, "scripts/resolvers.star", graphqlTestStar)
	writeFile(t, adapterDir, "schemas/blog.graphql", graphqlTestSDL)

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"blog": {Adapter: adapterDir},
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

	// Custom path should work.
	result, status := gqlPostAt(t, addrs["blog"]+"/api/graphql", `{ user(id: "1") { id } }`, nil)
	if status != 200 {
		t.Fatalf("custom path status = %d, want 200; body %v", status, result)
	}

	// Default /graphql path should NOT match (404 or falls through).
	resp, err := http.Post(addrs["blog"]+"/graphql", "application/json", bytes.NewReader([]byte(`{"query":"{user(id:\"1\"){id}}"}`)))
	if err != nil {
		t.Fatalf("POST /graphql: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("/graphql should not match custom-path adapter; got 200: %s", body)
	}
}

func gqlPostAt(t *testing.T, fullURL, query string, variables map[string]any) (map[string]any, int) {
	t.Helper()
	body := map[string]any{"query": query}
	if variables != nil {
		body["variables"] = variables
	}
	bodyBytes, _ := json.Marshal(body)
	resp, err := http.Post(fullURL, "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("POST %s: %v", fullURL, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var result map[string]any
	json.Unmarshal(data, &result)
	return result, resp.StatusCode
}

func TestGraphqlNonGraphqlPathFallsThrough(t *testing.T) {
	url, cleanup := setupGraphqlEngine(t)
	defer cleanup()

	// A non-graphql path should fall through to normal HTTP dispatch (404).
	resp, err := http.Get(url + "/some/path")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404 for non-graphql path", resp.StatusCode)
	}
}
