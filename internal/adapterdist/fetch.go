package adapterdist

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Default timeouts for git subprocesses. These prevent a slow or
// unresponsive remote from hanging `stunt adapter add/up/update`.
// Callers may pass a context with a shorter deadline to override.
const (
	cloneTimeout = 60 * time.Second // git clone (potentially large transfer)
	opTimeout    = 30 * time.Second // fetch, checkout, pull, merge, rev-parse
)

// Cache manages a local cache of adapter sources under a root directory.
// Git sources are cloned into <root>/git/<host>/<path>[@<ref>]. The cache is
// concurrency-safe: all git operations are serialized by a mutex.
//
// The cache path is ref-stable: sources pinned to different refs for the
// same URL get distinct cache directories (e.g. <root>/git/host/repo@v1.0
// vs <root>/git/host/repo@v2.0). This prevents ref thrashing when two
// services reference the same repository at different refs.
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
// For git sources with a pinned ref: <root>/git/<host>/<path>@<ref>.
// For git sources at head-at-install: <root>/git/<host>/<path>.
// For local sources: the source's URL (the local path as given).
//
// The ref is sanitized for use as a filesystem component (slashes are
// replaced with underscores) so that branch refs like "release/2.0"
// produce a single directory name rather than a nested path.
func (c *Cache) PathFor(s *Source) string {
	if s.Kind == "local" {
		return s.URL
	}
	base := filepath.Join(c.root, "git", s.Host, s.Path)
	if s.Ref == "" {
		return base
	}
	return base + "@" + refToDirName(s.Ref)
}

// refToDirName converts a git ref into a filesystem-safe directory name
// component. Slashes (e.g. in "release/2.0") are replaced with underscores.
func refToDirName(ref string) string {
	return strings.ReplaceAll(ref, "/", "_")
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
//
// The ctx parameter is used for all git subprocesses; internal timeouts
// are applied on top (clone: 60s, other ops: 30s) so a hung remote does
// not block indefinitely. A shorter caller deadline takes precedence.
func (c *Cache) Ensure(ctx context.Context, s *Source) (localDir, resolvedRef string, err error) {
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
		return c.cloneFresh(ctx, s, dir)
	}

	// Cache exists.
	if s.Ref == "" {
		// Head-at-install: return the current HEAD sha.
		sha, err := c.revParse(ctx, dir, "HEAD")
		if err != nil {
			return "", "", err
		}
		return dir, sha, nil
	}

	// Pinned ref: check if already there (fast path — no fetch/checkout).
	headSha, _ := c.revParse(ctx, dir, "HEAD")
	refSha, _ := c.revParse(ctx, dir, s.Ref)
	if headSha != "" && headSha == refSha {
		return dir, headSha, nil
	}

	// Checkout the pinned ref.
	octx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	if err := c.git(octx, dir, "checkout", s.Ref); err != nil {
		return "", "", fmt.Errorf("adapterdist: checkout %s %s: %w", s.URL, s.Ref, err)
	}
	sha, err := c.revParse(ctx, dir, "HEAD")
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
//
// The ctx parameter is used for all git subprocesses; internal timeouts
// are applied on top (fetch/checkout/pull/merge: 30s, clone: 60s).
func (c *Cache) Reconcile(ctx context.Context, s *Source) error {
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
		_, _, err := c.cloneFresh(ctx, s, dir)
		return err
	}

	// Fetch all refs from origin.
	fctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	if err := c.git(fctx, dir, "fetch", "origin"); err != nil {
		return fmt.Errorf("adapterdist: fetch %s: %w", s.URL, err)
	}

	if s.Ref == "" {
		// Head-at-install: pull the current branch (ff-only).
		pctx, cancel := context.WithTimeout(ctx, opTimeout)
		defer cancel()
		if err := c.git(pctx, dir, "pull", "--ff-only"); err != nil {
			return fmt.Errorf("adapterdist: pull %s: %w", s.URL, err)
		}
		return nil
	}

	// Checkout the pinned ref.
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	if err := c.git(cctx, dir, "checkout", s.Ref); err != nil {
		return fmt.Errorf("adapterdist: checkout %s %s: %w", s.URL, s.Ref, err)
	}

	// If the ref is a remote-tracking branch, fast-forward to its latest.
	// For tags/SHAs, origin/<ref> does not resolve and the checkout above
	// is the final state.
	if _, err := c.revParse(ctx, dir, "origin/"+s.Ref); err == nil {
		mctx, cancel := context.WithTimeout(ctx, opTimeout)
		defer cancel()
		if err := c.git(mctx, dir, "merge", "--ff-only", "origin/"+s.Ref); err != nil {
			return fmt.Errorf("adapterdist: merge %s: %w", s.URL, err)
		}
	}

	return nil
}

// cloneFresh clones the source URL into dir and checks out the ref.
// The parent directory is created if needed. On any error the partially
// created directory is removed so a subsequent Ensure retries cleanly.
func (c *Cache) cloneFresh(ctx context.Context, s *Source, dir string) (localDir, resolvedRef string, err error) {
	// On any error, remove the partial clone directory so the next Ensure
	// attempt starts fresh instead of seeing a corrupt half-cloned repo.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()

	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", "", fmt.Errorf("adapterdist: mkdir %s: %w", filepath.Dir(dir), err)
	}

	// The "--" terminates git's option parsing so the URL cannot be
	// interpreted as a flag (option-injection defense, C1).
	cctx, cancel := context.WithTimeout(ctx, cloneTimeout)
	defer cancel()
	if err := c.git(cctx, "", "clone", "--", s.URL, dir); err != nil {
		return "", "", fmt.Errorf("adapterdist: clone %s: %w", s.URL, err)
	}

	if s.Ref != "" {
		rctx, cancel := context.WithTimeout(ctx, opTimeout)
		defer cancel()
		if err := c.git(rctx, dir, "checkout", s.Ref); err != nil {
			return "", "", fmt.Errorf("adapterdist: checkout %s %s: %w", s.URL, s.Ref, err)
		}
	}

	sha, err := c.revParse(ctx, dir, "HEAD")
	if err != nil {
		return "", "", err
	}
	return dir, sha, nil
}

// --- git command helpers ---
//
// All git invocations use [exec.CommandContext] with explicit argv — no
// shell. This makes command injection impossible by construction. Ref
// validation (validateRef) and the "--" separator before URLs are
// additional defense-in-depth layers.

// git runs a git command with explicit argv and returns an error on
// non-zero exit. If dir is non-empty, it is used as the working directory.
// The ctx is used to enforce timeouts and cancellation.
func (c *Cache) git(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
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
func (c *Cache) gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
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
func (c *Cache) revParse(ctx context.Context, dir, ref string) (string, error) {
	out, err := c.gitOutput(ctx, dir, "rev-parse", ref)
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
