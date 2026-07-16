package har_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stunt-adapters/stunt/internal/adapter"
	"github.com/stunt-adapters/stunt/internal/contrib/har"
)

// harJSON returns a minimal HAR 1.2 with two entries: GET /users and
// POST /users. The GET response contains a JSON body with a known real
// value ("John Doe") that must NOT appear in the output.
func harJSON() []byte {
	return []byte(`{
  "log": {
    "version": "1.2",
    "entries": [
      {
        "request": {
          "method": "GET",
          "url": "https://api.example.com/users"
        },
        "response": {
          "status": 200,
          "content": {
            "mimeType": "application/json",
            "text": "{\"id\":\"real-user-42\",\"name\":\"John Doe\",\"email\":\"john@example.com\",\"age\":30,\"active\":true}"
          }
        }
      },
      {
        "request": {
          "method": "POST",
          "url": "https://api.example.com/users"
        },
        "response": {
          "status": 201,
          "content": {
            "mimeType": "application/json",
            "text": "{\"id\":\"real-user-99\",\"name\":\"Jane Smith\",\"created\":true}"
          }
        }
      }
    ]
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

func TestImportHARCreatesEndpoints(t *testing.T) {
	dir := t.TempDir()
	writeAdapterYAML(t, dir, "har-test")

	if err := har.Import(harJSON(), dir); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// Endpoint files for both method+path combos.
	for _, name := range []string{"get_users.yaml", "post_users.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, "endpoints", name)); err != nil {
			t.Errorf("expected endpoint file %s: %v", name, err)
		}
	}

	// Template/fixture files.
	for _, name := range []string{"get_users.json", "post_users.json"} {
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

func TestImportHARSyntheticNoRealData(t *testing.T) {
	dir := t.TempDir()
	writeAdapterYAML(t, dir, "har-synthetic")

	if err := har.Import(harJSON(), dir); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// Read the get_users template — real values must NOT appear.
	tmpl, err := os.ReadFile(filepath.Join(dir, "templates", "get_users.json"))
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	s := string(tmpl)

	// Known real values from the HAR that must be scrubbed.
	for _, real := range []string{"John Doe", "john@example.com", "real-user-42"} {
		if strings.Contains(s, real) {
			t.Errorf("template should not contain real value %q", real)
		}
	}

	// Faker expressions should be present.
	if !strings.Contains(s, "faker") {
		t.Error("template should contain faker expressions")
	}
	// Integer sentinel should have been replaced.
	if strings.Contains(s, "STUNT_INT") {
		t.Error("integer sentinel was not replaced")
	}
	// Integer values should have faker.Int expressions.
	if !strings.Contains(s, "faker.Int") {
		t.Error("template should contain faker.Int for integer values")
	}
}

func TestImportHARResponseStatusPreserved(t *testing.T) {
	dir := t.TempDir()
	writeAdapterYAML(t, dir, "har-status")

	if err := har.Import(harJSON(), dir); err != nil {
		t.Fatalf("Import: %v", err)
	}

	a, err := adapter.Load(dir)
	if err != nil {
		t.Fatalf("adapter.Load: %v", err)
	}

	// The POST /users endpoint should have status 201.
	for _, ep := range a.Endpoints {
		if ep.Route == "/users" && ep.Method == "POST" {
			if len(ep.Rules) == 0 {
				t.Fatal("no rules for POST /users")
			}
			if ep.Rules[0].Respond.Status != 201 {
				t.Errorf("POST /users status = %d, want 201", ep.Rules[0].Respond.Status)
			}
			return
		}
	}
	t.Fatal("POST /users endpoint not found")
}
