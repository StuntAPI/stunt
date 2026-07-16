package conform

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// adapterYAML defines an adapter with two endpoints: GET /items (200 with a
// list) and GET /health (200 ok). A request to any other path returns 404.
const adapterYAML = `
id: test-conf
name: Test Conformance
endpoints:
  - route: /items
    method: GET
    rules:
      - name: items-ok
        match: { method: GET, path: /items }
        respond:
          status: 200
          headers:
            Content-Type: application/json
          body:
            inline:
              items:
                - id: item-001
                  name: Sample Item
                  price: 9.99
              count: 1
  - route: /health
    method: GET
    rules:
      - name: health-ok
        match: { method: GET, path: /health }
        respond:
          status: 200
          body:
            inline:
              status: ok
rules:
  - name: catchall
    match: { path: /** }
    respond: { status: 404, body: { inline: { error: not_found } } }
`

// --- matching traces produce a high score ---

func TestRunMatchingTraces(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", adapterYAML)

	traces := []byte(
		`{"request":{"method":"GET","path":"/health"},"response":{"status":200,"body":{"status":"ok"}}}` + "\n" +
			`{"request":{"method":"GET","path":"/items"},"response":{"status":200,"body":{"items":[{"id":"x","name":"y","price":1.0}],"count":1}}}` + "\n",
	)
	tracesPath := filepath.Join(t.TempDir(), "traces.jsonl")
	if err := os.WriteFile(tracesPath, traces, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), adapterDir, tracesPath, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Total != 2 {
		t.Fatalf("Total = %d, want 2", report.Total)
	}
	if report.Matched != 2 {
		t.Fatalf("Matched = %d, want 2; mismatches: %+v", report.Matched, report.Mismatched)
	}
	if len(report.Mismatched) != 0 {
		for _, m := range report.Mismatched {
			t.Errorf("  mismatch: %s — %s", m.Request, m.Reason)
		}
	}
}

// --- a trace hitting an unimplemented endpoint mismatches ---

func TestRunMismatchedTrace(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", adapterYAML)

	// /unknown is not implemented — the adapter returns 404.
	traces := []byte(
		`{"request":{"method":"GET","path":"/unknown"},"response":{"status":200,"body":{"data":"hello"}}}` + "\n",
	)
	tracesPath := filepath.Join(t.TempDir(), "traces.jsonl")
	if err := os.WriteFile(tracesPath, traces, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), adapterDir, tracesPath, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Total != 1 {
		t.Fatalf("Total = %d, want 1", report.Total)
	}
	if report.Matched != 0 {
		t.Fatalf("Matched = %d, want 0", report.Matched)
	}
	if len(report.Mismatched) != 1 {
		t.Fatalf("Mismatches = %d, want 1", len(report.Mismatched))
	}
	m := report.Mismatched[0]
	if m.Reason == "" {
		t.Error("mismatch reason should be non-empty")
	}
}

// --- structure mode ignores value differences ---

func TestStructureModeIgnoresValues(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", adapterYAML)

	// The adapter returns {"items":[{"id":"item-001",...}],"count":1}.
	// The trace expects different VALUES but the same SHAPE.
	traces := []byte(
		`{"request":{"method":"GET","path":"/items"},"response":{"status":200,"body":{"items":[{"id":"AAA","name":"BBB","price":42.5}],"count":99}}}` + "\n",
	)
	tracesPath := filepath.Join(t.TempDir(), "traces.jsonl")
	if err := os.WriteFile(tracesPath, traces, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), adapterDir, tracesPath, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Matched != 1 {
		t.Fatalf("structure mode: Matched = %d, want 1; mismatches: %+v", report.Matched, report.Mismatched)
	}
}

// --- strict mode catches value differences ---

func TestStrictModeCatchesValues(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", adapterYAML)

	// The adapter returns {"items":[{"id":"item-001","name":"Sample Item","price":9.99}],"count":1}.
	// Strict mode should flag the different values.
	traces := []byte(
		`{"request":{"method":"GET","path":"/items"},"response":{"status":200,"body":{"items":[{"id":"AAA","name":"BBB","price":42.5}],"count":99}}}` + "\n",
	)
	tracesPath := filepath.Join(t.TempDir(), "traces.jsonl")
	if err := os.WriteFile(tracesPath, traces, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), adapterDir, tracesPath, Options{Strict: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Matched != 0 {
		t.Fatalf("strict mode: Matched = %d, want 0; values should differ", report.Matched)
	}
	if len(report.Mismatched) != 1 {
		t.Fatalf("Mismatches = %d, want 1", len(report.Mismatched))
	}
}

// --- POST with body works ---

func TestRunWithRequestBody(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", `
id: post-test
name: Post Test
endpoints:
  - route: /echo
    method: POST
    rules:
      - name: echo-ok
        match: { method: POST, path: /echo }
        respond:
          status: 201
          body:
            inline:
              created: true
`)

	traces := []byte(
		`{"request":{"method":"POST","path":"/echo","body":{"name":"widget"}},"response":{"status":201,"body":{"created":false}}}` + "\n",
	)
	tracesPath := filepath.Join(t.TempDir(), "traces.jsonl")
	if err := os.WriteFile(tracesPath, traces, 0o644); err != nil {
		t.Fatal(err)
	}

	// Structure mode: same shape ({"created": boolean}) → match.
	report, err := Run(context.Background(), adapterDir, tracesPath, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Matched != 1 {
		t.Fatalf("Matched = %d, want 1; mismatches: %+v", report.Matched, report.Mismatched)
	}
}

// --- Score computation ---

func TestReportScore(t *testing.T) {
	r := &Report{Total: 4, Matched: 3}
	if got := r.Score(); got != 75.0 {
		t.Errorf("Score() = %.1f, want 75.0", got)
	}
	r2 := &Report{Total: 0}
	if got := r2.Score(); got != 100.0 {
		t.Errorf("Score() with Total=0 = %.1f, want 100.0", got)
	}
}

// --- shapeOf / sameShape unit tests ---

func TestSameShape(t *testing.T) {
	tests := []struct {
		name string
		a, b any
		want bool
	}{
		{"same object, different values",
			map[string]any{"id": "aaa", "n": float64(1)},
			map[string]any{"id": "bbb", "n": float64(2)},
			true,
		},
		{"different keys",
			map[string]any{"id": "aaa"},
			map[string]any{"name": "aaa"},
			false,
		},
		{"nested object match",
			map[string]any{"obj": map[string]any{"x": "a"}},
			map[string]any{"obj": map[string]any{"x": "b"}},
			true,
		},
		{"array same length match",
			[]any{map[string]any{"id": "a"}},
			[]any{map[string]any{"id": "b"}},
			true,
		},
		{"type mismatch",
			map[string]any{"id": "aaa"},
			"a string",
			false,
		},
		{"extra key",
			map[string]any{"id": "a", "name": "x"},
			map[string]any{"id": "b"},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameShape(tt.a, tt.b); got != tt.want {
				t.Errorf("sameShape(%+v, %+v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// --- traces with empty body ---

func TestRunEmptyResponseBody(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", `
id: empty-body
name: Empty Body
endpoints:
  - route: /noop
    method: GET
    rules:
      - name: noop-ok
        match: { method: GET, path: /noop }
        respond:
          status: 204
`)

	traces := []byte(
		`{"request":{"method":"GET","path":"/noop"},"response":{"status":204}}` + "\n",
	)
	tracesPath := filepath.Join(t.TempDir(), "traces.jsonl")
	if err := os.WriteFile(tracesPath, traces, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), adapterDir, tracesPath, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Matched != 1 {
		t.Fatalf("Matched = %d, want 1; mismatches: %+v", report.Matched, report.Mismatched)
	}
}

// --- I5: client timeout prevents a hanging handler from blocking forever ---

func TestRunClientTimeoutBounded(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", `
id: slow-endpoint
name: Slow Endpoint
endpoints:
  - route: /slow
    method: GET
    rules:
      - name: slow-ok
        match: { method: GET, path: /slow }
        respond:
          behavior: timeout
          latency_ms: 10000
`)

	traces := []byte(
		`{"request":{"method":"GET","path":"/slow"},"response":{"status":200,"body":{"ok":true}}}` + "\n",
	)
	tracesPath := filepath.Join(t.TempDir(), "traces.jsonl")
	if err := os.WriteFile(tracesPath, traces, 0o644); err != nil {
		t.Fatal(err)
	}

	// Use a very short client timeout (200ms). The handler sleeps 10s.
	// Without the timeout, this test would hang for 10s. With the timeout,
	// Run should return in well under 5s.
	done := make(chan struct{})
	var report *Report
	var runErr error
	go func() {
		defer close(done)
		report, runErr = Run(context.Background(), adapterDir, tracesPath, Options{ClientTimeout: 200 * time.Millisecond})
	}()

	select {
	case <-done:
		// Good — Run returned.
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s — client timeout not working")
	}

	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	if report.Total != 1 {
		t.Fatalf("Total = %d, want 1", report.Total)
	}
	// The slow endpoint should produce a mismatch (request failed due to timeout).
	if report.Matched != 0 {
		t.Fatalf("Matched = %d, want 0 (slow endpoint should timeout)", report.Matched)
	}
	if len(report.Mismatched) != 1 {
		t.Fatalf("Mismatches = %d, want 1", len(report.Mismatched))
	}
}
