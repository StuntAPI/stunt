package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stunt-adapters/stunt/internal/adapterdist"
	"github.com/stunt-adapters/stunt/internal/manifest"
)

// --- fixture helpers (offline git source) ---

// requireGitCmd skips the test if git is not on PATH.
func requireGitCmd(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH — skipping live git tests")
	}
}

// runGitCmd runs a git command in dir (or wd if ""), failing the test on error.
func runGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func runGitCmdOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s in %s: %v", strings.Join(args, " "), dir, err)
	}
	return strings.TrimSpace(string(out))
}

// writeFixtureGitRepo creates a local git repo cloneable via file://.
// Returns the clone URL. The repo has one commit with hello.txt.
func writeFixtureGitRepo(t *testing.T) string {
	t.Helper()
	requireGitCmd(t)

	dir := t.TempDir()
	runGitCmd(t, dir, "init")
	runGitCmd(t, dir, "config", "user.email", "test@example.com")
	runGitCmd(t, dir, "config", "user.name", "Test")
	runGitCmd(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, dir, "add", ".")
	runGitCmd(t, dir, "commit", "-m", "initial commit")
	return "file://" + dir
}

// seedGitCache clones a file:// fixture repo into the cache using a
// directly-constructed Source (bypassing ParseSource, like adapterdist's
// own tests do). It returns the source spec string that should be stored in
// the manifest — a value that ParseSource can re-parse to the same Host/Path
// so that Cache.PathFor points to the cloned directory.
//
// The spec "git:<host>/<path>" round-trips through ParseSource into a Source
// with the same Host and Path; git's "origin" remote in the clone still
// points at the file:// URL, so Reconcile (git fetch origin) works offline.
func seedGitCache(t *testing.T, cacheDir, cloneURL, host, path string) string {
	t.Helper()
	cache, err := adapterdist.OpenCache(cacheDir)
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	src := &adapterdist.Source{
		Kind: "git",
		URL:  cloneURL,
		Host: host,
		Path: path,
	}
	if _, _, err := cache.Ensure(src); err != nil {
		t.Fatalf("seed cache Ensure: %v", err)
	}
	// Return a spec that ParseSource can re-parse to the same Host/Path.
	return "git:" + host + "/" + path
}

// =========================================================================
// add
// =========================================================================

func TestAdapterAddLocalSource(t *testing.T) {
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	// Create a local adapter dir to point at.
	localDir := t.TempDir()

	var out bytes.Buffer
	if err := runAdapterAdd(&out, manifestPath, cacheDir, localDir, "local-svc", false); err != nil {
		t.Fatalf("runAdapterAdd: %v", err)
	}

	m, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatalf("manifest.Load: %v", err)
	}
	svc, ok := m.Services["local-svc"]
	if !ok {
		t.Fatalf("service 'local-svc' not in manifest")
	}
	if svc.Adapter != localDir {
		t.Errorf("adapter = %q, want %q", svc.Adapter, localDir)
	}

	outStr := out.String()
	if !strings.Contains(strings.ToLower(outStr), "added") {
		t.Errorf("output should mention 'added': %q", outStr)
	}
}

func TestAdapterAddLocalDerivesName(t *testing.T) {
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	localDir := filepath.Join(t.TempDir(), "my-adapter")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := runAdapterAdd(&bytes.Buffer{}, manifestPath, cacheDir, localDir, "", false); err != nil {
		t.Fatalf("runAdapterAdd: %v", err)
	}

	m, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Services["my-adapter"]; !ok {
		t.Errorf("name should be derived as 'my-adapter', services=%v", m.Services)
	}
}

func TestAdapterAddGitSourceRecordsSpec(t *testing.T) {
	cloneURL := writeFixtureGitRepo(t)
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")

	var out bytes.Buffer
	if err := runAdapterAdd(&out, manifestPath, cacheDir, cloneURL, "git-svc", false); err != nil {
		t.Fatalf("runAdapterAdd: %v", err)
	}

	// The service should be in the manifest.
	m, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatalf("manifest.Load: %v", err)
	}
	svc, ok := m.Services["git-svc"]
	if !ok {
		t.Fatalf("service 'git-svc' not in manifest; services=%v", m.Services)
	}
	// The adapter field should contain the source URL (treated as local by
	// ParseSource since file:// isn't a git protocol — but the spec is stored
	// verbatim, which is what matters for declarative manifests).
	if svc.Adapter != cloneURL {
		t.Errorf("adapter = %q, want %q", svc.Adapter, cloneURL)
	}
}

func TestAdapterAddCollision(t *testing.T) {
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	localDir := t.TempDir()

	// Add first.
	if err := runAdapterAdd(&bytes.Buffer{}, manifestPath, cacheDir, localDir, "svc", false); err != nil {
		t.Fatal(err)
	}

	// Add second with same name → should error.
	err := runAdapterAdd(&bytes.Buffer{}, manifestPath, cacheDir, localDir, "svc", false)
	if err == nil {
		t.Fatal("expected error on name collision")
	}
}

func TestAdapterAddCollisionForce(t *testing.T) {
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	localDir := t.TempDir()

	if err := runAdapterAdd(&bytes.Buffer{}, manifestPath, cacheDir, localDir, "svc", false); err != nil {
		t.Fatal(err)
	}

	// With --force, collision is allowed.
	if err := runAdapterAdd(&bytes.Buffer{}, manifestPath, cacheDir, localDir, "svc", true); err != nil {
		t.Fatalf("runAdapterAdd with force: %v", err)
	}
}

func TestAdapterAddPreservesExistingServices(t *testing.T) {
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	localDir := t.TempDir()

	// Seed a manifest with an existing service.
	m := &manifest.Manifest{
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 8000},
		Services: map[string]manifest.Service{
			"existing": {Adapter: "/some/local/path"},
		},
	}
	if err := manifest.Save(m, manifestPath); err != nil {
		t.Fatal(err)
	}

	if err := runAdapterAdd(&bytes.Buffer{}, manifestPath, cacheDir, localDir, "new-svc", false); err != nil {
		t.Fatal(err)
	}

	m2, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m2.Services["existing"]; !ok {
		t.Error("existing service was removed")
	}
	if _, ok := m2.Services["new-svc"]; !ok {
		t.Error("new service not added")
	}
}

func TestAdapterAddCreatesManifestIfAbsent(t *testing.T) {
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	localDir := t.TempDir()

	if err := runAdapterAdd(&bytes.Buffer{}, manifestPath, cacheDir, localDir, "svc", false); err != nil {
		t.Fatal(err)
	}

	m, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatalf("manifest should exist and be valid: %v", err)
	}
	if m.Version != 1 {
		t.Errorf("Version = %d, want 1", m.Version)
	}
}

func TestAdapterAddInvalidSource(t *testing.T) {
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")

	// An empty source should error.
	err := runAdapterAdd(&bytes.Buffer{}, manifestPath, cacheDir, "", "svc", false)
	if err == nil {
		t.Fatal("expected error for empty source")
	}
}

// =========================================================================
// remove
// =========================================================================

func TestAdapterRemove(t *testing.T) {
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	localDir := t.TempDir()

	// Add then remove.
	if err := runAdapterAdd(&bytes.Buffer{}, manifestPath, cacheDir, localDir, "svc", false); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAdapterRemove(&out, manifestPath, "svc"); err != nil {
		t.Fatalf("runAdapterRemove: %v", err)
	}

	m, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Services["svc"]; ok {
		t.Error("service should have been removed")
	}
	if !strings.Contains(strings.ToLower(out.String()), "removed") {
		t.Errorf("output should mention 'removed': %q", out.String())
	}
}

func TestAdapterRemoveAbsent(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	// Write a minimal manifest.
	if err := manifest.Save(&manifest.Manifest{
		Version:  1,
		Network:  manifest.Network{Mode: "port", BasePort: 8000},
		Services: map[string]manifest.Service{},
	}, manifestPath); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAdapterRemove(&out, manifestPath, "nope"); err != nil {
		t.Fatalf("runAdapterRemove on absent service should not error: %v", err)
	}
	if !strings.Contains(strings.ToLower(out.String()), "no service") {
		t.Errorf("output should mention 'no service': %q", out.String())
	}
}

func TestAdapterRemoveNoManifest(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	var out bytes.Buffer
	if err := runAdapterRemove(&out, manifestPath, "nope"); err != nil {
		t.Fatalf("should not error: %v", err)
	}
	if !strings.Contains(strings.ToLower(out.String()), "no manifest") {
		t.Errorf("output should mention 'no manifest': %q", out.String())
	}
}

// =========================================================================
// list
// =========================================================================

func TestAdapterListLocalSource(t *testing.T) {
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	localDir := t.TempDir()

	if err := runAdapterAdd(&bytes.Buffer{}, manifestPath, cacheDir, localDir, "local-svc", false); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAdapterList(&out, manifestPath, cacheDir); err != nil {
		t.Fatalf("runAdapterList: %v", err)
	}

	outStr := out.String()
	if !strings.Contains(outStr, "local-svc") {
		t.Errorf("list should show local-svc: %q", outStr)
	}
	// The local dir exists, so it should show as cached.
	if !strings.Contains(strings.ToLower(outStr), "cached") {
		t.Errorf("list should show cache as present: %q", outStr)
	}
}

func TestAdapterListGitSourceCached(t *testing.T) {
	cloneURL := writeFixtureGitRepo(t)
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")

	// Seed the cache with a git clone, then write the manifest entry.
	spec := seedGitCache(t, cacheDir, cloneURL, "localhost", "fixture")
	m := &manifest.Manifest{
		Version:  1,
		Network:  manifest.Network{Mode: "port", BasePort: 8000},
		Services: map[string]manifest.Service{"git-svc": {Adapter: spec}},
	}
	if err := manifest.Save(m, manifestPath); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAdapterList(&out, manifestPath, cacheDir); err != nil {
		t.Fatalf("runAdapterList: %v", err)
	}

	outStr := out.String()
	if !strings.Contains(outStr, "git-svc") {
		t.Errorf("list should show git-svc: %q", outStr)
	}
	// The cache was seeded, so it should show as cached.
	if !strings.Contains(strings.ToLower(outStr), "cached") {
		t.Errorf("list should show cache as cached: %q", outStr)
	}
}

func TestAdapterListGitSourceAbsent(t *testing.T) {
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")

	// Write a git source spec to the manifest but don't seed the cache.
	m := &manifest.Manifest{
		Version:  1,
		Network:  manifest.Network{Mode: "port", BasePort: 8000},
		Services: map[string]manifest.Service{"git-svc": {Adapter: "git:github.com/user/repo"}},
	}
	if err := manifest.Save(m, manifestPath); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAdapterList(&out, manifestPath, cacheDir); err != nil {
		t.Fatalf("runAdapterList: %v", err)
	}

	outStr := strings.ToLower(out.String())
	if !strings.Contains(outStr, "git-svc") {
		t.Errorf("list should show git-svc: %q", outStr)
	}
	if !strings.Contains(outStr, "absent") {
		t.Errorf("list should show cache as absent: %q", outStr)
	}
}

func TestAdapterListEmptyManifest(t *testing.T) {
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	// Write empty manifest.
	if err := manifest.Save(&manifest.Manifest{
		Version:  1,
		Network:  manifest.Network{Mode: "port", BasePort: 8000},
		Services: map[string]manifest.Service{},
	}, manifestPath); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAdapterList(&out, manifestPath, cacheDir); err != nil {
		t.Fatalf("runAdapterList: %v", err)
	}
	// Should not error; may show "no services".
	if !strings.Contains(strings.ToLower(out.String()), "no services") {
		t.Errorf("list should show 'no services': %q", out.String())
	}
}

func TestAdapterListNoManifest(t *testing.T) {
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	var out bytes.Buffer
	if err := runAdapterList(&out, manifestPath, cacheDir); err != nil {
		t.Fatalf("should not error: %v", err)
	}
	if !strings.Contains(strings.ToLower(out.String()), "no manifest") {
		t.Errorf("output should mention 'no manifest': %q", out.String())
	}
}

func TestAdapterListMultipleServices(t *testing.T) {
	cloneURL := writeFixtureGitRepo(t)
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")

	// Seed a git cache entry.
	gitSpec := seedGitCache(t, cacheDir, cloneURL, "localhost", "fixture")

	// Add a local source.
	localDir := t.TempDir()
	if err := runAdapterAdd(&bytes.Buffer{}, manifestPath, cacheDir, localDir, "local-svc", false); err != nil {
		t.Fatal(err)
	}

	// Add the git source to the manifest by loading, adding, and saving.
	m, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	m.Services["git-svc"] = manifest.Service{Adapter: gitSpec}
	if err := manifest.Save(m, manifestPath); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAdapterList(&out, manifestPath, cacheDir); err != nil {
		t.Fatalf("runAdapterList: %v", err)
	}

	outStr := out.String()
	if !strings.Contains(outStr, "git-svc") {
		t.Errorf("list should show git-svc: %q", outStr)
	}
	if !strings.Contains(outStr, "local-svc") {
		t.Errorf("list should show local-svc: %q", outStr)
	}
}

// =========================================================================
// update
// =========================================================================

func TestAdapterUpdateGitSource(t *testing.T) {
	cloneURL := writeFixtureGitRepo(t)
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")

	// Seed the cache and write the manifest entry.
	spec := seedGitCache(t, cacheDir, cloneURL, "localhost", "fixture")
	if err := manifest.Save(&manifest.Manifest{
		Version:  1,
		Network:  manifest.Network{Mode: "port", BasePort: 8000},
		Services: map[string]manifest.Service{"git-svc": {Adapter: spec}},
	}, manifestPath); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAdapterUpdate(&out, manifestPath, cacheDir, "git-svc"); err != nil {
		t.Fatalf("runAdapterUpdate: %v", err)
	}

	if !strings.Contains(strings.ToLower(out.String()), "reconciled") {
		t.Errorf("output should mention 'reconciled': %q", out.String())
	}
}

func TestAdapterUpdateAllGitServices(t *testing.T) {
	cloneURL := writeFixtureGitRepo(t)
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")

	// Seed two git cache entries.
	spec1 := seedGitCache(t, cacheDir, cloneURL, "localhost", "fixture-a")
	spec2 := seedGitCache(t, cacheDir, cloneURL, "localhost", "fixture-b")

	if err := manifest.Save(&manifest.Manifest{
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 8000},
		Services: map[string]manifest.Service{
			"svc-a": {Adapter: spec1},
			"svc-b": {Adapter: spec2},
		},
	}, manifestPath); err != nil {
		t.Fatal(err)
	}

	// Update with no name → updates all git services.
	var out bytes.Buffer
	if err := runAdapterUpdate(&out, manifestPath, cacheDir, ""); err != nil {
		t.Fatalf("runAdapterUpdate: %v", err)
	}

	outStr := strings.ToLower(out.String())
	if !strings.Contains(outStr, "svc-a") || !strings.Contains(outStr, "svc-b") {
		t.Errorf("update should mention both services: %q", out.String())
	}
	// Should reconcile 2 services.
	if !strings.Contains(outStr, "reconciled 2") {
		t.Errorf("should report reconciling 2 services: %q", out.String())
	}
}

func TestAdapterUpdateLocalSkipped(t *testing.T) {
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")

	// Add a local source.
	localDir := t.TempDir()
	if err := runAdapterAdd(&bytes.Buffer{}, manifestPath, cacheDir, localDir, "local-svc", false); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAdapterUpdate(&out, manifestPath, cacheDir, "local-svc"); err != nil {
		t.Fatalf("runAdapterUpdate on local source: %v", err)
	}
	// Should not error; should mention "skipped".
	outStr := strings.ToLower(out.String())
	if !strings.Contains(outStr, "skipped") {
		t.Errorf("update should mention 'skipped' for local source: %q", out.String())
	}
}

func TestAdapterUpdateAllSkipsLocal(t *testing.T) {
	cloneURL := writeFixtureGitRepo(t)
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")

	// One git source, one local source.
	gitSpec := seedGitCache(t, cacheDir, cloneURL, "localhost", "fixture")
	localDir := t.TempDir()
	if err := runAdapterAdd(&bytes.Buffer{}, manifestPath, cacheDir, localDir, "local-svc", false); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	m.Services["git-svc"] = manifest.Service{Adapter: gitSpec}
	if err := manifest.Save(m, manifestPath); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAdapterUpdate(&out, manifestPath, cacheDir, ""); err != nil {
		t.Fatalf("runAdapterUpdate: %v", err)
	}

	outStr := strings.ToLower(out.String())
	// Git service should be reconciled, local should be skipped.
	if !strings.Contains(outStr, "git-svc") || !strings.Contains(outStr, "reconciled") {
		t.Errorf("update should reconcile git-svc: %q", out.String())
	}
	if !strings.Contains(outStr, "local-svc") || !strings.Contains(outStr, "skipped") {
		t.Errorf("update should skip local-svc: %q", out.String())
	}
	// Only 1 reconciled (the git one).
	if !strings.Contains(outStr, "reconciled 1") {
		t.Errorf("should report reconciling 1 service: %q", out.String())
	}
}

func TestAdapterUpdateAbsentService(t *testing.T) {
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	if err := manifest.Save(&manifest.Manifest{
		Version:  1,
		Network:  manifest.Network{Mode: "port", BasePort: 8000},
		Services: map[string]manifest.Service{},
	}, manifestPath); err != nil {
		t.Fatal(err)
	}

	err := runAdapterUpdate(&bytes.Buffer{}, manifestPath, cacheDir, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent service in update")
	}
}

func TestAdapterUpdateGitPinnedRef(t *testing.T) {
	// Create a fixture repo with a tag, seed the cache, and reconcile.
	dir := t.TempDir()
	requireGitCmd(t)
	runGitCmd(t, dir, "init")
	runGitCmd(t, dir, "config", "user.email", "test@example.com")
	runGitCmd(t, dir, "config", "user.name", "Test")
	runGitCmd(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, dir, "add", ".")
	runGitCmd(t, dir, "commit", "-m", "initial commit")
	runGitCmd(t, dir, "tag", "v1.0")
	cloneURL := "file://" + dir

	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")

	// Seed the cache pinned to v1.0.
	cache, err := adapterdist.OpenCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	src := &adapterdist.Source{Kind: "git", URL: cloneURL, Host: "localhost", Path: "pinned", Ref: "v1.0"}
	if _, _, err := cache.Ensure(src); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Write manifest with the pinned spec.
	spec := "git:localhost/pinned@v1.0"
	if err := manifest.Save(&manifest.Manifest{
		Version:  1,
		Network:  manifest.Network{Mode: "port", BasePort: 8000},
		Services: map[string]manifest.Service{"git-svc": {Adapter: spec}},
	}, manifestPath); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAdapterUpdate(&out, manifestPath, cacheDir, "git-svc"); err != nil {
		t.Fatalf("runAdapterUpdate: %v", err)
	}

	// After reconcile, HEAD should still be at the tag.
	cacheDir2 := filepath.Join(cacheDir, "git", "localhost", "pinned")
	sha := runGitCmdOut(t, cacheDir2, "rev-parse", "HEAD")
	tagSha := runGitCmdOut(t, dir, "rev-parse", "v1.0")
	if sha != tagSha {
		t.Errorf("after reconcile HEAD = %q, want tag sha %q", sha, tagSha)
	}
}

// =========================================================================
// subcommand registration
// =========================================================================

func TestAdapterAddSubcommandRegistered(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"adapter", "add"})
	if err != nil {
		t.Fatalf("could not find 'adapter add': %v", err)
	}
	if cmd.Name() != "add" {
		t.Fatalf("command name = %q, want %q", cmd.Name(), "add")
	}
}

func TestAdapterRemoveSubcommandRegistered(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"adapter", "remove"})
	if err != nil {
		t.Fatalf("could not find 'adapter remove': %v", err)
	}
	if cmd.Name() != "remove" {
		t.Fatalf("command name = %q, want %q", cmd.Name(), "remove")
	}
}

func TestAdapterListSubcommandRegistered(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"adapter", "list"})
	if err != nil {
		t.Fatalf("could not find 'adapter list': %v", err)
	}
	if cmd.Name() != "list" {
		t.Fatalf("command name = %q, want %q", cmd.Name(), "list")
	}
}

func TestAdapterUpdateSubcommandRegistered(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"adapter", "update"})
	if err != nil {
		t.Fatalf("could not find 'adapter update': %v", err)
	}
	if cmd.Name() != "update" {
		t.Fatalf("command name = %q, want %q", cmd.Name(), "update")
	}
}

// =========================================================================
// flag / env integration (cobra command path)
// =========================================================================

func TestAdapterAddRespectsCacheDirFlag(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	cacheDir := t.TempDir()
	localDir := t.TempDir()

	root := NewRootCmd()
	root.SetArgs([]string{
		"adapter", "add", localDir, "svc",
		"--manifest", manifestPath,
		"--cache-dir", cacheDir,
	})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The service should be in the manifest.
	m, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatalf("manifest.Load: %v", err)
	}
	if _, ok := m.Services["svc"]; !ok {
		t.Errorf("service 'svc' not in manifest")
	}
}

func TestAdapterListRespectsCacheDirEnv(t *testing.T) {
	cacheDir := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "stunt.yaml")
	localDir := t.TempDir()

	// Add first.
	if err := runAdapterAdd(&bytes.Buffer{}, manifestPath, cacheDir, localDir, "svc", false); err != nil {
		t.Fatal(err)
	}

	t.Setenv("STUNT_ADAPTER_CACHE", cacheDir)

	root := NewRootCmd()
	root.SetArgs([]string{"adapter", "list", "--manifest", manifestPath})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestAdapterAddNoManifestFlagUsesDefault(t *testing.T) {
	// Verify that the --manifest flag is inherited from the root persistent flag.
	root := NewRootCmd()
	addCmd, _, err := root.Find([]string{"adapter", "add"})
	if err != nil {
		t.Fatal(err)
	}
	flag := addCmd.Flag("manifest")
	if flag == nil {
		t.Fatal("adapter add should inherit --manifest flag")
	}
	// --cache-dir is a persistent flag on the adapter parent.
	cacheFlag := addCmd.Flag("cache-dir")
	if cacheFlag == nil {
		t.Fatal("adapter add should inherit --cache-dir flag")
	}
}

// =========================================================================
// name derivation
// =========================================================================

func TestDeriveServiceName(t *testing.T) {
	cases := []struct {
		kind string
		url  string
		path string
		want string
	}{
		{"git", "github.com/user/my-repo", "user/my-repo", "my-repo"},
		{"git", "github.com/org/sub/deep", "org/sub/deep", "deep"},
		{"local", "/abs/path/to/adapter", "", "adapter"},
		{"local", "./rel/path", "", "path"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/%s", tc.kind, tc.want), func(t *testing.T) {
			src := &adapterdist.Source{Kind: tc.kind, URL: tc.url, Path: tc.path}
			got := deriveServiceName(src)
			if got != tc.want {
				t.Errorf("deriveServiceName = %q, want %q", got, tc.want)
			}
		})
	}
}
