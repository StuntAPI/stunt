package graphqlsim

import (
	"context"
	"testing"
)

// TestVariableCoercionMissingRequired verifies that a missing required
// non-null variable produces a GraphQL error (not silent success) (I2).
func TestVariableCoercionMissingRequired(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"id": "1", "name": "A", "role": "ADMIN"}, nil
			},
		},
	}
	// Query requires $id: ID! but we don't provide it.
	result := runQuery(t, env, `query($id: ID!) { user(id: $id) { id } }`, map[string]any{}, Options{})
	if len(result.Errors) == 0 {
		t.Fatal("expected error for missing required variable")
	}
}

// TestVariableCoercionWrongType verifies that a wrong-type variable (Int!
// with a string value) produces a GraphQL error (I2).
func TestVariableCoercionWrongType(t *testing.T) {
	schema := `
type Query { echo(n: Int!): Int }
`
	env := testEnv{
		schema: schema,
		resolvers: map[string]Resolver{
			"Query.echo": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return args["n"], nil
			},
		},
	}
	// $n is Int! but we pass a string.
	result := runQuery(t, env, `query($n: Int!) { echo(n: $n) }`, map[string]any{"n": "not-a-number"}, Options{})
	if len(result.Errors) == 0 {
		t.Fatal("expected error for wrong-type variable")
	}
}

// TestVariableCoercionValidVariable verifies that a valid variable still works
// after coercion (I2).
func TestVariableCoercionValidVariable(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"id": args["id"], "name": "X", "role": "USER"}, nil
			},
		},
	}
	result := runQuery(t, env, `query($id: ID!) { user(id: $id) { id } }`, map[string]any{"id": "42"}, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	user := result.Data.(map[string]any)["user"].(map[string]any)
	if user["id"] != "42" {
		t.Errorf("id = %v, want 42", user["id"])
	}
}

// TestVariableCoercionEnumVariable verifies that enum variables coerce
// properly (I2).
func TestVariableCoercionEnumVariable(t *testing.T) {
	schema := `
type Query { usersByRole(role: Role!): [User!]! }
enum Role { ADMIN USER }
type User { id: ID! name: String! }
`
	var capturedArgs map[string]any
	env := testEnv{
		schema: schema,
		resolvers: map[string]Resolver{
			"Query.usersByRole": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				capturedArgs = args
				return []any{map[string]any{"id": "1", "name": "Admin"}}, nil
			},
		},
	}
	result := runQuery(t, env, `query($role: Role!) { usersByRole(role: $role) { id } }`, map[string]any{"role": "ADMIN"}, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	if capturedArgs["role"] != "ADMIN" {
		t.Errorf("role = %v, want ADMIN", capturedArgs["role"])
	}
	users := result.Data.(map[string]any)["usersByRole"].([]any)
	if len(users) != 1 {
		t.Errorf("expected 1 user, got %d", len(users))
	}
}

// TestVariableCoercionNullForNullableVariable verifies that passing null for a
// nullable variable works (I2).
func TestVariableCoercionNullForNullableVariable(t *testing.T) {
	schema := `
type Query { user(id: ID, name: String): User }
type User { id: ID! name: String! }
`
	env := testEnv{
		schema: schema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				// Return a valid user regardless of args.
				return map[string]any{"id": "1", "name": "X"}, nil
			},
		},
	}
	// $id is nullable (ID), pass null — should work without errors.
	result := runQuery(t, env, `query($id: ID) { user(id: $id) { id name } }`, map[string]any{"id": nil}, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
}
