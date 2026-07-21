// Package contrib implements the contributor workflow for building stunt
// adapters. Scaffold creates a synthetic adapter skeleton (adapter.yaml +
// convention directories) so a new adapter has the full layout from day one.
//
// All scaffolded content is SYNTHETIC — no real API data is included. This
// is the safe default the tool ships with.
package contrib

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ScaffoldOptions controls scaffold behavior.
type ScaffoldOptions struct {
	// Force allows writing into an existing non-empty directory.
	Force bool
}

// Scaffold creates an adapter skeleton at <dir>/<name>/.
//
// The skeleton includes adapter.yaml, an example endpoint, a sample
// template, a seed fixture, a Starlark handler, a JSON schema, and a
// README explaining the layout. Everything is synthetic.
//
// If the target directory already exists and is non-empty, Scaffold
// returns an error unless opts.Force is true.
func Scaffold(dir, name string, opts ScaffoldOptions) error {
	if name == "" {
		return fmt.Errorf("scaffold: adapter name is required")
	}

	target := filepath.Join(dir, name)

	if !opts.Force {
		if err := refuseNonEmpty(target); err != nil {
			return err
		}
	}

	for rel, content := range scaffoldFiles(name) {
		full := filepath.Join(target, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("scaffold: mkdir %s: %w", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return fmt.Errorf("scaffold: write %s: %w", rel, err)
		}
	}
	return nil
}

// refuseNonEmpty returns an error if dir exists and contains entries.
// A non-existent or empty directory is fine.
func refuseNonEmpty(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // doesn't exist yet — fine
		}
		return fmt.Errorf("scaffold: read %s: %w", dir, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("scaffold: %s already exists and is not empty (use --force to overwrite)", dir)
	}
	return nil
}

// scaffoldFiles returns the relative-path → content map for the adapter
// skeleton. The name parameter personalizes adapter.yaml and README.md.
func scaffoldFiles(name string) map[string]string {
	display := humanize(name)
	return map[string]string{
		"adapter.yaml":              adapterYAML(name, display),
		"README.md":                 readme(display),
		"endpoints/hello.yaml":      endpointHello,
		"templates/hello.json":      templateHello,
		"fixtures/seed.jsonl":       seedJSONL,
		"scripts/hello.star":        scriptHello,
		"schemas/hello.schema.json": schemaHello,
	}
}

// --- file content ---

func adapterYAML(id, display string) string {
	return fmt.Sprintf(`# stunt adapter manifest
# Docs: https://stuntapi.com/stunt
id: %s
name: %s
version: "0.1.0"

# The real upstream API this adapter simulates. Update these to match the
# real API's name and the specific version you are reproducing.
api:
  name: "Example API"
  version: "v1"
real_hosts:
  - api.example.com

# Endpoints are declared inline here (in the endpoints: list below).
endpoints:
  - route: /hello
    method: GET
    rules:
      - name: hello-ok
        match: { method: GET, path: /hello }
        respond:
          status: 200
          headers:
            Content-Type: application/json
          body:
            inline:
              message: hello from stunt

# Backing stores available to Starlark handlers via store_collection/store_kv.
resources:
  - name: items
    kind: collection
    seed: fixtures/seed.jsonl

# Auth scheme placeholder (no behavior yet).
identity:
  token_scheme: bearer

# Top-level rules apply to all routes not matched by an endpoint.
rules:
  - name: catchall-404
    match: { path: "/**" }
    respond:
      status: 404
      body:
        inline:
          error: not_found
`, id, display)
}

func readme(display string) string {
	return fmt.Sprintf(`# %s adapter

A stunt adapter for simulating the **%s** API locally.
All data is synthetic — no real API data is included.

## Layout

    adapter.yaml              Manifest: metadata, endpoints, resources, rules
    endpoints/                Per-endpoint definition files (reference copies)
      hello.yaml              Example GET /hello endpoint
    templates/                Response templates (text/template with faker/uuid)
      hello.json              Sample template using faker.Email and uuid
    fixtures/                 Seed data (JSONL, one JSON object per line)
      seed.jsonl              Synthetic seed for the "items" collection
    scripts/                  Starlark handlers for stateful logic
      hello.star              Example handler using store_collection
    schemas/                  JSON Schema definitions
      hello.schema.json       Schema for the /hello response

## Getting started

1. Edit adapter.yaml to set your id, name, and real_hosts.
2. Add endpoints to the endpoints: list in adapter.yaml.
3. Run stunt up to start the simulator.

## Convention directories

    endpoints/   Per-endpoint YAML definitions (reference; add endpoints inline in adapter.yaml)
    templates/   Response templates (text/template + faker/uuid)
    fixtures/    Seed data (JSONL)
    scripts/     Starlark handlers for stateful logic
    schemas/     JSON Schema definitions
`, display, display)
}

const endpointHello = `# Example standalone endpoint definition file.
# This file is for reference — it mirrors the /hello endpoint declared inline
# in adapter.yaml. stunt currently loads endpoints from the inline endpoints:
# list in adapter.yaml. Keep this file as a template when you want to add
# more endpoints inline.
route: /hello
method: GET
rules:
  - name: hello-ok
    match: { method: GET, path: /hello }
    respond:
      status: 200
      headers:
        Content-Type: application/json
      body:
        inline:
          message: hello from stunt
`

const templateHello = `{
  "id": "{{ uuid }}",
  "message": "hello from stunt",
  "email": "{{ faker.Email }}",
  "timestamp": "{{ now.Format "2006-01-02T15:04:05Z07:00" }}"
}
`

const seedJSONL = `{"id":"item-001","name":"Sample Item Alpha","status":"active"}
{"id":"item-002","name":"Sample Item Beta","status":"active"}
{"id":"item-003","name":"Sample Item Gamma","status":"archived"}
`

const scriptHello = `# Sample Starlark handler demonstrating store_collection usage.
#
# Lists all items from the "items" collection (seeded from
# fixtures/seed.jsonl) and returns them as JSON.
#
# To use this handler, add it to an endpoint in adapter.yaml:
#   handler: scripts/hello.star#on_get

def on_get(req):
    items = store_collection("items")
    docs = items.list()
    return respond(200, {"items": docs, "count": len(docs)})
`

const schemaHello = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "HelloResponse",
  "type": "object",
  "properties": {
    "message": { "type": "string" }
  },
  "required": ["message"]
}
`

// humanize converts an adapter id (e.g. "my-cool-api") into a human-readable
// display name (e.g. "My Cool Api").
func humanize(name string) string {
	s := strings.NewReplacer("-", " ", "_", " ").Replace(name)
	parts := strings.Fields(s)
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}
