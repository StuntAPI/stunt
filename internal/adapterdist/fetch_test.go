package adapterdist

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireGit skips the test if git is not on PATH.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH — skipping live git clone tests")
	}
}

// runGit runs a git command in dir (or the working dir if ""), failing the
// test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// runGitOut runs a git command in dir and returns its trimmed stdout.
func runGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s in %s: %v", strings.Join(args, " "), dir, err)
	}
	return strings.TrimSpace(string(out))
}

// writeFixtureRepo creates a local git repository suitable for cloning via
// file://. It returns:
//
//   - cloneURL: a file:// URL for cloning the repo
//   - firstSha: the SHA of the first commit (also tagged v1.0)
//   - headSha:  the SHA of the second commit (the current HEAD)
//
// The repo has two commits and one tag (v1.0 → first commit).
func writeFixtureRepo(t *testing.T) (cloneURL, firstSha, headSha string) {
	t.Helper()
	requireGit(t)

	dir := t.TempDir()

	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "commit.gpgsign", "false")

	// First commit + tag.
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "initial commit")
	firstSha = runGitOut(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "tag", "v1.0")

	// Second commit (HEAD advances).
	if err := os.WriteFile(filepath.Join(dir, "world.txt"), []byte("world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "second commit")
	headSha = runGitOut(t, dir, "rev-parse", "HEAD")

	return "file://" + dir, firstSha, headSha
}

// newFixtureSource builds a *Source that points at a local file:// git repo.
// Host/path are safe identifiers for the cache layout; each test uses its
// own OpenCache(t.TempDir()) so there is no collision.
func newFixtureSource(cloneURL, ref string) *Source {
	return &Source{
		Kind: "git",
		URL:  cloneURL,
		Host: "localhost",
		Path: "fixture",
		Ref:  ref,
	}
}

// --- OpenCache ---

func TestOpenCache(t *testing.T) {
	root := t.TempDir()
	c, err := OpenCache(root)
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	// The git/ directory should be created.
	gitDir := filepath.Join(c.Root(), "git")
	if _, err := os.Stat(gitDir); err != nil {
		t.Fatalf("git cache dir not created: %v", err)
	}
}

func TestOpenCacheEmptyRoot(t *testing.T) {
	_, err := OpenCache("")
	if err == nil {
		t.Fatal("OpenCache(\"\") should error")
	}
}

// --- PathFor ---

func TestPathForGit(t *testing.T) {
	c, _ := OpenCache(t.TempDir())
	s := &Source{Kind: "git", URL: "github.com/user/repo", Host: "github.com", Path: "user/repo"}
	want := filepath.Join(c.Root(), "git", "github.com", "user", "repo")
	if got := c.PathFor(s); got != want {
		t.Errorf("PathFor = %q, want %q", got, want)
	}
}

func TestPathForLocal(t *testing.T) {
	c, _ := OpenCache(t.TempDir())
	s := &Source{Kind: "local", URL: "/some/local/path"}
	if got := c.PathFor(s); got != "/some/local/path" {
		t.Errorf("PathFor = %q, want /some/local/path", got)
	}
}

// --- Ensure: git clone + checkout ---

func TestEnsureCloneHeadAtInstall(t *testing.T) {
	cloneURL, _, headSha := writeFixtureRepo(t)
	c, _ := OpenCache(t.TempDir())
	s := newFixtureSource(cloneURL, "") // head-at-install

	dir, resolvedRef, err := c.Ensure(s)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Resolved ref should be the current HEAD SHA.
	if resolvedRef != headSha {
		t.Errorf("resolvedRef = %q, want %q", resolvedRef, headSha)
	}

	// The directory should exist and contain the cloned files.
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err != nil {
		t.Errorf("hello.txt missing in cloned dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "world.txt")); err != nil {
		t.Errorf("world.txt missing in cloned dir: %v", err)
	}
}

func TestEnsureClonePinnedRef(t *testing.T) {
	cloneURL, tagSha, headSha := writeFixtureRepo(t)
	c, _ := OpenCache(t.TempDir())
	s := newFixtureSource(cloneURL, "v1.0")

	dir, resolvedRef, err := c.Ensure(s)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// At v1.0 (first commit), world.txt should NOT exist yet.
	if resolvedRef != tagSha {
		t.Errorf("resolvedRef = %q, want %q (tag sha)", resolvedRef, tagSha)
	}
	if resolvedRef == headSha {
		t.Errorf("resolvedRef should not be HEAD sha when pinned to v1.0")
	}
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err != nil {
		t.Errorf("hello.txt missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "world.txt")); err == nil {
		t.Errorf("world.txt should NOT exist at v1.0")
	}
}

// --- Ensure: idempotent ---

func TestEnsureIdempotentHead(t *testing.T) {
	cloneURL, _, headSha := writeFixtureRepo(t)
	c, _ := OpenCache(t.TempDir())
	s := newFixtureSource(cloneURL, "")

	// First call: clone.
	dir1, ref1, err := c.Ensure(s)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}

	// Second call: no-op.
	dir2, ref2, err := c.Ensure(s)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	if dir1 != dir2 {
		t.Errorf("dir changed: %q → %q", dir1, dir2)
	}
	if ref1 != ref2 {
		t.Errorf("resolvedRef changed: %q → %q", ref1, ref2)
	}
	if ref1 != headSha {
		t.Errorf("resolvedRef = %q, want %q", ref1, headSha)
	}
}

func TestEnsureIdempotentPinnedRef(t *testing.T) {
	cloneURL, tagSha, _ := writeFixtureRepo(t)
	c, _ := OpenCache(t.TempDir())
	s := newFixtureSource(cloneURL, "v1.0")

	dir1, ref1, err := c.Ensure(s)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	dir2, ref2, err := c.Ensure(s)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if dir1 != dir2 || ref1 != ref2 {
		t.Errorf("not idempotent: (%q,%q) → (%q,%q)", dir1, ref1, dir2, ref2)
	}
	if ref1 != tagSha {
		t.Errorf("resolvedRef = %q, want %q", ref1, tagSha)
	}
}

// --- Ensure: local path passthrough ---

func TestEnsureLocalPath(t *testing.T) {
	c, _ := OpenCache(t.TempDir())
	absPath := t.TempDir()

	s := &Source{Kind: "local", URL: absPath}
	dir, ref, err := c.Ensure(s)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if dir != absPath {
		t.Errorf("dir = %q, want %q", dir, absPath)
	}
	if ref != "" {
		t.Errorf("ref should be empty for local, got %q", ref)
	}
}

func TestEnsureLocalRelativePath(t *testing.T) {
	c, _ := OpenCache(t.TempDir())
	s := &Source{Kind: "local", URL: "."}
	dir, _, err := c.Ensure(s)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("dir should be absolute, got %q", dir)
	}
}

// --- Ensure: rejects unsafe ref (defense-in-depth) ---

func TestEnsureRejectsUnsafeRef(t *testing.T) {
	c, _ := OpenCache(t.TempDir())
	// Construct a Source with a dangerous ref directly (bypassing ParseSource).
	s := &Source{
		Kind: "git",
		URL:  "file:///nonexistent",
		Host: "localhost",
		Path: "fixture",
		Ref:  "--upload-pack=evil",
	}
	_, _, err := c.Ensure(s)
	if err == nil {
		t.Fatal("Ensure should reject unsafe ref")
	}
}

// --- Reconcile ---

func TestReconcileFastForwardsBranch(t *testing.T) {
	cloneURL, _, headSha := writeFixtureRepo(t)
	c, _ := OpenCache(t.TempDir())

	// First ensure at head-at-install.
	s := newFixtureSource(cloneURL, "")
	dir, resolvedRef, err := c.Ensure(s)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Reconcile (should be a no-op since HEAD hasn't changed in fixture).
	if err := c.Reconcile(s); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Verify dir still exists and HEAD unchanged.
	sha := runGitOut(t, dir, "rev-parse", "HEAD")
	if sha != headSha {
		t.Errorf("after reconcile, HEAD = %q, want %q", sha, headSha)
	}
	_ = resolvedRef
}

func TestReconcileNoOpForTag(t *testing.T) {
	cloneURL, tagSha, _ := writeFixtureRepo(t)
	c, _ := OpenCache(t.TempDir())
	s := newFixtureSource(cloneURL, "v1.0")

	dir, resolvedRef, err := c.Ensure(s)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if resolvedRef != tagSha {
		t.Fatalf("resolvedRef = %q, want %q", resolvedRef, tagSha)
	}

	// Reconcile: tag should stay at tagSha.
	if err := c.Reconcile(s); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	sha := runGitOut(t, dir, "rev-parse", "HEAD")
	if sha != tagSha {
		t.Errorf("after reconcile, HEAD = %q, want %q (tag)", sha, tagSha)
	}
}

func TestReconcileClonesIfMissing(t *testing.T) {
	cloneURL, _, headSha := writeFixtureRepo(t)
	c, _ := OpenCache(t.TempDir())
	s := newFixtureSource(cloneURL, "")

	// Reconcile without a prior Ensure — should clone.
	if err := c.Reconcile(s); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	dir := c.PathFor(s)
	sha := runGitOut(t, dir, "rev-parse", "HEAD")
	if sha != headSha {
		t.Errorf("HEAD = %q, want %q", sha, headSha)
	}
}

// --- Ensure: checkout switches refs between calls ---

func TestEnsureSwitchesRefBetweenCalls(t *testing.T) {
	cloneURL, tagSha, headSha := writeFixtureRepo(t)
	c, _ := OpenCache(t.TempDir())

	// First: pin to tag v1.0.
	s := newFixtureSource(cloneURL, "v1.0")
	dir, ref, err := c.Ensure(s)
	if err != nil {
		t.Fatalf("Ensure(v1.0): %v", err)
	}
	if ref != tagSha {
		t.Fatalf("at v1.0: ref = %q, want %q", ref, tagSha)
	}

	// Second: same cache dir, switch to HEAD by SHA → should checkout.
	s2 := newFixtureSource(cloneURL, headSha)
	dir2, ref2, err := c.Ensure(s2)
	if err != nil {
		t.Fatalf("Ensure(sha): %v", err)
	}
	if dir != dir2 {
		t.Errorf("dir changed: %q → %q", dir, dir2)
	}
	if ref2 != headSha {
		t.Errorf("at sha: ref = %q, want %q", ref2, headSha)
	}
}

// Head-at-install on a pre-existing cache returns the current HEAD sha
// without switching branches (records what is there).
func TestEnsureHeadAtInstallOnExistingReturnsCurrentHead(t *testing.T) {
	cloneURL, tagSha, _ := writeFixtureRepo(t)
	c, _ := OpenCache(t.TempDir())

	// First: pin to v1.0.
	s := newFixtureSource(cloneURL, "v1.0")
	_, ref, err := c.Ensure(s)
	if err != nil {
		t.Fatalf("Ensure(v1.0): %v", err)
	}
	if ref != tagSha {
		t.Fatalf("at v1.0: ref = %q, want %q", ref, tagSha)
	}

	// Second: head-at-install (Ref="") on the existing cache at v1.0.
	// Should return the current HEAD (which is v1.0's sha) without changing.
	s2 := newFixtureSource(cloneURL, "")
	_, ref2, err := c.Ensure(s2)
	if err != nil {
		t.Fatalf("Ensure(head): %v", err)
	}
	if ref2 != tagSha {
		t.Errorf("head-at-install should return current HEAD = %q, got %q",
			tagSha, ref2)
	}
}

// --- Concurrency: race-free Ensure ---

func TestEnsureConcurrent(t *testing.T) {
	cloneURL, _, headSha := writeFixtureRepo(t)
	c, _ := OpenCache(t.TempDir())

	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			s := newFixtureSource(cloneURL, "")
			_, ref, err := c.Ensure(s)
			if err != nil {
				done <- err
				return
			}
			if ref != headSha {
				done <- os.ErrInvalid
				return
			}
			done <- nil
		}()
	}
	for i := 0; i < 4; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent Ensure: %v", err)
		}
	}
}
