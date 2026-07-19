package graphqlsim

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// helper to build a schema + resolver set for tests.
type testEnv struct {
	schema    string
	resolvers map[string]Resolver
}

func runQuery(t *testing.T, env testEnv, query string, vars map[string]any, opts Options) *Result {
	t.Helper()
	schema, err := LoadSchema([]byte(env.schema))
	if err != nil {
		t.Fatalf("LoadSchema: %v", err)
	}
	rs := &MapResolverSet{Resolvers: env.resolvers}
	result, err := Execute(context.Background(), schema, query, vars, "", rs, opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return result
}

const blogSchema = `
type Query {
  user(id: ID!): User
  users: [User!]!
  post(id: ID!): Post
}
type Mutation {
  createUser(name: String!, role: Role = USER): User!
}
type User {
  id: ID!
  name: String!
  role: Role!
  posts: [Post!]!
  bestFriend: User
}
type Post {
  id: ID!
  title: String!
  author: User
  tags: [String!]!
}
enum Role {
  ADMIN
  USER
}
`

func TestLoadSchemaInvalid(t *testing.T) {
	_, err := LoadSchema([]byte("type Query { x }")) // missing type for x
	if err == nil {
		t.Fatal("expected error for invalid SDL")
	}
}

func TestSimpleQuery(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"id": "1", "name": "Alice", "role": "ADMIN"}, nil
			},
		},
	}
	result := runQuery(t, env, `{ user(id: "1") { id name } }`, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", result.Data)
	}
	user, ok := data["user"].(map[string]any)
	if !ok {
		t.Fatalf("expected user map, got %T", data["user"])
	}
	if user["id"] != "1" {
		t.Errorf("id = %v", user["id"])
	}
	if user["name"] != "Alice" {
		t.Errorf("name = %v", user["name"])
	}
}

func TestNestedObjectDefaultResolver(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.post": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{
					"id":    "p1",
					"title": "Hello",
					"author": map[string]any{
						"id":   "u1",
						"name": "Bob",
					},
				}, nil
			},
		},
	}
	result := runQuery(t, env, `{ post(id: "p1") { id title author { id name } } }`, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	post := result.Data.(map[string]any)["post"].(map[string]any)
	if post["title"] != "Hello" {
		t.Errorf("title = %v", post["title"])
	}
	author := post["author"].(map[string]any)
	if author["name"] != "Bob" {
		t.Errorf("author name = %v", author["name"])
	}
}

func TestList(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.users": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return []any{
					map[string]any{"id": "1", "name": "Alice", "role": "ADMIN"},
					map[string]any{"id": "2", "name": "Bob", "role": "USER"},
				}, nil
			},
		},
	}
	result := runQuery(t, env, `{ users { id name } }`, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	users := result.Data.(map[string]any)["users"].([]any)
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}
	if users[0].(map[string]any)["name"] != "Alice" {
		t.Errorf("first user name = %v", users[0])
	}
}

func TestListTags(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.post": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{
					"id":    "p1",
					"title": "Hello",
					"tags":  []any{"go", "graphql"},
				}, nil
			},
		},
	}
	result := runQuery(t, env, `{ post(id: "p1") { id tags } }`, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	post := result.Data.(map[string]any)["post"].(map[string]any)
	tags := post["tags"].([]any)
	if len(tags) != 2 || tags[0] != "go" || tags[1] != "graphql" {
		t.Errorf("tags = %v", tags)
	}
}

func TestMutation(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Mutation.createUser": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{
					"id":   "new",
					"name": args["name"],
					"role": args["role"],
				}, nil
			},
		},
	}
	result := runQuery(t, env, `mutation { createUser(name: "Carol") { id name role } }`, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	user := result.Data.(map[string]any)["createUser"].(map[string]any)
	if user["name"] != "Carol" {
		t.Errorf("name = %v", user["name"])
	}
	if user["role"] != "USER" {
		t.Errorf("role = %v (expected default USER)", user["role"])
	}
}

func TestArgsLiteral(t *testing.T) {
	var capturedArgs map[string]any
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				capturedArgs = args
				return map[string]any{"id": args["id"], "name": "X", "role": "USER"}, nil
			},
		},
	}
	runQuery(t, env, `{ user(id: "42") { id } }`, nil, Options{})
	if capturedArgs["id"] != "42" {
		t.Errorf("args.id = %v, want 42", capturedArgs["id"])
	}
}

func TestArgsVariable(t *testing.T) {
	var capturedArgs map[string]any
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				capturedArgs = args
				return map[string]any{"id": args["id"], "name": "X", "role": "USER"}, nil
			},
		},
	}
	runQuery(t, env, `query($id: ID!) { user(id: $id) { id } }`, map[string]any{"id": "99"}, Options{})
	if capturedArgs["id"] != "99" {
		t.Errorf("args.id = %v, want 99", capturedArgs["id"])
	}
}

func TestArgsDefault(t *testing.T) {
	var capturedArgs map[string]any
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Mutation.createUser": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				capturedArgs = args
				return map[string]any{"id": "x", "name": "y", "role": "USER"}, nil
			},
		},
	}
	// No role arg provided → should get default "USER".
	runQuery(t, env, `mutation { createUser(name: "Z") { id } }`, nil, Options{})
	if capturedArgs["role"] != "USER" {
		t.Errorf("default role = %v, want USER", capturedArgs["role"])
	}
}

func TestEnum(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"id": "1", "name": "Alice", "role": "ADMIN"}, nil
			},
		},
	}
	result := runQuery(t, env, `{ user(id: "1") { role } }`, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	user := result.Data.(map[string]any)["user"].(map[string]any)
	if user["role"] != "ADMIN" {
		t.Errorf("role = %v, want ADMIN", user["role"])
	}
}

func TestTypename(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"id": "1", "name": "Alice", "role": "ADMIN"}, nil
			},
		},
	}
	result := runQuery(t, env, `{ user(id: "1") { __typename } }`, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	user := result.Data.(map[string]any)["user"].(map[string]any)
	if user["__typename"] != "User" {
		t.Errorf("__typename = %v, want User", user["__typename"])
	}
}

func TestNamedFragment(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"id": "1", "name": "Alice", "role": "ADMIN"}, nil
			},
		},
	}
	query := `
fragment UserFields on User { id name }
{ user(id: "1") { ...UserFields } }
`
	result := runQuery(t, env, query, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	user := result.Data.(map[string]any)["user"].(map[string]any)
	if user["id"] != "1" {
		t.Errorf("id = %v", user["id"])
	}
	if user["name"] != "Alice" {
		t.Errorf("name = %v", user["name"])
	}
}

func TestInlineFragment(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"id": "1", "name": "Alice", "role": "ADMIN"}, nil
			},
		},
	}
	query := `{ user(id: "1") { ... on User { id name } } }`
	result := runQuery(t, env, query, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	user := result.Data.(map[string]any)["user"].(map[string]any)
	if user["name"] != "Alice" {
		t.Errorf("name = %v", user["name"])
	}
}

func TestSkipTrue(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"id": "1", "name": "Alice", "role": "ADMIN"}, nil
			},
		},
	}
	query := `{ user(id: "1") { id name @skip(if: true) } }`
	result := runQuery(t, env, query, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	user := result.Data.(map[string]any)["user"].(map[string]any)
	if _, exists := user["name"]; exists {
		t.Error("name should be skipped")
	}
	if user["id"] != "1" {
		t.Errorf("id = %v", user["id"])
	}
}

func TestSkipFalse(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"id": "1", "name": "Alice", "role": "ADMIN"}, nil
			},
		},
	}
	query := `{ user(id: "1") { id name @skip(if: false) } }`
	result := runQuery(t, env, query, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	user := result.Data.(map[string]any)["user"].(map[string]any)
	if user["name"] != "Alice" {
		t.Errorf("name = %v (should be present)", user["name"])
	}
}

func TestIncludeFalse(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"id": "1", "name": "Alice", "role": "ADMIN"}, nil
			},
		},
	}
	query := `{ user(id: "1") { id name @include(if: false) } }`
	result := runQuery(t, env, query, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	user := result.Data.(map[string]any)["user"].(map[string]any)
	if _, exists := user["name"]; exists {
		t.Error("name should be excluded")
	}
}

func TestSkipWithVariable(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"id": "1", "name": "Alice", "role": "ADMIN"}, nil
			},
		},
	}
	query := `query($skipName: Boolean!) { user(id: "1") { id name @skip(if: $skipName) } }`
	result := runQuery(t, env, query, map[string]any{"skipName": true}, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	user := result.Data.(map[string]any)["user"].(map[string]any)
	if _, exists := user["name"]; exists {
		t.Error("name should be skipped when skipName=true")
	}
}

func TestIncludeWithVariable(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"id": "1", "name": "Alice", "role": "ADMIN"}, nil
			},
		},
	}
	query := `query($incl: Boolean!) { user(id: "1") { id name @include(if: $incl) } }`
	result := runQuery(t, env, query, map[string]any{"incl": true}, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	user := result.Data.(map[string]any)["user"].(map[string]any)
	if user["name"] != "Alice" {
		t.Error("name should be present when incl=true")
	}
}

func TestNonNullErrorPropagatesToParent(t *testing.T) {
	// user: User (nullable). If bestFriend (User) returns an object but
	// bestFriend.name (String!) is null → bestFriend should be null, but
	// user should still have data.
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{
					"id":         "1",
					"name":       "Alice",
					"role":       "ADMIN",
					"bestFriend": map[string]any{"id": "2"}, // missing name (String!)
				}, nil
			},
		},
	}
	result := runQuery(t, env, `{ user(id: "1") { id name bestFriend { id name } } }`, nil, Options{})
	if len(result.Errors) == 0 {
		t.Fatal("expected at least one error")
	}
	data := result.Data.(map[string]any)
	user := data["user"].(map[string]any)
	if user["name"] != "Alice" {
		t.Errorf("user.name should still be present: %v", user["name"])
	}
	if user["bestFriend"] != nil {
		t.Errorf("bestFriend should be null due to non-null violation: %v", user["bestFriend"])
	}
}

func TestNonNullRootErrorPropagatesToDataNull(t *testing.T) {
	// user(id: "1"): User! — but we return null, causing data: null.
	schemaWithNonNullRoot := `
type Query { user(id: ID!): User! }
type User { id: ID! name: String! }
`
	env := testEnv{
		schema: schemaWithNonNullRoot,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return nil, errors.New("user not found")
			},
		},
	}
	result := runQuery(t, env, `{ user(id: "missing") { id name } }`, nil, Options{})
	if result.Data != nil {
		t.Errorf("data should be null, got %v", result.Data)
	}
	if len(result.Errors) == 0 {
		t.Error("expected errors")
	}
}

func TestPartialResults(t *testing.T) {
	// Two root fields: one succeeds, one errors.
	schema := `
type Query { a: String b: String }
`
	env := testEnv{
		schema: schema,
		resolvers: map[string]Resolver{
			"Query.a": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return "hello", nil
			},
			"Query.b": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return nil, errors.New("b failed")
			},
		},
	}
	result := runQuery(t, env, `{ a b }`, nil, Options{})
	data := result.Data.(map[string]any)
	if data["a"] != "hello" {
		t.Errorf("a = %v", data["a"])
	}
	if data["b"] != nil {
		t.Errorf("b should be null, got %v", data["b"])
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(result.Errors), result.Errors)
	}
}

func TestDepthLimitRejection(t *testing.T) {
	// A deeply nested query that exceeds MaxDepth.
	schema := `
type Query { a: A }
type A { a: A }
`
	env := testEnv{
		schema: schema,
		resolvers: map[string]Resolver{
			"Query.a": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{}, nil
			},
		},
	}
	// Depth 5 query with MaxDepth 3.
	query := `{ a { a { a { a { a } } } } }`
	schema_, err := LoadSchema([]byte(env.schema))
	if err != nil {
		t.Fatalf("LoadSchema: %v", err)
	}
	rs := &MapResolverSet{Resolvers: env.resolvers}
	_, err = Execute(context.Background(), schema_, query, nil, "", rs, Options{MaxDepth: 3})
	if err == nil {
		t.Fatal("expected depth limit error")
	}
}

func TestFieldCountLimit(t *testing.T) {
	// Build a query with many fields.
	schema := `
type Query { a: String b: String c: String d: String e: String f: String }
`
	env := testEnv{
		schema: schema,
		resolvers: map[string]Resolver{
			"Query.a": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return "1", nil
			},
		},
	}
	schema_, err := LoadSchema([]byte(env.schema))
	if err != nil {
		t.Fatalf("LoadSchema: %v", err)
	}
	rs := &MapResolverSet{Resolvers: env.resolvers}
	query := `{ a b c d e f }`
	_, err = Execute(context.Background(), schema_, query, nil, "", rs, Options{MaxFields: 3})
	if err == nil {
		t.Fatal("expected field count limit error")
	}
}

func TestTimeout(t *testing.T) {
	schema := `type Query { slow: String }`
	env := testEnv{
		schema: schema,
		resolvers: map[string]Resolver{
			"Query.slow": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				select {
				case <-time.After(200 * time.Millisecond):
					return "done", nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			},
		},
	}
	schema_, err := LoadSchema([]byte(env.schema))
	if err != nil {
		t.Fatalf("LoadSchema: %v", err)
	}
	rs := &MapResolverSet{Resolvers: env.resolvers}
	result, err := Execute(context.Background(), schema_, `{ slow }`, nil, "", rs, Options{Timeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(result.Errors) == 0 {
		t.Fatal("expected timeout error in result")
	}
}

func TestCustomResolverForObjectField(t *testing.T) {
	// resolve_User_posts is a custom resolver that filters by parent id.
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"id": "u1", "name": "Alice", "role": "ADMIN"}, nil
			},
			"User.posts": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return []any{
					map[string]any{"id": "p1", "title": "Post 1", "tags": []any{}},
				}, nil
			},
		},
	}
	result := runQuery(t, env, `{ user(id: "u1") { id posts { id title } } }`, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	user := result.Data.(map[string]any)["user"].(map[string]any)
	posts := user["posts"].([]any)
	if len(posts) != 1 {
		t.Fatalf("expected 1 post, got %d", len(posts))
	}
	if posts[0].(map[string]any)["title"] != "Post 1" {
		t.Errorf("post title = %v", posts[0])
	}
}

func TestMissingRootResolverIsError(t *testing.T) {
	env := testEnv{
		schema:    blogSchema,
		resolvers: map[string]Resolver{}, // no resolvers at all
	}
	result := runQuery(t, env, `{ user(id: "1") { id } }`, nil, Options{})
	if len(result.Errors) == 0 {
		t.Fatal("expected error for missing root resolver")
	}
}

func TestIntScalar(t *testing.T) {
	schema := `type Query { count: Int }`
	env := testEnv{
		schema: schema,
		resolvers: map[string]Resolver{
			"Query.count": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return int64(42), nil
			},
		},
	}
	result := runQuery(t, env, `{ count }`, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	if result.Data.(map[string]any)["count"] != int64(42) {
		t.Errorf("count = %v", result.Data.(map[string]any)["count"])
	}
}

func TestAlias(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"id": "1", "name": "Alice", "role": "ADMIN"}, nil
			},
		},
	}
	result := runQuery(t, env, `{ alice: user(id: "1") { id } }`, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	data := result.Data.(map[string]any)
	if _, ok := data["alice"]; !ok {
		t.Errorf("expected alias 'alice', got keys: %v", keys(data))
	}
}

func keys(m map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func TestOperationName(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"id": "1", "name": "Alice", "role": "ADMIN"}, nil
			},
		},
	}
	schema_, err := LoadSchema([]byte(env.schema))
	if err != nil {
		t.Fatalf("LoadSchema: %v", err)
	}
	rs := &MapResolverSet{Resolvers: env.resolvers}
	// Two operations, select by name.
	result, err := Execute(context.Background(), schema_, `
		query First { user(id: "1") { id } }
		query Second { user(id: "2") { name } }
	`, nil, "Second", rs, Options{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
}

func TestCustomScalarPassthrough(t *testing.T) {
	schema := `
scalar JSON
type Query { config: JSON }
`
	env := testEnv{
		schema: schema,
		resolvers: map[string]Resolver{
			"Query.config": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"nested": map[string]any{"key": "value"}}, nil
			},
		},
	}
	result := runQuery(t, env, `{ config }`, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	data := result.Data.(map[string]any)["config"].(map[string]any)
	if data["nested"] == nil {
		t.Error("custom scalar data should pass through")
	}
}

func TestNullOnNullableList(t *testing.T) {
	// users: [User!]! — if resolver returns null for a non-null list element,
	// the whole list should be null.
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.users": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return []any{
					map[string]any{"id": "1", "name": "A", "role": "ADMIN"},
					nil, // null element in [User!]!
				}, nil
			},
		},
	}
	result := runQuery(t, env, `{ users { id } }`, nil, Options{})
	// users is [User!]!, so a null element → entire list is null.
	var dataVal any = result.Data
	if dataVal == nil {
		// data itself was nulled — also acceptable for non-null propagation
		return
	}
	data := result.Data.(map[string]any)
	if data["users"] != nil {
		t.Errorf("users should be null, got %v", data["users"])
	}
	if len(result.Errors) == 0 {
		t.Error("expected errors")
	}
}

func TestNonNullErrorInListElementPropagates(t *testing.T) {
	// A non-null element error should propagate up through the list.
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.users": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return []any{
					map[string]any{"id": "1"}, // missing name (String!)
				}, nil
			},
		},
	}
	result := runQuery(t, env, `{ users { id name } }`, nil, Options{})
	// The element has a non-null violation on name, and the list is [User!]!,
	// so the whole list should be null.
	var dataVal any = result.Data
	if dataVal == nil {
		return // data nulled entirely — also acceptable
	}
	data := result.Data.(map[string]any)
	if data["users"] != nil {
		t.Errorf("users should be null due to non-null propagation: %v", data["users"])
	}
	if len(result.Errors) == 0 {
		t.Error("expected errors")
	}
}

func TestErrorMessage(t *testing.T) {
	schema := `type Query { a: String }`
	env := testEnv{
		schema: schema,
		resolvers: map[string]Resolver{
			"Query.a": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return nil, fmt.Errorf("custom error message")
			},
		},
	}
	result := runQuery(t, env, `{ a }`, nil, Options{})
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if result.Errors[0].Message != "custom error message" {
		t.Errorf("error message = %q", result.Errors[0].Message)
	}
}

func TestErrorPath(t *testing.T) {
	schema := `
type Query { user: User }
type User { name: String! }
`
	env := testEnv{
		schema: schema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{}, nil // missing name (String!)
			},
		},
	}
	result := runQuery(t, env, `{ user { name } }`, nil, Options{})
	// name is non-null and null → error should be recorded, user=null.
	data := result.Data.(map[string]any)
	if data["user"] != nil {
		t.Errorf("user should be null: %v", data["user"])
	}
	if len(result.Errors) == 0 {
		t.Fatal("expected errors")
	}
	// Error path should include "user" and "name".
	e := result.Errors[0]
	if len(e.Path) < 2 {
		t.Errorf("expected path of length >=2, got %v", e.Path)
	}
}

func TestInterfaceBasic(t *testing.T) {
	schema := `
interface Node { id: ID! }
type User implements Node { id: ID! name: String! }
type Post implements Node { id: ID! title: String! }
type Query { node(id: ID!): Node }
`
	env := testEnv{
		schema: schema,
		resolvers: map[string]Resolver{
			"Query.node": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"__typename": "User", "id": "1", "name": "Alice"}, nil
			},
		},
	}
	// Inline fragment to access User-specific fields on Node.
	query := `{ node(id: "1") { id __typename ... on User { name } } }`
	result := runQuery(t, env, query, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	node := result.Data.(map[string]any)["node"].(map[string]any)
	if node["__typename"] != "User" {
		t.Errorf("__typename = %v", node["__typename"])
	}
	if node["name"] != "Alice" {
		t.Errorf("name = %v", node["name"])
	}
}

func TestIntrospectionSchema(t *testing.T) {
	schema := `
type Query { hello: String }
`
	env := testEnv{
		schema:    schema,
		resolvers: map[string]Resolver{},
	}
	result := runQuery(t, env, `{ __schema { types { name } queryType { name } } }`, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	data := result.Data.(map[string]any)
	meta := data["__schema"].(map[string]any)
	queryType := meta["queryType"].(map[string]any)
	if queryType["name"] != "Query" {
		t.Errorf("queryType.name = %v", queryType["name"])
	}
	types := meta["types"].([]any)
	found := false
	for _, tm := range types {
		if tm.(map[string]any)["name"] == "Query" {
			found = true
		}
	}
	if !found {
		t.Error("Query type not found in __schema types")
	}
}

func TestIntrospectionType(t *testing.T) {
	schema := `
type Query { user: User }
type User { id: ID! name: String! }
`
	env := testEnv{
		schema:    schema,
		resolvers: map[string]Resolver{},
	}
	result := runQuery(t, env, `{ __type(name: "User") { name kind fields { name } } }`, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	data := result.Data.(map[string]any)
	ttype := data["__type"].(map[string]any)
	if ttype["name"] != "User" {
		t.Errorf("name = %v", ttype["name"])
	}
	if ttype["kind"] != "OBJECT" {
		t.Errorf("kind = %v", ttype["kind"])
	}
	fields := ttype["fields"].([]any)
	if len(fields) != 2 {
		t.Errorf("expected 2 fields, got %d", len(fields))
	}
}
