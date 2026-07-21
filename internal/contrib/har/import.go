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
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"stuntapi.com/stunt/internal/adapter"
	"stuntapi.com/stunt/internal/contrib"
	"stuntapi.com/stunt/internal/rules"
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

		// Parameterize the path so real IDs/tokens in the recorded URL never
		// leak into routes, match paths, or filenames.
		paramPath := parameterizePath(k.path)
		name := contrib.SafeName(k.method, paramPath)
		matchPath := contrib.GlobPath(paramPath)

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
			Route:  paramPath,
			Method: k.method,
			Rules: []rules.Rule{{
				Name:  name + "-ok",
				Match: rules.Match{Method: k.method, Path: matchPath},
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
// path parameterization
// ---------------------------------------------------------------------------

// reNumericSegment matches a purely numeric path segment (e.g. "42").
var reNumericSegment = regexp.MustCompile(`^[0-9]+$`)

// reUUIDSegment matches canonical UUIDs (8-4-4-4-12 hex).
var reUUIDSegment = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// reLongAlphaNum matches long alphanumeric strings (16+ chars) that are
// characteristic of opaque API IDs/tokens (e.g. "1a2b3c4d5e6f7a8b9c").
var reLongAlphaNum = regexp.MustCompile(`^[A-Za-z0-9]{16,}$`)

// reVersionSegment matches simple version path segments like "v1", "v2".
var reVersionSegment = regexp.MustCompile(`(?i)^v[0-9]+$`)

// reHasLetter / reHasDigit test for the presence of at least one letter or
// digit respectively. Used to detect mixed alphanumeric IDs.
var reHasLetter = regexp.MustCompile(`[A-Za-z]`)
var reHasDigit = regexp.MustCompile(`[0-9]`)

// providerPrefixes are known provider-specific ID prefixes. A path segment
// starting with one of these (followed by alphanumeric content) is treated as
// a real ID.
var providerPrefixes = []string{
	"cus_", "ch_", "pi_", "sub_", "txn_", "acct_", // Stripe (partial)
	"ghp_", "gho_", "ghu_", "ghs_", // GitHub tokens
	"sk_", "rk_", // Stripe secret/restricted keys
	"AKIA", // AWS access key IDs
	"xox",  // Slack tokens (xox[bpoa]-)
	"AIza", // Google API keys
}

// looksLikeID reports whether a single path segment looks like a real
// identifier/token that should be parameterized rather than embedded verbatim
// in a route or filename. Returns true for numeric IDs, UUIDs, long opaque
// alphanumeric strings, known provider-prefixed IDs, and mixed alphanumeric
// segments that are not simple version segments (v1, v2).
func looksLikeID(seg string) bool {
	if seg == "" {
		return false
	}
	// Pure numeric segment (e.g. "42", "12345").
	if reNumericSegment.MatchString(seg) {
		return true
	}
	// UUID segment.
	if reUUIDSegment.MatchString(seg) {
		return true
	}
	// Known provider-prefixed ID.
	for _, p := range providerPrefixes {
		if strings.HasPrefix(seg, p) && len(seg) > len(p) {
			return true
		}
	}
	// Long opaque alphanumeric string (16+ chars, no separators).
	// This catches opaque API IDs/tokens without known prefixes.
	if reLongAlphaNum.MatchString(seg) {
		return true
	}
	// Mixed alphanumeric segment (contains both letters and digits) that is
	// not a simple version segment like "v1". This catches IDs like
	// "real-user-42", "item-001", "abc123", etc.
	if reHasLetter.MatchString(seg) && reHasDigit.MatchString(seg) && !reVersionSegment.MatchString(seg) {
		return true
	}
	return false
}

// parameterizePath replaces path segments that look like real IDs/tokens with
// a generic {id} placeholder. Segments that are already {param} placeholders
// (e.g. from OpenAPI) are preserved. This ensures real values in recorded URLs
// never leak into routes, match paths, or filenames.
//
// Example:
//
//	/users/real-user-42/orders    -> /users/{id}/orders
//	/v1/charges/ch_realtoken      -> /v1/charges/{id}
//	/users/550e8400-.../profile   -> /users/{id}/profile
//	/users/{userId}/orders        -> /users/{userId}/orders  (preserved)
func parameterizePath(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, seg := range segs {
		// Preserve existing OpenAPI-style placeholders.
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			continue
		}
		if looksLikeID(seg) {
			segs[i] = "{id}"
		}
	}
	result := "/" + strings.Join(segs, "/")
	// Preserve trailing slash from original path (rare but deterministic).
	if strings.HasSuffix(path, "/") && result != "/" {
		result += "/"
	}
	return result
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
