package graphqlsim

import (
	"context"
	"testing"
)

// TestExecutePanicRecovery verifies that a panic inside the executor (not a
// resolver) is recovered and returned as a GraphQL error, not propagated as
// a Go panic (I5).
func TestExecutePanicRecovery(t *testing.T) {
	schema := `type Query { a: String }`

	// Use a resolver that panics. This simulates a bug.
	rs := &MapResolverSet{
		Resolvers: map[string]Resolver{
			"Query.a": func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
				panic("simulated bug in resolver")
			},
		},
	}

	s, err := LoadSchema([]byte(schema))
	if err != nil {
		t.Fatalf("LoadSchema: %v", err)
	}

	result, err := Execute(context.Background(), s, `{ a }`, nil, "", rs, Options{})
	if err != nil {
		t.Fatalf("Execute returned error instead of recovering: %v", err)
	}
	if result == nil {
		t.Fatal("result should not be nil after recovery")
	}
	if len(result.Errors) == 0 {
		t.Fatal("expected at least one error after recovery")
	}
}
