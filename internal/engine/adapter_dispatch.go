package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/stunt-adapters/stunt/internal/adapter"
	"github.com/stunt-adapters/stunt/internal/manifest"
	"github.com/stunt-adapters/stunt/internal/rules"
	"github.com/stunt-adapters/stunt/internal/starlark"
)

// dispatchAdapter attempts to handle a request via the adapter's endpoints.
// It first tries to match a handler-backed endpoint (Starlark). If no handler
// endpoint matches, it falls through to the rules engine over endpoint rules,
// service-level rules, and adapter top-level rules.
//
// Returns true if the request was fully handled (a response was written).
// Returns false if nothing matched, so the caller can try rules-only dispatch.
func (e *Engine) dispatchAdapter(
	w http.ResponseWriter,
	r *http.Request,
	st *serviceState,
	body []byte,
	rng *rules.RNG,
	fk *rules.Faker,
	baseDir string,
	serviceRules []rules.Rule,
	rulesMu *sync.Mutex,
) bool {
	a := st.adapter

	// 1. Try handler-backed endpoints first.
	for _, ep := range a.Endpoints {
		if ep.Handler == "" {
			continue
		}
		if !methodMatches(ep.Method, r.Method) {
			continue
		}
		params, ok := matchRoute(ep.Route, r.URL.Path)
		if !ok {
			continue
		}
		e.runHandler(w, r, st, ep, body, params)
		return true
	}

	// 2. Build the combined rules list and evaluate.
	//    Order: matched endpoint rules → service rules overlay → adapter rules.
	combined := combinedRules(a, serviceRules, r.Method, r.URL.Path)
	if len(combined) > 0 {
		req := rules.Request{Method: r.Method, Path: r.URL.Path, Headers: headerMap(r.Header), Body: body}
		rulesMu.Lock()
		d := rules.Evaluate(req, combined, rng, fk, baseDir)
		rulesMu.Unlock()
		if d.Matched {
			applyDecision(w, r, d)
			return true
		}
	}

	return false // nothing matched — caller will 404
}

// combinedRules assembles the rule list for rules-based dispatch: endpoint
// rules (from endpoints without handlers that match the request), service
// overlay rules, then adapter top-level rules.
func combinedRules(a *adapter.Adapter, serviceRules []rules.Rule, method, path string) []rules.Rule {
	var out []rules.Rule

	// Endpoint rules for rules-only endpoints matching this request.
	for _, ep := range a.Endpoints {
		if ep.Handler != "" {
			continue
		}
		if methodMatches(ep.Method, method) {
			if _, ok := matchRoute(ep.Route, path); ok {
				out = append(out, ep.Rules...)
			}
		}
	}

	// Service-level rules overlay.
	out = append(out, serviceRules...)

	// Adapter top-level rules (catch-all, etc.).
	out = append(out, a.Rules...)

	return out
}

// runHandler loads (or retrieves cached) the Starlark VM for the endpoint's
// handler script, invokes the handler function, and writes the response.
func (e *Engine) runHandler(
	w http.ResponseWriter,
	r *http.Request,
	st *serviceState,
	ep adapter.Endpoint,
	body []byte,
	params map[string]string,
) {
	scriptPath, fnName := adapter.SplitHandler(ep.Handler)

	vm, err := st.getOrLoadVM(scriptPath)
	if err != nil {
		writeError(w, 500, fmt.Sprintf("failed to load handler script: %v", err))
		return
	}

	var bodyMap map[string]any
	if len(body) > 0 {
		ct := r.Header.Get("Content-Type")
		if isFormContentType(ct) {
			bodyMap = parseFormBody(string(body))
		} else if err := json.Unmarshal(body, &bodyMap); err != nil {
			// Try parsing as a JSON array (e.g., JSON-RPC batch requests).
			// If it parses, wrap under a reserved key so the handler can
			// detect and process batch bodies.
			var bodyList []any
			if err2 := json.Unmarshal(body, &bodyList); err2 == nil {
				bodyMap = map[string]any{"_batch": bodyList}
			} else {
				bodyMap = nil // non-JSON body; handler gets empty body
			}
		}
	}

	req := starlark.Request{
		Method:  r.Method,
		Path:    r.URL.Path,
		Headers: headerMap(r.Header),
		Body:    bodyMap,
		Params:  params,
		Query:   queryMap(r.URL.Query()),
	}

	resp, err := vm.Call(fnName, req)
	if err != nil {
		writeError(w, 500, fmt.Sprintf("handler error: %v", err))
		return
	}

	// Write headers.
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}

	status := resp.Status
	if status == 0 {
		status = 200
	}

	// Raw text body takes precedence over JSON body — used for content
	// download endpoints (e.g., alt=media) that return raw file content.
	if resp.RawBody != "" {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "text/plain")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp.RawBody))
		return
	}

	// JSON array body (for endpoints returning a bare array, e.g. Discord
	// GET /channels/{id}/messages).
	if resp.BodyList != nil {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		data, err := json.Marshal(resp.BodyList)
		if err != nil {
			writeError(w, 500, fmt.Sprintf("marshal response body list: %v", err))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(data)
		return
	}

	// Default content type for JSON bodies.
	if resp.Body != nil && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}

	// Marshal the body BEFORE writing the header so that a marshal failure
	// produces a clean 500 error instead of a superfluous WriteHeader (I3).
	var respBody []byte
	if resp.Body != nil {
		data, err := json.Marshal(resp.Body)
		if err != nil {
			writeError(w, 500, fmt.Sprintf("marshal response body: %v", err))
			return
		}
		respBody = data
	}

	w.WriteHeader(status)
	if respBody != nil {
		_, _ = w.Write(respBody)
	}
}

// getOrLoadVM returns the cached VM for scriptPath, or loads it on first use.
// If a lib.star exists in the same directory as the handler script, its
// top-level definitions are preloaded and made available to the handler as
// shared-library helpers.
func (st *serviceState) getOrLoadVM(scriptPath string) (*starlark.VM, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if vm, ok := st.vms[scriptPath]; ok {
		return vm, nil
	}

	src, err := os.ReadFile(scriptPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", scriptPath, err)
	}

	// Check for a lib.star in the same directory as the handler script.
	// If present, its top-level defs are injected as predeclared globals.
	libPath := filepath.Join(filepath.Dir(scriptPath), "lib.star")
	var libSrc string
	if libData, err := os.ReadFile(libPath); err == nil {
		libSrc = string(libData)
	}

	vm, err := starlark.LoadWithLib(string(src), libSrc, st.builtins)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", scriptPath, err)
	}

	st.vms[scriptPath] = vm
	return vm, nil
}

// --- route matching ---

// matchRoute matches a path against a route pattern. The pattern may contain
// {param} segments which match exactly one path segment and capture its value.
// Returns the captured params and true on match, or nil/false on mismatch.
//
// Examples:
//
//	/charges               matches /charges
//	/charges/{id}          matches /charges/abc123 (params={id:abc123})
//	/charges/{id}/refund   matches /charges/abc123/refund
func matchRoute(pattern, path string) (map[string]string, bool) {
	patSegs := splitPathSegments(pattern)
	pathSegs := splitPathSegments(path)
	if len(patSegs) != len(pathSegs) {
		return nil, false
	}
	params := map[string]string{}
	for i, ps := range patSegs {
		if len(ps) >= 2 && ps[0] == '{' && ps[len(ps)-1] == '}' {
			name := ps[1 : len(ps)-1]
			params[name] = pathSegs[i]
		} else if ps != pathSegs[i] {
			return nil, false
		}
	}
	return params, true
}

// splitPathSegments splits a path on '/', trimming leading/trailing slashes.
func splitPathSegments(s string) []string {
	s = strings.Trim(s, "/")
	if s == "" {
		return nil
	}
	return strings.Split(s, "/")
}

// methodMatches reports whether the endpoint method accepts the request method.
// An empty endpoint method matches any method.
func methodMatches(epMethod, reqMethod string) bool {
	if epMethod == "" {
		return true
	}
	return strings.EqualFold(epMethod, reqMethod)
}

// queryMap converts a url.Values into a map[string]string taking the first
// value of each key. This mirrors how headerMap handles multi-value headers.
func queryMap(v url.Values) map[string]string {
	if len(v) == 0 {
		return nil
	}
	out := make(map[string]string, len(v))
	for k, vals := range v {
		if len(vals) > 0 {
			out[k] = vals[0]
		}
	}
	return out
}

// defaultStateDir returns the directory for per-service SQLite databases.
// It is derived from the manifest location: <manifest-dir>/.stunt/state/.
func defaultStateDir(m *manifest.Manifest) string {
	dir := filepath.Dir(m.Path)
	return filepath.Join(dir, ".stunt", "state")
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	data, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(data)
}

// isFormContentType reports whether ct is an HTML form content type
// (application/x-www-form-urlencoded), optionally followed by parameters
// such as charset.
func isFormContentType(ct string) bool {
	ct = strings.TrimSpace(strings.ToLower(ct))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return ct == "application/x-www-form-urlencoded"
}

// parseFormBody parses a URL-encoded form body
// (key=value&key2=value2) into a map[string]any suitable for the Starlark
// handler's req["body"].
func parseFormBody(raw string) map[string]any {
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return nil
	}
	out := make(map[string]any, len(vals))
	for k, vs := range vals {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
