package validator

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("unmarshal test json: %v", err)
	}
	return m
}

func TestValidObjectPasses(t *testing.T) {
	v := NewValidator()
	schema := `{
		"type": "object",
		"properties": {
			"name": {"type": "string"},
			"age":  {"type": "integer"}
		},
		"required": ["name", "age"]
	}`
	s, err := v.Compile([]byte(schema))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	doc := mustJSON(t, `{"name": "Alice", "age": 30}`)
	errs, err := s.Validate(doc)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %d: %+v", len(errs), errs)
	}
}

func TestMissingRequiredFails(t *testing.T) {
	v := NewValidator()
	schema := `{
		"type": "object",
		"properties": {
			"name": {"type": "string"},
			"age":  {"type": "integer"}
		},
		"required": ["name", "age"]
	}`
	s, err := v.Compile([]byte(schema))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	// Missing "age".
	doc := mustJSON(t, `{"name": "Alice"}`)
	errs, err := s.Validate(doc)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(errs) == 0 {
		t.Fatal("expected validation errors for missing required field, got none")
	}
	// At least one error should mention "age".
	if !anyMessage(errs, "age") {
		t.Fatalf("no error mentions \"age\": %+v", errs)
	}
}

func TestWrongTypeFails(t *testing.T) {
	v := NewValidator()
	schema := `{
		"type": "object",
		"properties": {
			"age": {"type": "integer"}
		},
		"required": ["age"]
	}`
	s, err := v.Compile([]byte(schema))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	// "age" is a string, not an integer.
	doc := mustJSON(t, `{"age": "thirty"}`)
	errs, err := s.Validate(doc)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(errs) == 0 {
		t.Fatal("expected validation errors for wrong type, got none")
	}
	// The error path should reference the "age" property.
	if !anyPath(errs, "age") {
		t.Fatalf("no error path references \"age\": %+v", errs)
	}
}

func TestMultipleErrorsCollected(t *testing.T) {
	v := NewValidator()
	schema := `{
		"type": "object",
		"properties": {
			"name": {"type": "string"},
			"age":  {"type": "integer"}
		},
		"required": ["name", "age"]
	}`
	s, err := v.Compile([]byte(schema))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	// Both "name" and "age" are missing → at least two errors.
	doc := mustJSON(t, `{}`)
	errs, err := s.Validate(doc)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(errs) < 2 {
		t.Fatalf("expected at least 2 errors, got %d: %+v", len(errs), errs)
	}
}

func TestCompileInvalidSchema(t *testing.T) {
	v := NewValidator()
	// Invalid JSON-Schema: the "type" keyword has an invalid value.
	_, err := v.Compile([]byte(`{"type": "not-a-real-type"}`))
	if err == nil {
		t.Fatal("expected error compiling invalid schema, got nil")
	}
}

func TestCompileInvalidJSON(t *testing.T) {
	v := NewValidator()
	_, err := v.Compile([]byte(`{not json`))
	if err == nil {
		t.Fatal("expected error compiling malformed JSON, got nil")
	}
}

func TestValidateNonObject(t *testing.T) {
	v := NewValidator()
	s, err := v.Compile([]byte(`{"type": "string"}`))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	errs, err := s.Validate(42) // integer, not string
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(errs) == 0 {
		t.Fatal("expected validation error for int vs string schema")
	}
}

func TestValidationErrorFields(t *testing.T) {
	ve := ValidationError{Path: "/foo", Message: "bad type"}
	if ve.Path != "/foo" {
		t.Errorf("Path = %q, want /foo", ve.Path)
	}
	if ve.Message != "bad type" {
		t.Errorf("Message = %q, want \"bad type\"", ve.Message)
	}
}

func TestValidateNilDoc(t *testing.T) {
	v := NewValidator()
	s, err := v.Compile([]byte(`{"type": "object"}`))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	// nil should fail type "object".
	errs, err := s.Validate(nil)
	if err != nil {
		t.Fatalf("Validate(nil): unexpected err %v", err)
	}
	if len(errs) == 0 {
		t.Fatal("expected errors for nil vs object schema")
	}
}

func TestRecompileSeparateSchemas(t *testing.T) {
	v := NewValidator()
	s1, err := v.Compile([]byte(`{"type": "string"}`))
	if err != nil {
		t.Fatalf("Compile s1: %v", err)
	}
	s2, err := v.Compile([]byte(`{"type": "integer"}`))
	if err != nil {
		t.Fatalf("Compile s2: %v", err)
	}

	// "hello" is valid for s1 but not s2.
	errs, _ := s1.Validate("hello")
	if len(errs) != 0 {
		t.Errorf("s1.Validate(hello): expected no errors, got %d", len(errs))
	}
	errs, _ = s2.Validate("hello")
	if len(errs) == 0 {
		t.Errorf("s2.Validate(hello): expected errors, got none")
	}
}

// TestConcurrentCompile verifies that Compile is safe under concurrent use.
// The underlying jsonschema.Compiler is not goroutine-safe; this test guards
// the mutex fix. Run with -race.
func TestConcurrentCompile(t *testing.T) {
	v := NewValidator()

	schemas := []string{
		`{"type": "string"}`,
		`{"type": "integer"}`,
		`{"type": "object", "properties": {"x": {"type": "string"}}}`,
		`{"type": "array", "items": {"type": "boolean"}}}`,
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sch := schemas[idx%len(schemas)]
			for j := 0; j < 10; j++ {
				_, err := v.Compile([]byte(sch))
				if err != nil {
					t.Errorf("Compile: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestFileRefRejected verifies that a $ref pointing to a local file path
// (e.g. /etc/passwd) does NOT read the file contents but instead errors.
// This prevents information leakage via malicious schemas.
func TestFileRefRejected(t *testing.T) {
	v := NewValidator()

	// A $ref to a filesystem path should fail compilation, not read the file.
	_, err := v.Compile([]byte(`{"$ref": "/etc/passwd"}`))
	if err == nil {
		t.Fatal("expected error compiling schema with $ref to /etc/passwd, got nil")
	}
	if !strings.Contains(err.Error(), "external") && !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("error should mention external ref disabled, got: %v", err)
	}
}

// TestHTTPRefRejected verifies that a $ref pointing to an HTTP URL is also
// rejected (no network access during compilation).
func TestHTTPRefRejected(t *testing.T) {
	v := NewValidator()

	_, err := v.Compile([]byte(`{"$ref": "https://evil.example.com/schema.json"}`))
	if err == nil {
		t.Fatal("expected error compiling schema with HTTP $ref, got nil")
	}
}

// --- helpers ---

func anyMessage(errs []ValidationError, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e.Message, substr) {
			return true
		}
	}
	return false
}

func anyPath(errs []ValidationError, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e.Path, substr) {
			return true
		}
	}
	return false
}
