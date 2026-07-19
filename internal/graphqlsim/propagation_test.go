package graphqlsim

import (
	"context"
	"testing"
)

// TestDeepNonNullPropagationSingleError verifies that a query with
// A!→B!→C!→name! where name's resolver returns null produces exactly ONE
// error at the deepest path, with data:null at the root (I1).
func TestDeepNonNullPropagationSingleError(t *testing.T) {
	schema := `
type Query { a: A! }
type A { b: B! }
type B { c: C! }
type C { name: String! }
`
	env := testEnv{
		schema: schema,
		resolvers: map[string]Resolver{
			"Query.a": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{
					"b": map[string]any{
						"c": map[string]any{
							// name (String!) is missing/null.
						},
					},
				}, nil
			},
		},
	}
	result := runQuery(t, env, `{ a { b { c { name } } } }`, nil, Options{})

	// data must be null at the root (all fields are non-null).
	if result.Data != nil {
		t.Errorf("data should be null, got %v", result.Data)
	}

	// Exactly ONE error (not one per NonNull level).
	if len(result.Errors) != 1 {
		t.Fatalf("expected exactly 1 error, got %d: %v", len(result.Errors), result.Errors)
	}

	// Error path should be the deepest field.
	e := result.Errors[0]
	if len(e.Path) != 4 {
		t.Errorf("error path = %v, expected 4 elements [a b c name]", e.Path)
	}
}

// TestNonNullListElementPropagationSingleError verifies that a null element
// in a non-null list produces exactly one error (I1).
func TestNonNullListElementPropagationSingleError(t *testing.T) {
	schema := `
type Query { items: [String!]! }
`
	env := testEnv{
		schema: schema,
		resolvers: map[string]Resolver{
			"Query.items": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return []any{"a", nil, "c"}, nil
			},
		},
	}
	result := runQuery(t, env, `{ items }`, nil, Options{})

	if result.Data != nil {
		t.Errorf("data should be null, got %v", result.Data)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected exactly 1 error, got %d: %v", len(result.Errors), result.Errors)
	}
}

// TestNullableFieldAbsorbsNonNullChild verifies that a nullable parent field
// absorbs non-null propagation from a child, producing one error and partial
// data (I1 + existing behavior).
func TestNullableFieldAbsorbsNonNullChild(t *testing.T) {
	// bestFriend: User (nullable), name: String! (non-null inside bestFriend)
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{
					"id":   "1",
					"name": "Alice",
					"bestFriend": map[string]any{
						"id": "2", // missing name (String!)
					},
				}, nil
			},
		},
	}
	result := runQuery(t, env, `{ user(id: "1") { id name bestFriend { id name } } }`, nil, Options{})

	if len(result.Errors) != 1 {
		t.Fatalf("expected exactly 1 error, got %d: %v", len(result.Errors), result.Errors)
	}
	user := result.Data.(map[string]any)["user"].(map[string]any)
	if user["name"] != "Alice" {
		t.Errorf("user.name = %v, want Alice", user["name"])
	}
	if user["bestFriend"] != nil {
		t.Errorf("bestFriend should be null, got %v", user["bestFriend"])
	}
}

// TestEnumValidationInvalidValue verifies that an invalid enum value from a
// resolver is handled properly (M1).
func TestEnumValidationInvalidValue(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				// role is Role! but we return an invalid value.
				return map[string]any{"id": "1", "name": "A", "role": "SUPERADMIN"}, nil
			},
		},
	}
	// Query role field which is Role! (non-null enum)
	result := runQuery(t, env, `{ user(id: "1") { role } }`, nil, Options{})

	// role is Role! (non-null) and invalid. Since user is nullable, the
	// non-null violation propagates up but is absorbed by user → user=null.
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map data, got %T", result.Data)
	}
	if data["user"] != nil {
		t.Errorf("user should be null for invalid non-null enum, got %v", data["user"])
	}
	if len(result.Errors) == 0 {
		t.Error("expected error for invalid enum value")
	}
}

// TestEnumValidationValidValue verifies that valid enum values still work (M1).
func TestEnumValidationValidValue(t *testing.T) {
	env := testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"id": "1", "name": "A", "role": "ADMIN"}, nil
			},
		},
	}
	result := runQuery(t, env, `{ user(id: "1") { role } }`, nil, Options{})
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	role := result.Data.(map[string]any)["user"].(map[string]any)["role"]
	if role != "ADMIN" {
		t.Errorf("role = %v, want ADMIN", role)
	}
}

// TestEnumValidationNullableFieldInvalidValue verifies that an invalid enum
// value on a nullable field is nullified but doesn't propagate (M1).
func TestEnumValidationNullableFieldInvalidValue(t *testing.T) {
	schema := `
type Query { status: Status }
enum Status { ACTIVE INACTIVE }
`
	env := testEnv{
		schema: schema,
		resolvers: map[string]Resolver{
			"Query.status": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return "UNKNOWN", nil // invalid enum value
			},
		},
	}
	result := runQuery(t, env, `{ status }`, nil, Options{})
	// status is nullable, so it should be null with an error.
	data := result.Data.(map[string]any)
	if data["status"] != nil {
		t.Errorf("status should be null for invalid enum, got %v", data["status"])
	}
}
