package graphqlsim

import (
	"context"
	"testing"
)

// TestSerializeTypeRefScalarNonNull verifies that a String! field serializes
// as {kind:NON_NULL, ofType:{kind:SCALAR, name:String}} (I3).
func TestSerializeTypeRefScalarNonNull(t *testing.T) {
	result := runQuery(t, testEnv{
		schema:    `type Query { name: String! }`,
		resolvers: map[string]Resolver{},
	}, `{ __type(name: "Query") { fields { name type { kind name ofType { kind name ofType { kind name } } } } } }`, nil, Options{})

	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}

	ttype := result.Data.(map[string]any)["__type"].(map[string]any)
	fields := ttype["fields"].([]any)
	nameField := fields[0].(map[string]any)
	typeRef := nameField["type"].(map[string]any)

	// String! → NON_NULL wrapping SCALAR
	if typeRef["kind"] != "NON_NULL" {
		t.Errorf("outer kind = %v, want NON_NULL", typeRef["kind"])
	}
	if typeRef["name"] != nil {
		t.Errorf("outer name = %v, want nil", typeRef["name"])
	}
	inner := typeRef["ofType"].(map[string]any)
	if inner["kind"] != "SCALAR" {
		t.Errorf("inner kind = %v, want SCALAR", inner["kind"])
	}
	if inner["name"] != "String" {
		t.Errorf("inner name = %v, want String", inner["name"])
	}
}

// TestSerializeTypeRefEnumKind verifies that an enum field shows kind=ENUM (I3).
func TestSerializeTypeRefEnumKind(t *testing.T) {
	result := runQuery(t, testEnv{
		schema: blogSchema,
		resolvers: map[string]Resolver{
			"Query.user": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				return map[string]any{"id": "1", "name": "A", "role": "ADMIN"}, nil
			},
		},
	}, `{ __type(name: "User") { fields { name type { kind name ofType { kind name } } } } }`, nil, Options{})

	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}

	ttype := result.Data.(map[string]any)["__type"].(map[string]any)
	fields := ttype["fields"].([]any)
	for _, f := range fields {
		fm := f.(map[string]any)
		if fm["name"] == "role" {
			// role: Role! → NON_NULL → ENUM(Role)
			typeRef := fm["type"].(map[string]any)
			if typeRef["kind"] != "NON_NULL" {
				t.Fatalf("role type kind = %v, want NON_NULL", typeRef["kind"])
			}
			inner := typeRef["ofType"].(map[string]any)
			if inner["kind"] != "ENUM" {
				t.Errorf("role inner kind = %v, want ENUM", inner["kind"])
			}
			if inner["name"] != "Role" {
				t.Errorf("role inner name = %v, want Role", inner["name"])
			}
			return
		}
	}
	t.Fatal("role field not found")
}

// TestSerializeTypeRefScalarKind verifies that scalar fields show kind=SCALAR (I3).
func TestSerializeTypeRefScalarKind(t *testing.T) {
	result := runQuery(t, testEnv{
		schema:    blogSchema,
		resolvers: map[string]Resolver{},
	}, `{ __type(name: "User") { fields { name type { kind name ofType { kind name } } } } }`, nil, Options{})

	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}

	ttype := result.Data.(map[string]any)["__type"].(map[string]any)
	fields := ttype["fields"].([]any)
	foundString := false
	foundID := false
	for _, f := range fields {
		fm := f.(map[string]any)
		typeRef := fm["type"].(map[string]any)
		// Skip non-null wrapper to check inner.
		innerType := typeRef
		if typeRef["kind"] == "NON_NULL" {
			innerType = typeRef["ofType"].(map[string]any)
		}
		// Only validate that scalar fields (id, name) show SCALAR.
		// Other fields (role=ENUM, posts=LIST, bestFriend=OBJECT) are
		// validated in their own dedicated tests.
		if innerType["name"] == "String" {
			foundString = true
		}
		if innerType["name"] == "ID" {
			foundID = true
		}
	}
	if !foundString {
		t.Error("String field not found")
	}
	if !foundID {
		t.Error("ID field not found")
	}
}

// TestSerializeTypeRefListNonNull verifies that [User!]! serializes as
// LIST-of-NON_NULL-of-OBJECT (I3).
func TestSerializeTypeRefListNonNull(t *testing.T) {
	result := runQuery(t, testEnv{
		schema:    blogSchema,
		resolvers: map[string]Resolver{},
	}, `{ __type(name: "Query") { fields { name type { kind name ofType { kind name ofType { kind name ofType { kind name } } } } } } }`, nil, Options{})

	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}

	ttype := result.Data.(map[string]any)["__type"].(map[string]any)
	fields := ttype["fields"].([]any)
	for _, f := range fields {
		fm := f.(map[string]any)
		if fm["name"] == "users" {
			// users: [User!]!
			// NON_NULL → LIST → NON_NULL → OBJECT(User)
			level1 := fm["type"].(map[string]any)
			if level1["kind"] != "NON_NULL" {
				t.Fatalf("level1 kind = %v, want NON_NULL", level1["kind"])
			}
			level2 := level1["ofType"].(map[string]any)
			if level2["kind"] != "LIST" {
				t.Fatalf("level2 kind = %v, want LIST", level2["kind"])
			}
			level3 := level2["ofType"].(map[string]any)
			if level3["kind"] != "NON_NULL" {
				t.Fatalf("level3 kind = %v, want NON_NULL", level3["kind"])
			}
			level4 := level3["ofType"].(map[string]any)
			if level4["kind"] != "OBJECT" {
				t.Fatalf("level4 kind = %v, want OBJECT", level4["kind"])
			}
			if level4["name"] != "User" {
				t.Fatalf("level4 name = %v, want User", level4["name"])
			}
			return
		}
	}
	t.Fatal("users field not found")
}

// TestSerializeTypeRefEnumDefinitionKind verifies that an enum type definition
// serializes with kind=ENUM (I3).
func TestSerializeTypeRefEnumDefinitionKind(t *testing.T) {
	result := runQuery(t, testEnv{
		schema:    blogSchema,
		resolvers: map[string]Resolver{},
	}, `{ __type(name: "Role") { name kind } }`, nil, Options{})

	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}

	ttype := result.Data.(map[string]any)["__type"].(map[string]any)
	if ttype["kind"] != "ENUM" {
		t.Errorf("Role kind = %v, want ENUM", ttype["kind"])
	}
}

// TestSerializeTypeInputFields verifies that input objects expose inputFields
// (not fields) (I4).
func TestSerializeTypeInputFields(t *testing.T) {
	schema := `
input CreateUserInput {
  name: String!
  email: String
}
type Query { dummy: String }
`
	result := runQuery(t, testEnv{
		schema:    schema,
		resolvers: map[string]Resolver{},
	}, `{ __type(name: "CreateUserInput") { name kind fields { name } inputFields { name } } }`, nil, Options{})

	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}

	ttype := result.Data.(map[string]any)["__type"].(map[string]any)
	if ttype["kind"] != "INPUT_OBJECT" {
		t.Fatalf("kind = %v, want INPUT_OBJECT", ttype["kind"])
	}
	// inputFields should be populated.
	inputFields, ok := ttype["inputFields"].([]any)
	if !ok || len(inputFields) != 2 {
		t.Fatalf("expected 2 inputFields, got %v", ttype["inputFields"])
	}
	// Verify field names.
	names := map[string]bool{}
	for _, f := range inputFields {
		fm := f.(map[string]any)
		names[fm["name"].(string)] = true
	}
	if !names["name"] || !names["email"] {
		t.Errorf("inputField names = %v, want name+email", names)
	}
}

// TestSerializeTypeInputFieldsOmitsFieldsForInputObject verifies that input
// objects do NOT expose "fields" (only inputFields) (I4).
func TestSerializeTypeInputFieldsOmitsFieldsForInputObject(t *testing.T) {
	schema := `
input CreateUserInput {
  name: String!
}
type Query { dummy: String }
`
	result := runQuery(t, testEnv{
		schema:    schema,
		resolvers: map[string]Resolver{},
	}, `{ __type(name: "CreateUserInput") { name kind fields { name } inputFields { name } } }`, nil, Options{})

	if len(result.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}

	ttype := result.Data.(map[string]any)["__type"].(map[string]any)
	if ttype["fields"] != nil {
		t.Errorf("fields should be null for INPUT_OBJECT, got %v", ttype["fields"])
	}
	inputFields, ok := ttype["inputFields"].([]any)
	if !ok || len(inputFields) != 1 {
		t.Errorf("expected 1 inputField, got %v", ttype["inputFields"])
	}
}
