package contrib

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSafeName(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   string
	}{
		{"GET", "/users", "get_users"},
		{"POST", "/users/{userId}", "post_users_userid"},
		{"GET", "/api/v1/items", "get_api_v1_items"},
		{"GET", "/", "get_"},
		{"get", "/hello", "get_hello"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			got := SafeName(tt.method, tt.path)
			if got != tt.want {
				t.Errorf("SafeName(%q, %q) = %q, want %q", tt.method, tt.path, got, tt.want)
			}
		})
	}
}

// M4: SafeName must sanitize backslashes to prevent Windows path traversal.
func TestSafeNameSanitizesBackslashes(t *testing.T) {
	got := SafeName("GET", `\users\evil\..\.secret`)
	// No backslashes should remain — they must all be replaced with _.
	for _, c := range got {
		if c == '\\' || c == '/' {
			t.Errorf("SafeName result %q contains a path separator", got)
		}
	}
	// The name should be safe to use as a filename within a directory.
	// Verify it doesn't resolve outside the expected directory.
	dir := t.TempDir()
	full := filepath.Join(dir, got+".json")
	if err := os.WriteFile(full, []byte("ok"), 0o644); err != nil {
		t.Fatalf("writing SafeName-derived path: %v", err)
	}
	// Ensure the file is within dir.
	rel, err := filepath.Rel(dir, full)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	if rel != got+".json" {
		t.Errorf("file resolved to %q, expected %q — possible path traversal", rel, got+".json")
	}
}

func TestGlobPath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/users/{userId}", "/users/*"},
		{"/users/{userId}/orders/{orderId}", "/users/*/orders/*"},
		{"/items", "/items"},
		{"/users/{id}/orders", "/users/*/orders"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := GlobPath(tt.input)
			if got != tt.want {
				t.Errorf("GlobPath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
