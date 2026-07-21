package openapi_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stuntapi.com/stunt/internal/adapter"
	"stuntapi.com/stunt/internal/contrib/openapi"
)

// openapiJSONSpec is a minimal OpenAPI 3.0 spec with two paths/methods.
// GET /users returns an object with string properties.
// GET /users/{userId} returns an object with string, integer, and boolean
// properties.
func openapiJSONSpec() []byte {
	return []byte(`{
  "openapi": "3.0.0",
  "info": {"title": "Test API", "version": "1.0.0"},
  "paths": {
    "/users": {
      "get": {
        "summary": "List users",
        "responses": {
          "200": {
            "content": {
              "application/json": {
                "schema": {
                  "type": "object",
                  "properties": {
                    "id": {"type": "string"},
                    "name": {"type": "string"}
                  }
                }
              }
            }
          }
        }
      }
    },
    "/users/{userId}": {
      "get": {
        "summary": "Get user",
        "responses": {
          "200": {
            "content": {
              "application/json": {
                "schema": {
                  "type": "object",
                  "properties": {
                    "id": {"type": "string"},
                    "name": {"type": "string"},
                    "age": {"type": "integer"},
                    "active": {"type": "boolean"}
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}`)
}

func writeAdapterYAML(t *testing.T, dir, id string) {
	t.Helper()
	content := "id: " + id + "\nname: Test\nversion: \"0.1.0\"\n"
	if err := os.WriteFile(filepath.Join(dir, "adapter.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestImportCreatesEndpointsAndTemplates(t *testing.T) {
	dir := t.TempDir()
	writeAdapterYAML(t, dir, "test-api")

	if err := openapi.Import(openapiJSONSpec(), dir); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// Endpoint files generated.
	for _, name := range []string{"get_users.yaml", "get_users_userid.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, "endpoints", name)); err != nil {
			t.Errorf("expected endpoint file %s: %v", name, err)
		}
	}

	// Template files generated.
	for _, name := range []string{"get_users.json", "get_users_userid.json"} {
		if _, err := os.Stat(filepath.Join(dir, "templates", name)); err != nil {
			t.Errorf("expected template file %s: %v", name, err)
		}
	}

	// adapter.yaml loads and includes imported endpoints.
	a, err := adapter.Load(dir)
	if err != nil {
		t.Fatalf("adapter.Load: %v", err)
	}
	if len(a.Endpoints) < 2 {
		t.Fatalf("Endpoints: %d, want >= 2", len(a.Endpoints))
	}
}

func TestImportSyntheticBodies(t *testing.T) {
	dir := t.TempDir()
	writeAdapterYAML(t, dir, "synthetic-test")

	if err := openapi.Import(openapiJSONSpec(), dir); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// Read the get_users_userid template — it should contain faker expressions
	// for all value types.
	tmpl, err := os.ReadFile(filepath.Join(dir, "templates", "get_users_userid.json"))
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	s := string(tmpl)

	// String property -> faker expression.
	if !strings.Contains(s, "faker") {
		t.Error("template should contain faker expressions for string values")
	}
	// Boolean property -> literal false.
	if !strings.Contains(s, "false") {
		t.Error("template should contain false for boolean values")
	}
	// Integer property -> faker.Int expression (not a sentinel).
	if strings.Contains(s, "STUNT_INT") {
		t.Error("integer sentinel was not replaced with faker expression")
	}
	if !strings.Contains(s, "faker.Int") {
		t.Error("template should contain faker.Int for integer values")
	}
	// No real API data — the spec summary "Get user" should NOT appear as a value.
	if strings.Contains(s, "Get user") {
		t.Error("template should not contain real data from spec")
	}
}

func TestImportYAMLSpec(t *testing.T) {
	dir := t.TempDir()
	writeAdapterYAML(t, dir, "yaml-spec-test")

	spec := []byte(`
openapi: "3.0.0"
info:
  title: YAML Test API
  version: "1.0.0"
paths:
  /products:
    get:
      summary: List products
      responses:
        "200":
          content:
            application/json:
              schema:
                type: object
                properties:
                  name:
                    type: string
`)
	if err := openapi.Import(spec, dir); err != nil {
		t.Fatalf("Import YAML: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "endpoints", "get_products.yaml")); err != nil {
		t.Errorf("expected endpoint file: %v", err)
	}
}

func TestImportParameterizedPathConversion(t *testing.T) {
	dir := t.TempDir()
	writeAdapterYAML(t, dir, "param-test")

	if err := openapi.Import(openapiJSONSpec(), dir); err != nil {
		t.Fatalf("Import: %v", err)
	}

	a, err := adapter.Load(dir)
	if err != nil {
		t.Fatalf("adapter.Load: %v", err)
	}

	// Find the /users/{userId} endpoint and check its match path uses *.
	for _, ep := range a.Endpoints {
		if ep.Route == "/users/{userId}" {
			if len(ep.Rules) == 0 {
				t.Fatal("no rules for parameterized endpoint")
			}
			matchPath := ep.Rules[0].Match.Path
			if matchPath != "/users/*" {
				t.Errorf("match path = %q, want /users/*", matchPath)
			}
			return
		}
	}
	t.Fatal("parameterized endpoint /users/{userId} not found")
}

// --- I1: null path item does not panic ---

func TestImportNullPathItemNoPanic(t *testing.T) {
	dir := t.TempDir()
	writeAdapterYAML(t, dir, "null-path-test")

	// A spec where /users has a valid operation but /products is explicitly null.
	spec := []byte(`{
  "openapi": "3.0.0",
  "info": {"title": "Null Path", "version": "1.0.0"},
  "paths": {
    "/users": {
      "get": {
        "responses": {
          "200": {
            "content": {
              "application/json": {
                "schema": { "type": "object", "properties": { "name": { "type": "string" } } }
              }
            }
          }
        }
      }
    },
    "/products": null
  }
}`)

	// Must not panic.
	if err := openapi.Import(spec, dir); err != nil {
		t.Fatalf("Import with null path item: %v", err)
	}

	// /users endpoint should still be generated.
	if _, err := os.Stat(filepath.Join(dir, "endpoints", "get_users.yaml")); err != nil {
		t.Errorf("expected get_users.yaml endpoint: %v", err)
	}
}

// --- I4: $ref in response schemas is resolved ---

func TestImportResolvesRefResponseSchema(t *testing.T) {
	dir := t.TempDir()
	writeAdapterYAML(t, dir, "ref-test")

	spec := []byte(`{
  "openapi": "3.0.0",
  "info": {"title": "Ref API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "responses": {
          "200": {
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/Pet" }
              }
            }
          }
        }
      }
    }
  },
  "components": {
    "schemas": {
      "Pet": {
        "type": "object",
        "properties": {
          "id": { "type": "integer" },
          "name": { "type": "string" },
          "species": { "type": "string" }
        }
      }
    }
  }
}`)

	if err := openapi.Import(spec, dir); err != nil {
		t.Fatalf("Import: %v", err)
	}

	tmpl, err := os.ReadFile(filepath.Join(dir, "templates", "get_pets.json"))
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	s := string(tmpl)

	// The body should match the referenced schema's structure.
	for _, key := range []string{"id", "name", "species"} {
		if !strings.Contains(s, key) {
			t.Errorf("template should contain key %q from $ref-resolved schema, got: %s", key, s)
		}
	}
	// Should not be a generic fallback body.
	if strings.Contains(s, "message") {
		t.Errorf("template should not be generic fallback, got: %s", s)
	}
}

// --- I4: unresolvable $ref produces generic body, not panic ---

func TestImportUnresolvableRefProducesGenericBody(t *testing.T) {
	dir := t.TempDir()
	writeAdapterYAML(t, dir, "bad-ref-test")

	spec := []byte(`{
  "openapi": "3.0.0",
  "info": {"title": "Bad Ref API", "version": "1.0.0"},
  "paths": {
    "/things": {
      "get": {
        "responses": {
          "200": {
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/NonExistent" }
              }
            }
          }
        }
      }
    }
  }
}`)

	if err := openapi.Import(spec, dir); err != nil {
		t.Fatalf("Import: %v", err)
	}

	tmpl, err := os.ReadFile(filepath.Join(dir, "templates", "get_things.json"))
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	s := string(tmpl)
	// Should produce a generic synthetic body, not panic or crash.
	if !strings.Contains(s, "faker") {
		t.Errorf("template should contain faker expression even for unresolvable ref, got: %s", s)
	}
}
