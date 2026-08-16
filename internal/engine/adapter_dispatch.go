package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"stuntapi.com/stunt/internal/adapter"
	"stuntapi.com/stunt/internal/manifest"
	"stuntapi.com/stunt/internal/rules"
	"stuntapi.com/stunt/internal/starlark"
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
		Host:    r.Host,
		Headers: headerMap(r.Header),
		Body:    bodyMap,
		RawBody: string(body),
		Params:  params,
		Query:   queryMap(r.URL.Query()),
	}

	// Serialize handler calls that share a concurrency key (a path-param name
	// declared on the endpoint) so a handler's read-modify-write across stores
	// runs atomically per key. Released on return, across every early return.
	if ep.ConcurrencyKey != "" {
		release := st.handlerLocks.acquire(params[ep.ConcurrencyKey])
		defer release()
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

// matchRoute matches a path against a route pattern. A pattern segment that is
// exactly {param} matches any single path segment and captures it. A segment
// with an embedded {param} (e.g. accounts({id}), the OData key-in-parens form)
// matches a path segment carrying the literal prefix and suffix and captures
// the middle. All other segments match literally.
// Returns the captured params and true on match, or nil/false on mismatch.
//
// Examples:
//
//	/charges               matches /charges
//	/charges/{id}          matches /charges/abc123 (params={id:abc123})
//	/charges/{id}/refund   matches /charges/abc123/refund
//	/accounts({id})        matches /accounts(abc123) (params={id:abc123})
func matchRoute(pattern, path string) (map[string]string, bool) {
	patSegs := splitPathSegments(pattern)
	pathSegs := splitPathSegments(path)
	if len(patSegs) != len(pathSegs) {
		return nil, false
	}
	params := map[string]string{}
	for i, ps := range patSegs {
		if !matchSegment(ps, pathSegs[i], params) {
			return nil, false
		}
	}
	return params, true
}

// matchSegment matches one path segment against a pattern segment, capturing
// any param into params.
func matchSegment(pat, pathSeg string, params map[string]string) bool {
	// Whole-segment {name}: capture the entire segment.
	if len(pat) >= 2 && pat[0] == '{' && pat[len(pat)-1] == '}' {
		params[pat[1:len(pat)-1]] = pathSeg
		return true
	}
	// No placeholder: literal match.
	if !strings.Contains(pat, "{") {
		return pat == pathSeg
	}
	// Embedded single {name} within a literal segment (e.g. accounts({id})).
	// A segment with multiple placeholders is treated literally (no match here).
	open := strings.Index(pat, "{")
	closeIdx := strings.LastIndex(pat, "}")
	if open < 0 || closeIdx < 0 || closeIdx < open || strings.Count(pat, "{") != 1 {
		return pat == pathSeg
	}
	prefix := pat[:open]
	name := pat[open+1 : closeIdx]
	suffix := pat[closeIdx+1:]
	if !strings.HasPrefix(pathSeg, prefix) || !strings.HasSuffix(pathSeg, suffix) || len(pathSeg) < len(prefix)+len(suffix) {
		return false
	}
	params[name] = pathSeg[len(prefix) : len(pathSeg)-len(suffix)]
	return true
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
		if len(vs) == 0 {
			continue
		}
		// Bracket-notation keys (a[b]=v, a[]=v, a[0][b]=v) expand into nested
		// dicts/lists — provider SDKs (stripe-node, Octokit, …) POST
		// urlencoded bodies in exactly this Rails/PHP shape, and handlers
		// expect req["body"]["line_items"] to be a real list. Every value of
		// a repeated bracket key appends (a[]=1&a[]=2). Malformed bracketing
		// falls back to the flat first-value behavior.
		if segs, ok := splitFormKey(k); ok && len(segs) > 1 {
			for _, v := range vs {
				assignFormValue(out, segs, v)
			}
			continue
		}
		out[k] = vs[0]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// splitFormKey breaks "a[b][c]" into ["a","b","c"] and "a[]" into ["a",""].
// ok is false when the brackets are malformed (unbalanced, trailing junk,
// missing bare key) — the caller then treats the whole key as flat.
func splitFormKey(k string) ([]string, bool) {
	i := strings.IndexByte(k, '[')
	if i < 0 {
		return []string{k}, true
	}
	if i == 0 {
		return nil, false // no bare key before the first bracket
	}
	segs := []string{k[:i]}
	rest := k[i:]
	for rest != "" {
		if rest[0] != '[' {
			return nil, false
		}
		j := strings.IndexByte(rest, ']')
		if j < 1 {
			return nil, false
		}
		segs = append(segs, rest[1:j])
		rest = rest[j+1:]
	}
	return segs, true
}

// assignFormValue walks segs, materializing nested dicts and lists, and sets
// the final segment to val. "" means "append to a list"; a numeric segment
// indexes one (gaps become nil). Conflicting shapes at a path are skipped
// (first writer wins) rather than crashing the handler.
func assignFormValue(cur map[string]any, segs []string, val string) {
	head := segs[0]
	if len(segs) == 1 {
		// A terminal scalar never clobbers an existing structure at the same
		// path (ParseQuery's map iteration order makes "first writer"
		// unenforceable; skip-instead-of-clobber is order-independent).
		switch cur[head].(type) {
		case map[string]any, []any:
			return
		}
		cur[head] = val
		return
	}
	tail := segs[1:]
	switch {
	case tail[0] == "":
		l, _ := cur[head].([]any)
		l = append(l, buildFormValue(tail[1:], val))
		cur[head] = l
	case isNumericSegment(tail[0]):
		idx, _ := strconv.Atoi(tail[0])
		l, _ := cur[head].([]any)
		for len(l) <= idx {
			l = append(l, nil)
		}
		if em, ok := l[idx].(map[string]any); ok {
			assignFormValue(em, tail[1:], val)
		} else if l[idx] == nil {
			sub := map[string]any{}
			assignFormValue(sub, tail[1:], val)
			l[idx] = sub
		}
		cur[head] = l
	default:
		sub, ok := cur[head].(map[string]any)
		if !ok {
			if cur[head] != nil {
				return // shape conflict; skip
			}
			sub = map[string]any{}
			cur[head] = sub
		}
		assignFormValue(sub, tail, val)
	}
}

// buildFormValue materializes the value under a bare "a[]" append: the
// remaining segments nest inside the appended element.
func buildFormValue(segs []string, val string) any {
	if len(segs) == 0 {
		return val
	}
	if len(segs) == 1 {
		return map[string]any{segs[0]: val}
	}
	sub := map[string]any{}
	assignFormValue(sub, segs, val)
	return sub
}

func isNumericSegment(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
