package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stunt-adapters/stunt/internal/adapterdist"
	"github.com/stunt-adapters/stunt/internal/catalog"
	"github.com/stunt-adapters/stunt/internal/manifest"
)

// defaultCacheDir returns the default adapter cache root (~/.stunt/adapters).
func defaultCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".stunt", "adapters")
}

// resolveCacheDir resolves the adapter cache directory from (in order):
//  1. the --cache-dir flag (if non-empty),
//  2. the STUNT_ADAPTER_CACHE env var (if non-empty),
//  3. the default ~/.stunt/adapters.
func resolveCacheDir(cmd *cobra.Command) string {
	if cacheDir, _ := cmd.Flags().GetString("cache-dir"); cacheDir != "" {
		return cacheDir
	}
	if env := os.Getenv("STUNT_ADAPTER_CACHE"); env != "" {
		return env
	}
	return defaultCacheDir()
}

// resolveCatalogName checks whether sourceSpec is a bare adapter name that
// matches a catalog entry. If so, it returns the git source spec from the
// catalog (e.g. "git:github.com/stunt-adapters/stripe-style@v0.1.0").
// If the spec looks like a path, URL, or git shorthand, it is returned
// unchanged.
func resolveCatalogName(sourceSpec string) string {
	// If it looks like a path, URL, or git shorthand, don't resolve.
	if strings.ContainsAny(sourceSpec, "/") || strings.HasPrefix(sourceSpec, ".") {
		return sourceSpec
	}
	if strings.Contains(sourceSpec, "://") || strings.HasPrefix(sourceSpec, "git:") {
		return sourceSpec
	}
	// Bare name — try the bundled catalog (no network needed).
	entry, err := catalog.GetBundled(sourceSpec)
	if err != nil {
		return sourceSpec // not in catalog — treat as local path
	}
	return catalogEntryToSpec(entry)
}

// catalogEntryToSpec converts a catalog Entry to a git source spec string
// (e.g. "git:github.com/stunt-adapters/stripe-style@v0.1.0").
func catalogEntryToSpec(e catalog.Entry) string {
	u := e.GitURL
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "git://")
	spec := "git:" + u
	if e.LatestRef != "" {
		spec += "@" + e.LatestRef
	}
	return spec
}

// --- subcommand constructors ---

func newAdapterAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <source> [name]",
		Short: "Add an adapter source to the manifest",
		Long: `Add an adapter source spec to stunt.yaml and ensure it is fetchable.

The source can be a git URL, a local path, or a catalog name:
  git:github.com/user/repo          shorthand, head-at-install
  git:github.com/user/repo@v1.0     shorthand, pinned ref
  https://github.com/user/repo@v1   protocol URL
  ./local/adapter                   local filesystem path
  stripe-style                      catalog name (resolved to git source)

The source spec (not the cache path) is stored in the manifest so the
declaration stays portable. If [name] is omitted it is derived from the
repo name (git) or directory basename (local).

On success the adapter is fetched into the cache and the service is added
(or updated with --force).`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			manifestPath, _ := cmd.Flags().GetString("manifest")
			cacheDir := resolveCacheDir(cmd)
			name := ""
			if len(args) > 1 {
				name = args[1]
			}
			force, _ := cmd.Flags().GetBool("force")
			sourceSpec := resolveCatalogName(args[0])
			return runAdapterAdd(cmd.OutOrStdout(), manifestPath, cacheDir, sourceSpec, name, force)
		},
	}
	cmd.Flags().Bool("force", false, "overwrite an existing service with the same name")
	return cmd
}

func newAdapterRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an adapter service from the manifest",
		Long: `Remove a service from stunt.yaml. This only deletes the declaration —
the cache (if any) is left untouched. If the service does not exist, a
clear message is printed and the command exits successfully.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			manifestPath, _ := cmd.Flags().GetString("manifest")
			return runAdapterRemove(cmd.OutOrStdout(), manifestPath, args[0])
		},
	}
}

func newAdapterListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List adapter services in the manifest",
		Long: `List services declared in stunt.yaml with their adapter source and
whether the cached copy is present on disk.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			manifestPath, _ := cmd.Flags().GetString("manifest")
			cacheDir := resolveCacheDir(cmd)
			return runAdapterList(cmd.OutOrStdout(), manifestPath, cacheDir)
		},
	}
}

func newAdapterUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update [name]",
		Short: "Re-fetch adapter sources for git-backed services",
		Long: `Re-fetch (reconcile) each git-backed adapter service to the latest
commits for its pinned ref. This does NOT bump to a newer ref — it pulls
the latest commits reachable from the pinned ref (useful for branches).

If [name] is omitted, all git-backed services are reconciled. Local-path
services are skipped (they are always live).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			manifestPath, _ := cmd.Flags().GetString("manifest")
			cacheDir := resolveCacheDir(cmd)
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			return runAdapterUpdate(cmd.OutOrStdout(), manifestPath, cacheDir, name)
		},
	}
}

// --- run functions ---

// runAdapterAdd parses the source spec, ensures it is fetchable, and records
// it in the manifest as services.<name>.adapter.
func runAdapterAdd(out interface{ Write([]byte) (int, error) }, manifestPath, cacheDir, sourceSpec, name string, force bool) error {
	src, err := adapterdist.ParseSource(sourceSpec)
	if err != nil {
		return fmt.Errorf("adapter add: %w", err)
	}

	cache, err := adapterdist.OpenCache(cacheDir)
	if err != nil {
		return fmt.Errorf("adapter add: %w", err)
	}

	// Ensure the source is fetchable (git: clone+checkout; local: resolve path).
	if _, _, err := cache.Ensure(context.Background(), src); err != nil {
		return fmt.Errorf("adapter add: fetch %s: %w", src.String(), err)
	}

	// Derive the service name if not provided.
	if name == "" {
		name = deriveServiceName(src)
	}

	// Load or create the manifest.
	m, err := loadOrCreateManifest(manifestPath)
	if err != nil {
		return err
	}

	// Check for collision.
	if _, exists := m.Services[name]; exists && !force {
		return fmt.Errorf("adapter add: service %q already exists (use --force to overwrite)", name)
	}

	// Record the source spec.
	if m.Services == nil {
		m.Services = make(map[string]manifest.Service)
	}
	svc := m.Services[name] // preserve existing rules/config if present
	svc.Adapter = src.String()
	m.Services[name] = svc

	if err := manifest.Save(m, manifestPath); err != nil {
		return fmt.Errorf("adapter add: %w", err)
	}

	fmt.Fprintf(out, "added %s  adapter: %s\n", name, src.String())
	return nil
}

// runAdapterRemove deletes a service from the manifest.
func runAdapterRemove(out interface{ Write([]byte) (int, error) }, manifestPath, name string) error {
	m, err := manifest.Load(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "no manifest at %s — nothing to remove\n", manifestPath)
			return nil
		}
		return fmt.Errorf("adapter remove: %w", err)
	}

	if _, exists := m.Services[name]; !exists {
		fmt.Fprintf(out, "no service %q in manifest — nothing to remove\n", name)
		return nil
	}

	delete(m.Services, name)
	if err := manifest.Save(m, manifestPath); err != nil {
		return fmt.Errorf("adapter remove: %w", err)
	}
	fmt.Fprintf(out, "removed %s\n", name)
	return nil
}

// runAdapterList prints services with their adapter source and cache status.
func runAdapterList(out interface{ Write([]byte) (int, error) }, manifestPath, cacheDir string) error {
	m, err := manifest.Load(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "no manifest at %s\n", manifestPath)
			return nil
		}
		return fmt.Errorf("adapter list: %w", err)
	}

	if len(m.Services) == 0 {
		fmt.Fprintf(out, "no services in manifest\n")
		return nil
	}

	cache, err := adapterdist.OpenCache(cacheDir)
	if err != nil {
		return fmt.Errorf("adapter list: %w", err)
	}

	// Deterministic order.
	names := make([]string, 0, len(m.Services))
	for n := range m.Services {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		svc := m.Services[name]
		if svc.Adapter == "" {
			fmt.Fprintf(out, "  %-20s  (rules only, no adapter)\n", name)
			continue
		}
		src, err := adapterdist.ParseSource(svc.Adapter)
		if err != nil {
			fmt.Fprintf(out, "  %-20s  (invalid source %q: %v)\n", name, svc.Adapter, err)
			continue
		}
		cachePath := cache.PathFor(src)
		cached := "absent"
		if _, statErr := os.Stat(cachePath); statErr == nil {
			cached = "cached"
		}
		fmt.Fprintf(out, "  %-20s  %-10s  %s\n", name, cached, src.String())
	}
	return nil
}

// runAdapterUpdate reconciles git-backed services to the latest commits for
// their pinned ref (does NOT bump the ref).
func runAdapterUpdate(out interface{ Write([]byte) (int, error) }, manifestPath, cacheDir, name string) error {
	m, err := manifest.Load(manifestPath)
	if err != nil {
		return fmt.Errorf("adapter update: %w", err)
	}

	cache, err := adapterdist.OpenCache(cacheDir)
	if err != nil {
		return fmt.Errorf("adapter update: %w", err)
	}

	// Build the list of (name, source) pairs to reconcile.
	type target struct {
		name string
		src  *adapterdist.Source
	}
	var targets []target

	if name != "" {
		svc, exists := m.Services[name]
		if !exists {
			return fmt.Errorf("adapter update: service %q not found in manifest", name)
		}
		src, err := adapterdist.ParseSource(svc.Adapter)
		if err != nil {
			return fmt.Errorf("adapter update: service %q has invalid source %q: %w", name, svc.Adapter, err)
		}
		targets = append(targets, target{name, src})
	} else {
		// All services, deterministic order.
		names := make([]string, 0, len(m.Services))
		for n := range m.Services {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			svc := m.Services[n]
			if svc.Adapter == "" {
				continue
			}
			src, err := adapterdist.ParseSource(svc.Adapter)
			if err != nil {
				fmt.Fprintf(out, "  warning: service %q has invalid source %q: %v\n", n, svc.Adapter, err)
				continue
			}
			targets = append(targets, target{n, src})
		}
	}

	reconciled := 0
	for _, tgt := range targets {
		if tgt.src.Kind == "local" {
			fmt.Fprintf(out, "  %-20s  skipped (local path)\n", tgt.name)
			continue
		}
		if err := cache.Reconcile(context.Background(), tgt.src); err != nil {
			fmt.Fprintf(out, "  %-20s  error: %v\n", tgt.name, err)
			continue
		}
		reconciled++
		fmt.Fprintf(out, "  %-20s  reconciled  %s\n", tgt.name, tgt.src.String())
	}
	fmt.Fprintf(out, "reconciled %d service(s)\n", reconciled)
	return nil
}

// --- helpers ---

// deriveServiceName infers a service name from a source: the repo name for
// git sources, or the directory basename for local sources.
func deriveServiceName(src *adapterdist.Source) string {
	if src.Kind == "git" {
		// Path is like "user/repo" — take the last segment.
		path := src.Path
		if path == "" {
			// Fall back to parsing the URL.
			path = src.URL
		}
		return lastSegment(path)
	}
	// Local path: use the directory basename.
	return lastSegment(src.URL)
}

// lastSegment returns the part after the last '/'.
func lastSegment(s string) string {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return s[i+1:]
		}
	}
	return s
}

// loadOrCreateManifest loads an existing manifest or creates a new minimal
// one (version:1, no services) if the file does not exist.
func loadOrCreateManifest(manifestPath string) (*manifest.Manifest, error) {
	m, err := manifest.Load(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &manifest.Manifest{
				Version:  1,
				Services: make(map[string]manifest.Service),
			}, nil
		}
		return nil, fmt.Errorf("load manifest %s: %w", manifestPath, err)
	}
	if m.Services == nil {
		m.Services = make(map[string]manifest.Service)
	}
	return m, nil
}
