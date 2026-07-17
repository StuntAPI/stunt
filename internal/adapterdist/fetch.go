package adapterdist

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Cache manages a local cache of adapter sources under a root directory.
// Git sources are cloned into <root>/git/<host>/<path>. The cache is
// concurrency-safe: all git operations are serialized by a mutex.
type Cache struct {
	root string
	mu   sync.Mutex
}

// OpenCache opens or creates a cache rooted at root. The directory
// structure (<root>/git/) is created if it does not exist.
func OpenCache(root string) (*Cache, error) {
	if root == "" {
		return nil, fmt.Errorf("adapterdist: cache root is required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("adapterdist: resolve cache root %q: %w", root, err)
	}
	gitDir := filepath.Join(absRoot, "git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		return nil, fmt.Errorf("adapterdist: create cache dir %s: %w", gitDir, err)
	}
	return &Cache{root: absRoot}, nil
}

// Root returns the absolute root directory of the cache.
func (c *Cache) Root() string {
	return c.root
}

// PathFor returns the cache directory for a source without fetching it.
// For git sources: <root>/git/<host>/<path>.
// For local sources: the source's URL (the local path as given).
func (c *Cache) PathFor(s *Source) string {
	if s.Kind == "local" {
		return s.URL
	}
	return filepath.Join(c.root, "git", s.Host, s.Path)
}

// Ensure clones the source into the cache (if not already present) and
// checks out the pinned ref. If the cache already exists and is at the
// pinned ref, it is a no-op (fast — only a rev-parse, no network).
//
// For head-at-install sources (Ref == ""), Ensure clones the default
// branch and returns the resolved commit SHA as resolvedRef.
//
// For local sources, Ensure returns the resolved absolute path (no copy).
//
// Ensure is idempotent: calling it multiple times yields the same result.
func (c *Cache) Ensure(s *Source) (localDir, resolvedRef string, err error) {
	if s.Kind == "local" {
		abs, err := filepath.Abs(s.URL)
		if err != nil {
			return "", "", fmt.Errorf("adapterdist: resolve local path %q: %w", s.URL, err)
		}
		return abs, "", nil
	}

	// Defense-in-depth: re-validate the ref before passing it to git,
	// even though ParseSource already validated it.
	if err := validateRef(s.Ref); err != nil {
		return "", "", err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	dir := c.PathFor(s)

	if !fileExists(dir) {
		return c.cloneFresh(s, dir)
	}

	// Cache exists.
	if s.Ref == "" {
		// Head-at-install: return the current HEAD sha.
		sha, err := c.revParse(dir, "HEAD")
		if err != nil {
			return "", "", err
		}
		return dir, sha, nil
	}

	// Pinned ref: check if already there (fast path — no fetch/checkout).
	headSha, _ := c.revParse(dir, "HEAD")
	refSha, _ := c.revParse(dir, s.Ref)
	if headSha != "" && headSha == refSha {
		return dir, headSha, nil
	}

	// Checkout the pinned ref.
	if err := c.git(dir, "checkout", s.Ref); err != nil {
		return "", "", fmt.Errorf("adapterdist: checkout %s %s: %w", s.URL, s.Ref, err)
	}
	sha, err := c.revParse(dir, "HEAD")
	if err != nil {
		return "", "", err
	}
	return dir, sha, nil
}

// Reconcile re-fetches from the remote and resets the working tree to the
// pinned ref WITHOUT changing which ref is pinned (reconcile-don't-bump).
// This is used by `stunt adapter update` to pull the latest commits for a
// branch or re-verify a tag/sha.
//
// For tags and SHAs the checkout is effectively a no-op (they don't move).
// For branches the local branch is fast-forwarded to the remote tip.
func (c *Cache) Reconcile(s *Source) error {
	if s.Kind == "local" {
		return nil
	}

	if err := validateRef(s.Ref); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	dir := c.PathFor(s)

	if !fileExists(dir) {
		_, _, err := c.cloneFresh(s, dir)
		return err
	}

	// Fetch all refs from origin.
	if err := c.git(dir, "fetch", "origin"); err != nil {
		return fmt.Errorf("adapterdist: fetch %s: %w", s.URL, err)
	}

	if s.Ref == "" {
		// Head-at-install: pull the current branch (ff-only).
		if err := c.git(dir, "pull", "--ff-only"); err != nil {
			return fmt.Errorf("adapterdist: pull %s: %w", s.URL, err)
		}
		return nil
	}

	// Checkout the pinned ref.
	if err := c.git(dir, "checkout", s.Ref); err != nil {
		return fmt.Errorf("adapterdist: checkout %s %s: %w", s.URL, s.Ref, err)
	}

	// If the ref is a remote-tracking branch, fast-forward to its latest.
	// For tags/SHAs, origin/<ref> does not resolve and the error is ignored.
	if _, err := c.revParse(dir, "origin/"+s.Ref); err == nil {
		_ = c.git(dir, "merge", "--ff-only", "origin/"+s.Ref)
	}

	return nil
}

// cloneFresh clones the source URL into dir and checks out the ref.
// The parent directory is created if needed.
func (c *Cache) cloneFresh(s *Source, dir string) (string, string, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", "", fmt.Errorf("adapterdist: mkdir %s: %w", filepath.Dir(dir), err)
	}

	if err := c.git("", "clone", s.URL, dir); err != nil {
		return "", "", fmt.Errorf("adapterdist: clone %s: %w", s.URL, err)
	}

	if s.Ref != "" {
		if err := c.git(dir, "checkout", s.Ref); err != nil {
			return "", "", fmt.Errorf("adapterdist: checkout %s %s: %w", s.URL, s.Ref, err)
		}
	}

	sha, err := c.revParse(dir, "HEAD")
	if err != nil {
		return "", "", err
	}
	return dir, sha, nil
}

// --- git command helpers ---
//
// All git invocations use [exec.Command] with explicit argv — no shell.
// This makes command injection impossible by construction. Ref validation
// (validateRef) is an additional defense-in-depth layer.

// git runs a git command with explicit argv and returns an error on
// non-zero exit. If dir is non-empty, it is used as the working directory.
func (c *Cache) git(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// gitOutput runs a git command and returns its stdout.
func (c *Cache) gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// revParse runs `git -C dir rev-parse <ref>` and returns the trimmed output.
func (c *Cache) revParse(dir, ref string) (string, error) {
	out, err := c.gitOutput(dir, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// fileExists returns true if path exists (file or directory).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
