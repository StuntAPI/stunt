// Package openapi imports OpenAPI 3.x specifications into stunt adapters.
//
// The importer uses a minimal hand-rolled parser (no heavy third-party
// dependency) that reads paths → operations → 2xx response → JSON schema.
// For each operation it emits a synthetic endpoint with a template body whose
// placeholder values are derived from the response schema by type:
//
//	string  → {{ faker.Word }}
//	integer → {{ faker.Int 1 999 }}
//	boolean → false
//	object  → recurse
//	array   → recurse on items
//
// All generated content is SYNTHETIC — no real API data is included.
package openapi

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/stunt-adapters/stunt/internal/adapter"
	"github.com/stunt-adapters/stunt/internal/contrib"
	"github.com/stunt-adapters/stunt/internal/rules"
	"gopkg.in/yaml.v3"
)

// Import parses an OpenAPI 3.x spec (JSON or YAML) and generates stunt adapter
// endpoint files, template files, and updates adapter.yaml. All generated
// response bodies are synthetic.
func Import(specBytes []byte, dir string) error {
	spec, err := parseSpec(specBytes)
	if err != nil {
		return err
	}

	var endpoints []adapter.Endpoint
	for _, path := range spec.sortedPaths() {
		item := spec.Paths[path]
		for _, op := range item.operations() {
			name := contrib.SafeName(op.method, path)
			matchPath := contrib.GlobPath(path)

			tmpl := schemaToTemplate(op.op.firstSchema())

			// Write template file.
			if err := contrib.WriteAdapterFile(dir, "templates/"+name+".json", tmpl); err != nil {
				return err
			}

			ep := buildEndpoint(name, path, op.method, matchPath, tmpl)
			endpoints = append(endpoints, ep)

			// Write endpoint YAML mirror.
			epYAML, err := yaml.Marshal(&ep)
			if err != nil {
				return fmt.Errorf("openapi: marshal endpoint %s: %w", name, err)
			}
			if err := contrib.WriteAdapterFile(dir, "endpoints/"+name+".yaml", string(epYAML)); err != nil {
				return err
			}
		}
	}

	if len(endpoints) == 0 {
		return nil
	}
	return contrib.MergeEndpoints(dir, endpoints)
}

// buildEndpoint creates an adapter.Endpoint with a 200 rule whose body is the
// given template.
func buildEndpoint(name, route, method, matchPath, tmpl string) adapter.Endpoint {
	return adapter.Endpoint{
		Route:  route,
		Method: method,
		Rules: []rules.Rule{{
			Name:  name + "-ok",
			Match: rules.Match{Method: method, Path: matchPath},
			Respond: rules.Respond{
				Status:  200,
				Headers: map[string]string{"Content-Type": "application/json"},
				Body:    &rules.Body{Template: tmpl},
			},
		}},
	}
}

// ---------------------------------------------------------------------------
// minimal OpenAPI parser
// ---------------------------------------------------------------------------

type openapiSpec struct {
	Paths map[string]*pathItem `yaml:"paths"`
}

// sortedPaths returns paths in lexicographic order for deterministic output.
func (s *openapiSpec) sortedPaths() []string {
	keys := make([]string, 0, len(s.Paths))
	for k := range s.Paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type pathItem struct {
	Get     *operation `yaml:"get,omitempty"`
	Post    *operation `yaml:"post,omitempty"`
	Put     *operation `yaml:"put,omitempty"`
	Delete  *operation `yaml:"delete,omitempty"`
	Patch   *operation `yaml:"patch,omitempty"`
	Head    *operation `yaml:"head,omitempty"`
	Options *operation `yaml:"options,omitempty"`
}

type opEntry struct {
	method string
	op     *operation
}

// operations returns all non-nil HTTP methods in a canonical order.
func (p *pathItem) operations() []opEntry {
	entries := []struct {
		name string
		op   *operation
	}{
		{"GET", p.Get},
		{"POST", p.Post},
		{"PUT", p.Put},
		{"DELETE", p.Delete},
		{"PATCH", p.Patch},
		{"HEAD", p.Head},
		{"OPTIONS", p.Options},
	}
	var out []opEntry
	for _, e := range entries {
		if e.op != nil {
			out = append(out, opEntry{e.name, e.op})
		}
	}
	return out
}

type operation struct {
	Summary     string               `yaml:"summary"`
	OperationID string               `yaml:"operationId"`
	Responses   map[string]*response `yaml:"responses"`
}

// firstSchema returns the JSON schema of the first 2xx response with JSON
// content, or nil if none is found.
func (o *operation) firstSchema() *jsonSchema {
	if o.Responses == nil {
		return nil
	}
	// Try common 2xx codes in order.
	for _, code := range []string{"200", "201", "202", "204", "203"} {
		if resp, ok := o.Responses[code]; ok && resp != nil {
			if s := resp.jsonSchema(); s != nil {
				return s
			}
		}
	}
	// Fallback: any 2xx (iterate deterministically).
	codes := make([]string, 0, len(o.Responses))
	for c := range o.Responses {
		if strings.HasPrefix(c, "2") {
			codes = append(codes, c)
		}
	}
	sort.Strings(codes)
	for _, code := range codes {
		if resp := o.Responses[code]; resp != nil {
			if s := resp.jsonSchema(); s != nil {
				return s
			}
		}
	}
	return nil
}

type response struct {
	Content map[string]*mediaType `yaml:"content"`
}

// jsonSchema returns the schema from application/json content, or the first
// available content type if JSON is absent.
func (r *response) jsonSchema() *jsonSchema {
	if r.Content == nil {
		return nil
	}
	if mt, ok := r.Content["application/json"]; ok && mt != nil {
		return mt.Schema
	}
	// Fallback: first content type (deterministic order).
	keys := make([]string, 0, len(r.Content))
	for k := range r.Content {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if mt := r.Content[k]; mt != nil && mt.Schema != nil {
			return mt.Schema
		}
	}
	return nil
}

type mediaType struct {
	Schema *jsonSchema `yaml:"schema"`
}

type jsonSchema struct {
	Type       string             `yaml:"type"`
	Properties map[string]*jsonSchema `yaml:"properties"`
	Items      *jsonSchema        `yaml:"items"`
}

// ---------------------------------------------------------------------------
// schema → synthetic template
// ---------------------------------------------------------------------------

// Sentinel placeholder used during JSON marshaling. After marshaling it is
// replaced with the actual unquoted template expression. The sentinel is a
// valid JSON string so json.MarshalIndent succeeds and does not escape it.
const intSentinel = "@@STUNT_INT@@"

// schemaToTemplate produces a pretty-printed JSON string with faker template
// expressions for all leaf values.
func schemaToTemplate(s *jsonSchema) string {
	if s == nil {
		return "{\n  \"message\": \"{{ faker.Word }}\"\n}\n"
	}
	v := schemaValue(s)
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "{\n  \"message\": \"{{ faker.Word }}\"\n}\n"
	}
	result := string(data)
	// Replace the quoted sentinel with an unquoted template expression.
	result = strings.ReplaceAll(result, "\""+intSentinel+"\"", "{{ faker.Int 1 999 }}")
	return result + "\n"
}

// schemaValue converts a JSON schema into a Go value tree where strings are
// faker template expressions (quoted) and objects/arrays recurse. Integer
// values use a sentinel that is replaced post-marshal with an unquoted
// template expression.
func schemaValue(s *jsonSchema) any {
	switch s.Type {
	case "string":
		return "{{ faker.Word }}"
	case "integer", "number":
		return intSentinel
	case "boolean":
		return false
	case "array":
		if s.Items != nil {
			return []any{schemaValue(s.Items)}
		}
		return []any{}
	default: // "object" or unspecified
		if len(s.Properties) == 0 {
			return "{{ faker.Word }}"
		}
		obj := make(map[string]any, len(s.Properties))
		for k, prop := range s.Properties {
			if prop == nil {
				obj[k] = "{{ faker.Word }}"
			} else {
				obj[k] = schemaValue(prop)
			}
		}
		return obj
	}
}

// ---------------------------------------------------------------------------
// parsing
// ---------------------------------------------------------------------------

// parseSpec unmarshals spec bytes (JSON or YAML — JSON is valid YAML) into an
// openapiSpec.
func parseSpec(data []byte) (*openapiSpec, error) {
	var spec openapiSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("openapi: parse spec: %w", err)
	}
	if spec.Paths == nil {
		spec.Paths = map[string]*pathItem{}
	}
	return &spec, nil
}
