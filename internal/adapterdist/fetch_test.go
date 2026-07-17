package adapterdist

import (
	"context"
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

func TestPathForGitPinnedRef(t *testing.T) {
	c, _ := OpenCache(t.TempDir())
	s := &Source{Kind: "git", URL: "github.com/user/repo", Host: "github.com", Path: "user/repo", Ref: "v1.0"}
	base := filepath.Join(c.Root(), "git", "github.com", "user", "repo")
	want := base + "@v1.0"
	if got := c.PathFor(s); got != want {
		t.Errorf("PathFor = %q, want %q", got, want)
	}
}

func TestPathForGitPinnedBranchRef(t *testing.T) {
	c, _ := OpenCache(t.TempDir())
	s := &Source{Kind: "git", URL: "github.com/user/repo", Host: "github.com", Path: "user/repo", Ref: "release/2.0"}
	base := filepath.Join(c.Root(), "git", "github.com", "user", "repo")
	want := base + "@release_2.0" // slash → underscore for dir safety
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

	dir, resolvedRef, err := c.Ensure(context.Background(), s)
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

	dir, resolvedRef, err := c.Ensure(context.Background(), s)
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
	dir1, ref1, err := c.Ensure(context.Background(), s)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}

	// Second call: no-op.
	dir2, ref2, err := c.Ensure(context.Background(), s)
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

	dir1, ref1, err := c.Ensure(context.Background(), s)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	dir2, ref2, err := c.Ensure(context.Background(), s)
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
	dir, ref, err := c.Ensure(context.Background(), s)
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
	dir, _, err := c.Ensure(context.Background(), s)
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
	_, _, err := c.Ensure(context.Background(), s)
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
	dir, resolvedRef, err := c.Ensure(context.Background(), s)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Reconcile (should be a no-op since HEAD hasn't changed in fixture).
	if err := c.Reconcile(context.Background(), s); err != nil {
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

	dir, resolvedRef, err := c.Ensure(context.Background(), s)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if resolvedRef != tagSha {
		t.Fatalf("resolvedRef = %q, want %q", resolvedRef, tagSha)
	}

	// Reconcile: tag should stay at tagSha.
	if err := c.Reconcile(context.Background(), s); err != nil {
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
	if err := c.Reconcile(context.Background(), s); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	dir := c.PathFor(s)
	sha := runGitOut(t, dir, "rev-parse", "HEAD")
	if sha != headSha {
		t.Errorf("HEAD = %q, want %q", sha, headSha)
	}
}

// --- Ensure: different refs get different cache dirs (ref-stable cache) ---

func TestEnsureDifferentRefsGetDifferentDirs(t *testing.T) {
	cloneURL, tagSha, headSha := writeFixtureRepo(t)
	c, _ := OpenCache(t.TempDir())

	// Pin to tag v1.0.
	s1 := newFixtureSource(cloneURL, "v1.0")
	dir1, ref1, err := c.Ensure(context.Background(), s1)
	if err != nil {
		t.Fatalf("Ensure(v1.0): %v", err)
	}
	if ref1 != tagSha {
		t.Fatalf("at v1.0: ref = %q, want %q", ref1, tagSha)
	}

	// Pin to HEAD sha — should get a DIFFERENT dir and check out independently.
	s2 := newFixtureSource(cloneURL, headSha)
	dir2, ref2, err := c.Ensure(context.Background(), s2)
	if err != nil {
		t.Fatalf("Ensure(sha): %v", err)
	}
	if dir1 == dir2 {
		t.Errorf("dirs should differ for different refs: both %q", dir1)
	}
	if ref2 != headSha {
		t.Errorf("at sha: ref = %q, want %q", ref2, headSha)
	}

	// Verify each dir is at the correct ref independently.
	sha1 := runGitOut(t, dir1, "rev-parse", "HEAD")
	sha2 := runGitOut(t, dir2, "rev-parse", "HEAD")
	if sha1 != tagSha {
		t.Errorf("dir1 HEAD = %q, want %q", sha1, tagSha)
	}
	if sha2 != headSha {
		t.Errorf("dir2 HEAD = %q, want %q", sha2, headSha)
	}

	// Verify file content differs: v1.0 has no world.txt, HEAD does.
	if _, err := os.Stat(filepath.Join(dir1, "world.txt")); err == nil {
		t.Error("dir1 (v1.0) should NOT have world.txt")
	}
	if _, err := os.Stat(filepath.Join(dir2, "world.txt")); err != nil {
		t.Error("dir2 (HEAD) should have world.txt")
	}
}

// Head-at-install on a repo whose pinned-ref cache already exists still
// works: it gets its own cache dir (no ref suffix) and clones the default
// branch HEAD.
func TestEnsureHeadAtInstallSeparateFromPinnedRef(t *testing.T) {
	cloneURL, tagSha, headSha := writeFixtureRepo(t)
	c, _ := OpenCache(t.TempDir())

	// First: pin to v1.0.
	s := newFixtureSource(cloneURL, "v1.0")
	dir1, ref, err := c.Ensure(context.Background(), s)
	if err != nil {
		t.Fatalf("Ensure(v1.0): %v", err)
	}
	if ref != tagSha {
		t.Fatalf("at v1.0: ref = %q, want %q", ref, tagSha)
	}

	// Second: head-at-install (Ref="") — different cache dir, clones HEAD.
	s2 := newFixtureSource(cloneURL, "")
	dir2, ref2, err := c.Ensure(context.Background(), s2)
	if err != nil {
		t.Fatalf("Ensure(head): %v", err)
	}
	if dir1 == dir2 {
		t.Errorf("pinned-ref and head-at-install should have different dirs")
	}
	if ref2 != headSha {
		t.Errorf("head-at-install should return HEAD sha %q, got %q",
			headSha, ref2)
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
			_, ref, err := c.Ensure(context.Background(), s)
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

// --- C1: option injection defense ---

// TestNewGitSourceRejectsURLStartingWithDash verifies that a URL that would
// start with "-" is rejected at construction time (defense in depth alongside
// the "--" separator in the clone argv).
func TestNewGitSourceRejectsURLStartingWithDash(t *testing.T) {
	_, err := newGitSource("--upload-pack=evil@host:path", "host", "path", "")
	if err == nil {
		t.Fatal("newGitSource should reject URL starting with '-'")
	}
	if !strings.Contains(err.Error(), "must not start with -") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestParseSourceRejectsSSHShorthandURLStartingWithDash verifies the full
// parse path: the SSH shorthand "git:--upload-pack=evil@host:path" must be
// rejected because the resulting URL starts with "-".
func TestParseSourceRejectsSSHShorthandURLStartingWithDash(t *testing.T) {
	_, err := ParseSource("git:--upload-pack=evil@host:path")
	if err == nil {
		t.Fatal("ParseSource should reject SSH shorthand yielding URL starting with '-'")
	}
}

// TestCloneUsesDoubleDashSeparator verifies that the git clone argv includes
// a "--" before the URL so the URL cannot be interpreted as a git option.
// Indirectly: clone of a valid file:// URL still works with the "--" present.
func TestCloneUsesDoubleDashSeparator(t *testing.T) {
	cloneURL, _, headSha := writeFixtureRepo(t)
	c, _ := OpenCache(t.TempDir())
	s := newFixtureSource(cloneURL, "")
	dir, ref, err := c.Ensure(context.Background(), s)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if ref != headSha {
		t.Errorf("ref = %q, want %q", ref, headSha)
	}
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err != nil {
		t.Errorf("clone dir missing files: %v", err)
	}
}

// --- I5: failed clone cleans up partial directory ---

// TestCloneFreshCleansUpOnFailure verifies that when a clone fails (e.g.
// nonexistent remote), the partial cache directory is removed so a subsequent
// Ensure can retry cleanly.
func TestCloneFreshCleansUpOnFailure(t *testing.T) {
	c, _ := OpenCache(t.TempDir())
	s := &Source{
		Kind: "git",
		URL:  "file:///nonexistent/path/to/repo",
		Host: "localhost",
		Path: "nonexistent",
	}

	_, _, err := c.Ensure(context.Background(), s)
	if err == nil {
		t.Fatal("Ensure should fail for nonexistent remote")
	}

	// The cache directory should NOT exist (cleaned up).
	dir := c.PathFor(s)
	if fileExists(dir) {
		t.Errorf("partial clone dir should be cleaned up, but exists: %s", dir)
	}

	// A subsequent Ensure should also fail (not think the dir already exists).
	_, _, err = c.Ensure(context.Background(), s)
	if err == nil {
		t.Fatal("second Ensure should also fail for nonexistent remote")
	}
}

// --- I3: merge error is returned (not swallowed) ---

// TestReconcileReturnsMergeError verifies that Reconcile propagates merge
// errors instead of silently ignoring them. We create diverged histories:
// the local cache gets a commit the origin doesn't have, AND the origin gets
// a commit the cache doesn't have, so `git merge --ff-only` must fail.
func TestReconcileReturnsMergeError(t *testing.T) {
	cloneURL, _, _ := writeFixtureRepo(t)
	c, _ := OpenCache(t.TempDir())

	// Use a branch ref so Reconcile attempts a merge --ff-only.
	s := newFixtureSource(cloneURL, "master")
	dir, _, err := c.Ensure(context.Background(), s)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Create a divergent local commit in the cache.
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "local-divergent.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "divergent local commit")

	// Also advance the origin fixture repo with a different commit so
	// histories truly diverge (both sides have unique commits).
	originDir := strings.TrimPrefix(cloneURL, "file://")
	if err := os.WriteFile(filepath.Join(originDir, "origin-new.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, originDir, "add", ".")
	runGit(t, originDir, "commit", "-m", "origin advances")

	// Reconcile should fail with a merge error (not silently succeed).
	err = c.Reconcile(context.Background(), s)
	if err == nil {
		t.Fatal("Reconcile should return merge error for diverged histories")
	}
	if !strings.Contains(err.Error(), "merge") {
		t.Errorf("error should mention 'merge', got: %v", err)
	}
}

// --- I2: same URL different refs get different dirs ---

// TestRefStableCacheDifferentRefs verifies that two sources with the same
// URL but different refs resolve to DIFFERENT local directories and each
// checks out its own ref independently.
func TestRefStableCacheDifferentRefs(t *testing.T) {
	cloneURL, tagSha, headSha := writeFixtureRepo(t)
	c, _ := OpenCache(t.TempDir())

	// Source A: pinned to v1.0.
	sA := newFixtureSource(cloneURL, "v1.0")
	dirA, refA, err := c.Ensure(context.Background(), sA)
	if err != nil {
		t.Fatalf("Ensure(v1.0): %v", err)
	}
	if refA != tagSha {
		t.Fatalf("refA = %q, want %q", refA, tagSha)
	}

	// Source B: pinned to HEAD by SHA.
	sB := newFixtureSource(cloneURL, headSha)
	dirB, refB, err := c.Ensure(context.Background(), sB)
	if err != nil {
		t.Fatalf("Ensure(sha): %v", err)
	}
	if refB != headSha {
		t.Fatalf("refB = %q, want %q", refB, headSha)
	}

	// Different dirs.
	if dirA == dirB {
		t.Fatalf("same URL different refs should have different cache dirs, both: %s", dirA)
	}

	// Both dirs still at their respective refs.
	shaA := runGitOut(t, dirA, "rev-parse", "HEAD")
	shaB := runGitOut(t, dirB, "rev-parse", "HEAD")
	if shaA != tagSha {
		t.Errorf("dirA HEAD = %q, want %q", shaA, tagSha)
	}
	if shaB != headSha {
		t.Errorf("dirB HEAD = %q, want %q", shaB, headSha)
	}

	// Content isolation: dirA (v1.0) has no world.txt; dirB (HEAD) does.
	if _, err := os.Stat(filepath.Join(dirA, "world.txt")); err == nil {
		t.Error("dirA (v1.0) should NOT have world.txt")
	}
	if _, err := os.Stat(filepath.Join(dirB, "world.txt")); err != nil {
		t.Error("dirB (HEAD) should have world.txt")
	}
}

// --- I4: context is threaded (cancelled context fails) ---

// TestEnsureWithContextCancelled verifies that a cancelled context causes
// Ensure to fail (proving the context is actually used).
func TestEnsureWithContextCancelled(t *testing.T) {
	cloneURL, _, _ := writeFixtureRepo(t)
	c, _ := OpenCache(t.TempDir())
	s := newFixtureSource(cloneURL, "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, _, err := c.Ensure(ctx, s)
	if err == nil {
		t.Fatal("Ensure with cancelled context should fail")
	}
}
