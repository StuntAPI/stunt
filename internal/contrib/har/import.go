// Package har imports HAR 1.2 files into stunt adapters.
//
// The importer parses entries → request{method,url} + response{status,
// content{mimeType,text}}. For each unique (method, pathname) an endpoint is
// inferred and a synthetic fixture generated. Real response values are walked
// and replaced with faker template expressions — no real data is copied.
package har

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/stunt-adapters/stunt/internal/adapter"
	"github.com/stunt-adapters/stunt/internal/contrib"
	"github.com/stunt-adapters/stunt/internal/rules"
	"gopkg.in/yaml.v3"
)

// Import parses a HAR 1.2 file and generates stunt adapter endpoint files,
// template files, and updates adapter.yaml. All response values are
// synthesized — no real data is copied through.
func Import(harBytes []byte, dir string) error {
	doc, err := parseHAR(harBytes)
	if err != nil {
		return err
	}

	// Deduplicate by (method, pathname) — last entry wins.
	type key struct{ method, path string }
	seen := make(map[key]harEntry)

	for _, entry := range doc.Log.Entries {
		u, err := url.Parse(entry.Request.URL)
		if err != nil || u.Path == "" {
			continue
		}
		method := strings.ToUpper(entry.Request.Method)
		if method == "" {
			continue
		}
		k := key{method, u.Path}
		seen[k] = entry
	}

	// Sort keys for deterministic output.
	keys := make([]key, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].path != keys[j].path {
			return keys[i].path < keys[j].path
		}
		return keys[i].method < keys[j].method
	})

	var endpoints []adapter.Endpoint
	for _, k := range keys {
		entry := seen[k]
		name := contrib.SafeName(k.method, k.path)

		tmpl := synthesizeBody(entry.Response.Content)

		// Write template file.
		if err := contrib.WriteAdapterFile(dir, "templates/"+name+".json", tmpl); err != nil {
			return err
		}

		status := entry.Response.Status
		if status == 0 {
			status = 200
		}

		ep := adapter.Endpoint{
			Route:  k.path,
			Method: k.method,
			Rules: []rules.Rule{{
				Name:  name + "-ok",
				Match: rules.Match{Method: k.method, Path: k.path},
				Respond: rules.Respond{
					Status:  status,
					Headers: map[string]string{"Content-Type": "application/json"},
					Body:    &rules.Body{Template: tmpl},
				},
			}},
		}
		endpoints = append(endpoints, ep)

		// Write endpoint YAML mirror.
		epYAML, err := yaml.Marshal(&ep)
		if err != nil {
			return fmt.Errorf("har: marshal endpoint %s: %w", name, err)
		}
		if err := contrib.WriteAdapterFile(dir, "endpoints/"+name+".yaml", string(epYAML)); err != nil {
			return err
		}
	}

	if len(endpoints) == 0 {
		return nil
	}
	return contrib.MergeEndpoints(dir, endpoints)
}

// ---------------------------------------------------------------------------
// body synthesis
// ---------------------------------------------------------------------------

// intSentinel is a valid-JSON placeholder replaced post-marshal with an
// unquoted {{ faker.Int }} expression. It avoids special characters that
// json.MarshalIndent would escape.
const intSentinel = "@@STUNT_INT@@"

// synthesizeBody takes a HAR content object and returns a synthetic JSON
// template string. If the body is valid JSON, all values are replaced with
// faker expressions while preserving the structure.
func synthesizeBody(c harContent) string {
	if c.Text == "" {
		return "{\n  \"message\": \"{{ faker.Word }}\"\n}\n"
	}

	// Try to parse as JSON; if it fails, return a generic synthetic body.
	var v any
	if err := json.Unmarshal([]byte(c.Text), &v); err != nil {
		return "{\n  \"message\": \"{{ faker.Word }}\"\n}\n"
	}

	synth := synthesizeValue(v)
	data, err := json.MarshalIndent(synth, "", "  ")
	if err != nil {
		return "{\n  \"message\": \"{{ faker.Word }}\"\n}\n"
	}
	result := string(data)
	result = strings.ReplaceAll(result, "\""+intSentinel+"\"", "{{ faker.Int 1 999 }}")
	return result + "\n"
}

// synthesizeValue recursively walks a parsed JSON value and replaces all
// leaf values with synthetic faker expressions, preserving structure.
func synthesizeValue(v any) any {
	switch val := v.(type) {
	case string:
		return "{{ faker.Word }}"
	case float64:
		return intSentinel
	case bool:
		return false
	case map[string]any:
		result := make(map[string]any, len(val))
		for k, child := range val {
			result[k] = synthesizeValue(child)
		}
		return result
	case []any:
		result := make([]any, len(val))
		for i, child := range val {
			result[i] = synthesizeValue(child)
		}
		return result
	case nil:
		return nil
	default:
		return "{{ faker.Word }}"
	}
}

// ---------------------------------------------------------------------------
// HAR parsing types
// ---------------------------------------------------------------------------

type harFile struct {
	Log harLog `yaml:"log" json:"log"`
}

type harLog struct {
	Entries []harEntry `yaml:"entries" json:"entries"`
}

type harEntry struct {
	Request  harRequest  `yaml:"request" json:"request"`
	Response harResponse `yaml:"response" json:"response"`
}

type harRequest struct {
	Method string `yaml:"method" json:"method"`
	URL    string `yaml:"url" json:"url"`
}

type harResponse struct {
	Status  int        `yaml:"status" json:"status"`
	Content harContent `yaml:"content" json:"content"`
}

type harContent struct {
	MimeType string `yaml:"mimeType" json:"mimeType"`
	Text     string `yaml:"text" json:"text"`
}

// parseHAR unmarshals HAR bytes (JSON) into a harFile.
func parseHAR(data []byte) (*harFile, error) {
	var doc harFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("har: parse: %w", err)
	}
	return &doc, nil
}
