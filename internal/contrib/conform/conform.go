// Package conform replays recorded real traces against a locally-running
// adapter instance and compares the simulator's responses to the recorded
// expected responses.
//
// The user supplies a traces file (JSONL of {request, response}). For each
// trace, Run fires the request at a live engine instance and compares the
// response. By default the comparison is STRUCTURAL: same HTTP status + same
// JSON shape (keys, nesting, types) — values may differ because the simulator
// produces synthetic data. Strict mode additionally compares exact values.
//
// This lets a contributor verify their adapter matches the real API's
// contract without shipping any real data: the adapter ships synthetic
// templates, the traces live locally only.
package conform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"stuntapi.com/stunt/internal/engine"
	"stuntapi.com/stunt/internal/manifest"
)

// Options controls conformance check behavior.
type Options struct {
	// Strict compares exact JSON values instead of just structure. By
	// default (false), only the JSON shape (keys, nesting, types) is compared
	// since the simulator produces synthetic values.
	Strict bool
	// ClientTimeout is the per-request timeout. If zero, a default of 10s is
	// used. This prevents a hanging handler from blocking the conform run
	// indefinitely.
	ClientTimeout time.Duration
}

// Trace is a single recorded request/response pair from a real session.
type Trace struct {
	Request  TraceRequest  `json:"request"`
	Response TraceResponse `json:"response"`
}

// TraceRequest is the recorded request to replay.
type TraceRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

// TraceResponse is the recorded response to compare against.
type TraceResponse struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// Report summarizes a conformance run.
type Report struct {
	Total      int        // total traces replayed
	Matched    int        // traces that matched
	Mismatched []Mismatch // traces that did not match
}

// Score returns the conformance percentage (matched/total * 100).
// If Total is 0, returns 100.0 (nothing to check = perfect).
func (r *Report) Score() float64 {
	if r.Total == 0 {
		return 100.0
	}
	return float64(r.Matched) / float64(r.Total) * 100.0
}

// Mismatch describes a single failed conformance check.
type Mismatch struct {
	Request  string // "METHOD path"
	Expected string // description of expected response
	Got      string // description of actual response
	Reason   string // why they differ
}

// Run replays each trace against a locally-running adapter instance and
// returns a conformance report. It starts the engine on a free high port,
// fires each recorded request, captures the simulator's response, and
// compares it to the recorded expected response.
func Run(ctx context.Context, adapterDir, tracesPath string, opts Options) (*Report, error) {
	traces, err := loadTraces(tracesPath)
	if err != nil {
		return nil, err
	}

	// Build a manifest that serves the adapter as a single service.
	stateDir := adapterDir
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, ".stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"sut": {Adapter: adapterDir},
		},
	}

	eng, err := engine.New(m)
	if err != nil {
		return nil, fmt.Errorf("conform: start engine: %w", err)
	}
	defer eng.Close()

	addrs, cancel, err := eng.ServeForTest(ctx)
	if err != nil {
		return nil, fmt.Errorf("conform: serve: %w", err)
	}
	defer cancel()

	baseURL := addrs["sut"]

	report := &Report{Total: len(traces)}
	timeout := opts.ClientTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	for _, tr := range traces {
		mismatch := replay(ctx, client, baseURL, tr, opts)
		if mismatch == nil {
			report.Matched++
		} else {
			report.Mismatched = append(report.Mismatched, *mismatch)
		}
	}
	return report, nil
}

// replay fires a single trace's request at baseURL and compares the response.
func replay(ctx context.Context, client *http.Client, baseURL string, tr Trace, opts Options) *Mismatch {
	reqDesc := fmt.Sprintf("%s %s", tr.Request.Method, tr.Request.Path)

	url := baseURL + tr.Request.Path
	method := tr.Request.Method
	if method == "" {
		method = "GET"
	}

	var bodyReader io.Reader
	if len(tr.Request.Body) > 0 {
		bodyReader = bytes.NewReader(tr.Request.Body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return &Mismatch{Request: reqDesc, Reason: fmt.Sprintf("build request: %v", err)}
	}
	for k, v := range tr.Request.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return &Mismatch{Request: reqDesc, Reason: fmt.Sprintf("request failed: %v", err)}
	}
	defer resp.Body.Close()

	gotBody, _ := io.ReadAll(resp.Body)

	// Compare status.
	if resp.StatusCode != tr.Response.Status {
		return &Mismatch{
			Request:  reqDesc,
			Expected: fmt.Sprintf("status %d", tr.Response.Status),
			Got:      fmt.Sprintf("status %d (%s)", resp.StatusCode, truncate(string(gotBody), 100)),
			Reason:   fmt.Sprintf("status mismatch: expected %d, got %d", tr.Response.Status, resp.StatusCode),
		}
	}

	// Compare body.
	expectedBody, err := parseBody(tr.Response.Body)
	if err != nil {
		return &Mismatch{Request: reqDesc, Reason: fmt.Sprintf("parse expected body: %v", err)}
	}
	gotParsed, err := parseBody(gotBody)
	if err != nil {
		return &Mismatch{Request: reqDesc, Reason: fmt.Sprintf("parse response body: %v", err)}
	}

	if opts.Strict {
		if !reflect.DeepEqual(expectedBody, gotParsed) {
			return &Mismatch{
				Request:  reqDesc,
				Expected: truncate(string(tr.Response.Body), 100),
				Got:      truncate(string(gotBody), 100),
				Reason:   "strict value mismatch",
			}
		}
	} else {
		if !sameShape(expectedBody, gotParsed) {
			return &Mismatch{
				Request:  reqDesc,
				Expected: describeShape(expectedBody),
				Got:      describeShape(gotParsed),
				Reason:   "response structure (keys/types) does not match",
			}
		}
	}

	return nil
}

// loadTraces reads and parses a JSONL traces file.
func loadTraces(path string) ([]Trace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("conform: read traces: %w", err)
	}
	var traces []Trace
	for lineNum, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var tr Trace
		if err := json.Unmarshal([]byte(line), &tr); err != nil {
			return nil, fmt.Errorf("conform: parse trace line %d: %w", lineNum+1, err)
		}
		traces = append(traces, tr)
	}
	if len(traces) == 0 {
		return nil, fmt.Errorf("conform: no traces found in %s", path)
	}
	return traces, nil
}

// parseBody parses a raw JSON body. Empty/nil input returns nil.
func parseBody(raw []byte) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// sameShape reports whether two JSON values have the same structure:
// the same keys for objects, same nesting, and same types for scalars.
// Values are ignored — only the shape is compared.
func sameShape(a, b any) bool {
	return reflect.DeepEqual(shapeOf(a), shapeOf(b))
}

// shapeOf returns a normalized "shape" of a JSON value: for objects, a map
// of key → shape; for arrays, a one-element array of the first item's shape;
// for scalars, the type name. Values are discarded.
func shapeOf(v any) any {
	switch val := v.(type) {
	case map[string]any:
		result := make(map[string]any, len(val))
		for k, child := range val {
			result[k] = shapeOf(child)
		}
		return result
	case []any:
		if len(val) == 0 {
			return []string{} // empty array shape
		}
		return []any{shapeOf(val[0])}
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	default:
		return "unknown"
	}
}

// describeShape produces a compact string representation of a JSON value's
// shape for human-readable mismatch messages.
func describeShape(v any) string {
	s, err := json.Marshal(shapeOf(v))
	if err != nil {
		return "<unknown>"
	}
	return truncate(string(s), 120)
}

// truncate shortens s to at most n runes, appending "…" if truncated.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
