package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// openRecorder is a test seam injected into openBrowser; it records the URL
// passed to the browser-open step and reports success without spawning a process.
var openRecorder = &urlRecorder{}

type urlRecorder struct {
	url string
}

func (u *urlRecorder) open(url string) error {
	u.url = url
	return nil
}

func (u *urlRecorder) reset() { u.url = "" }

// TestRunUIOpensBrowserWithToken drives runUI against a runtime file and
// asserts that the resolved dashboard URL is opened in the browser with a
// ?token=<token> query param.
func TestRunUIOpensBrowserWithToken(t *testing.T) {
	openRecorder.reset()
	prev := openBrowser
	openBrowser = openRecorder.open
	t.Cleanup(func() { openBrowser = prev })

	const tok = "abc123"
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	const dashURL = "http://127.0.0.1:41257"
	if err := writeRuntimeFile(manifestDir(manifestPath), RuntimeFile{
		Manifest:       manifestPath,
		Mode:           "port",
		DashboardURL:   dashURL,
		DashboardToken: tok,
	}); err != nil {
		t.Fatalf("writeRuntimeFile: %v", err)
	}

	if err := runUI(manifestPath); err != nil {
		t.Fatalf("runUI: %v", err)
	}

	if openRecorder.url == "" {
		t.Fatal("expected browser open to be attempted, but no URL recorded")
	}
	wantSuffix := "/?token=" + tok
	if !strings.HasSuffix(openRecorder.url, wantSuffix) {
		t.Fatalf("expected URL ending in %q, got %q", wantSuffix, openRecorder.url)
	}
	if !strings.HasPrefix(openRecorder.url, dashURL) {
		t.Fatalf("expected URL starting with %q, got %q", dashURL, openRecorder.url)
	}
}

// TestRunUINoServer asserts that with no runtime file, runUI returns a
// friendly error pointing at `stunt up`.
func TestRunUINoServer(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	if err := runUI(manifestPath); err == nil {
		t.Fatal("expected error when no server is running")
	} else if !strings.Contains(err.Error(), "stunt up") {
		t.Fatalf("expected error mentioning `stunt up`, got: %v", err)
	}
}
