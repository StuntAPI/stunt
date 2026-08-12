package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPGracefulShutdown(t *testing.T) {
	// 2xx → nil; also verifies POST + path + token header.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/api/shutdown" {
			t.Errorf("path = %q, want /api/shutdown", r.URL.Path)
		}
		if r.Header.Get("X-Stunt-Token") != "sekret" {
			t.Errorf("token header = %q, want sekret", r.Header.Get("X-Stunt-Token"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ok.Close)
	if err := httpGracefulShutdown(ok.URL, "sekret"); err != nil {
		t.Fatalf("2xx: want nil, got %v", err)
	}

	// 5xx → error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(bad.Close)
	if err := httpGracefulShutdown(bad.URL, "sekret"); err == nil {
		t.Fatal("5xx: want error, got nil")
	}

	// Non-listening → error (connection refused on a privileged port).
	if err := httpGracefulShutdown("http://127.0.0.1:1", "sekret"); err == nil {
		t.Fatal("non-listening: want error, got nil")
	}

	// Empty URL → error (caller must skip the graceful step).
	if err := httpGracefulShutdown("", "sekret"); err == nil {
		t.Fatal("empty URL: want error, got nil")
	}
}
