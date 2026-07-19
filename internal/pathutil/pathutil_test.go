package pathutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContainedPath_ValidRelative(t *testing.T) {
	dir := t.TempDir()
	absDir, _ := filepath.Abs(dir)

	got, err := ContainedPath(absDir, "fixtures/body.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(absDir, "fixtures", "body.json")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestContainedPath_ValidNested(t *testing.T) {
	dir := t.TempDir()
	absDir, _ := filepath.Abs(dir)

	got, err := ContainedPath(absDir, "a/b/c.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(absDir, "a", "b", "c.txt")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestContainedPath_TraversalEscape(t *testing.T) {
	dir := t.TempDir()
	absDir, _ := filepath.Abs(dir)

	_, err := ContainedPath(absDir, "../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for ../../etc/passwd, got nil")
	}
}

func TestContainedPath_TraversalSingleLevel(t *testing.T) {
	dir := t.TempDir()
	absDir, _ := filepath.Abs(dir)

	_, err := ContainedPath(absDir, "../secret.txt")
	if err == nil {
		t.Fatal("expected error for ../secret.txt, got nil")
	}
}

func TestContainedPath_AbsoluteOutsideBase(t *testing.T) {
	dir := t.TempDir()
	absDir, _ := filepath.Abs(dir)

	_, err := ContainedPath(absDir, "/etc/passwd")
	if err == nil {
		t.Fatal("expected error for absolute /etc/passwd, got nil")
	}
}

func TestContainedPath_AbsoluteInsideBase(t *testing.T) {
	dir := t.TempDir()
	absDir, _ := filepath.Abs(dir)

	// An absolute path that IS inside baseDir should be accepted.
	inner := filepath.Join(absDir, "data", "file.json")
	got, err := ContainedPath(absDir, inner)
	if err != nil {
		t.Fatalf("unexpected error for absolute-in-base: %v", err)
	}
	if got != filepath.Clean(inner) {
		t.Errorf("got %q, want %q", got, filepath.Clean(inner))
	}
}

func TestContainedPath_DotAndDotDotWithinBase(t *testing.T) {
	dir := t.TempDir()
	absDir, _ := filepath.Abs(dir)

	// "./file" resolves within base — OK
	got, err := ContainedPath(absDir, "./file.json")
	if err != nil {
		t.Fatalf("unexpected error for ./file.json: %v", err)
	}
	want := filepath.Join(absDir, "file.json")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestContainedPath_NormalizedTraversal(t *testing.T) {
	dir := t.TempDir()
	absDir, _ := filepath.Abs(dir)

	// "a/../../etc/passwd" normalizes to "../etc/passwd" — escapes
	_, err := ContainedPath(absDir, "a/../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for a/../../etc/passwd, got nil")
	}
}

// TestContainedPath_ReadGuard confirms that ContainedPath + os.ReadFile
// returns an error (not the file bytes) for a traversal attempt targeting a
// real file in baseDir's parent.
func TestContainedPath_ReadGuard(t *testing.T) {
	baseDir := t.TempDir()
	absBase, _ := filepath.Abs(baseDir)

	// Create a secret file in baseDir's parent.
	parent := filepath.Dir(absBase)
	secretPath := filepath.Join(parent, "stunt_secret_marker.txt")
	if err := os.WriteFile(secretPath, []byte("SHOULD_NOT_BE_SERVED"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(secretPath) })

	_, err := ContainedPath(absBase, "../stunt_secret_marker.txt")
	if err == nil {
		t.Fatal("expected ContainedPath to reject traversal to parent file")
	}
}
