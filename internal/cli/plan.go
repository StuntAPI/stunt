package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stunt-adapters/stunt/internal/adapter"
	"github.com/stunt-adapters/stunt/internal/adapter/runtime"
	"github.com/stunt-adapters/stunt/internal/adapterdist"
	"github.com/stunt-adapters/stunt/internal/manifest"
	"github.com/stunt-adapters/stunt/internal/starlark"
)

const defaultManifestPath = "stunt.yaml"

// planResult holds the per-service validation outcome for plan output.
type planResult struct {
	endpoints   int              // number of adapter endpoints (0 for rules-only)
	grpcMethods int              // number of gRPC methods (0 if no gRPC section)
	wsRoutes    int              // number of WebSocket routes (0 if none)
	rules       int              // number of rules on the service
	loadError   error            // non-nil if the adapter could not be loaded
	adapter     *adapter.Adapter // loaded adapter (nil on error or rules-only)
}

func newPlanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "plan",
		Short: "Validate the manifest and show what would run",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("manifest")
			m, err := manifest.Load(path)
			if err != nil {
				return fmt.Errorf("load %s: %w", path, err)
			}
			if err := manifest.Validate(m); err != nil {
				return err
			}
			m.Network.Defaults()

			out := cmd.OutOrStdout()

			// Warn about unknown manifest fields (typos like `netwrok:`).
			for _, field := range m.UnknownFields {
				fmt.Fprintf(out, "  WARNING: unknown manifest field %q (may be a typo)\n", field)
			}

			// Resolve and validate each adapter so plan surfaces load errors
			// before `stunt up` crashes. A non-loadable adapter prints a
			// WARNING but does not abort — the rest of the plan is still
			// reported.
			manifestDir := filepath.Dir(path)
			results := planValidateAdapters(out, m, manifestDir)

			fmt.Fprintf(out, "stunt.yaml OK — %d service(s):\n", len(m.Services))

			switch m.Network.Mode {
			case "subdomain":
				printPlanSubdomain(out, m, results)
			default:
				printPlanPort(out, m, results)
			}
			return nil
		},
	}
}

// planValidateAdapters attempts to load each adapter-backed service. It
// returns a map of service name → planResult containing the loaded adapter
// (or load error). Non-loadable adapters are printed as WARNING lines so
// the user sees the problem before running `stunt up`.
func planValidateAdapters(out interface{ Write([]byte) (int, error) }, m *manifest.Manifest, manifestDir string) map[string]planResult {
	results := make(map[string]planResult, len(m.Services))
	for _, name := range sortedServiceNames(m.Services) {
		svc := m.Services[name]
		rules := len(svc.Rules)
		if svc.Adapter == "" {
			results[name] = planResult{rules: rules}
			continue
		}
		a, err := planResolveAdapter(svc.Adapter, manifestDir)
		if err != nil {
			results[name] = planResult{rules: rules, loadError: err}
			fmt.Fprintf(out, "  WARNING: service %q: %v\n", name, err)
			continue
		}
		grpcMethods := 0
		if a.Grpc != nil {
			grpcMethods = len(a.Grpc.Methods)
		}
		results[name] = planResult{
			endpoints:   len(a.Endpoints),
			grpcMethods: grpcMethods,
			wsRoutes:    len(a.Websockets),
			rules:       rules,
			adapter:     a,
		}

		// Pre-compile handler scripts so plan catches missing files,
		// syntax errors, and undefined functions before `stunt up`.
		planCheckHandlers(out, name, a)
	}
	return results
}

// planResolveAdapter resolves the adapter source spec to a local directory
// and attempts to load it. This reuses the same resolution logic as the
// engine (git sources are fetched, local paths are resolved relative to
// the manifest dir) but does NOT open any state stores — it only validates
// that the adapter is loadable.
func planResolveAdapter(spec, manifestDir string) (*adapter.Adapter, error) {
	src, err := adapterdist.ParseSource(spec)
	if err != nil {
		return nil, fmt.Errorf("adapter %q: %w", spec, err)
	}
	var dir string
	if src.Kind == "git" {
		cacheRoot := defaultAdapterCacheRoot()
		cache, err := adapterdist.OpenCache(cacheRoot)
		if err != nil {
			return nil, fmt.Errorf("adapter %q: open cache: %w", spec, err)
		}
		localDir, _, err := cache.Ensure(context.Background(), src)
		if err != nil {
			return nil, fmt.Errorf("adapter %q: fetch: %w", spec, err)
		}
		dir = localDir
	} else {
		dir = src.URL
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(manifestDir, dir)
		}
	}
	a, err := adapter.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("adapter %q: %w", spec, err)
	}
	return a, nil
}

// planCheckHandlers attempts to compile each handler script referenced by
// the adapter (HTTP endpoints, gRPC methods, WebSocket routes). It catches
// missing script files, syntax errors, and undefined handler functions —
// problems that would otherwise only surface as a 500 at request time.
// Failures are printed as WARNING lines; plan never aborts.
func planCheckHandlers(out interface{ Write([]byte) (int, error) }, serviceName string, a *adapter.Adapter) {
	// Dummy builtins so name resolution passes during compilation — handler
	// scripts reference store_collection etc. which must resolve at compile
	// time even though we never call them.
	dummyBuiltins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{})

	check := func(spec string) {
		if spec == "" {
			return
		}
		scriptPath, fnName := adapter.SplitHandler(spec)
		if scriptPath == "" {
			return
		}

		// Show the path relative to the adapter dir for a cleaner message.
		display := spec
		if rel, err := filepath.Rel(a.Dir, scriptPath); err == nil {
			display = rel
			if fnName != "" {
				display += "#" + fnName
			}
		}

		src, err := os.ReadFile(scriptPath)
		if err != nil {
			fmt.Fprintf(out, "  WARNING: service %q: handler %s: %v\n", serviceName, display, err)
			return
		}
		vm, err := starlark.Load(string(src), dummyBuiltins)
		if err != nil {
			fmt.Fprintf(out, "  WARNING: service %q: handler %s: %v\n", serviceName, display, err)
			return
		}
		if fnName != "" && !vm.Has(fnName) {
			fmt.Fprintf(out, "  WARNING: service %q: handler %s: function %q is not defined\n", serviceName, display, fnName)
		}
	}

	for _, ep := range a.Endpoints {
		check(ep.Handler)
	}
	if a.Grpc != nil {
		for _, m := range a.Grpc.Methods {
			check(m.Handler)
		}
	}
	for _, ws := range a.Websockets {
		check(ws.Handler)
	}
}

func printPlanPort(out interface{ Write([]byte) (int, error) }, m *manifest.Manifest, results map[string]planResult) {
	port := m.Network.BasePort
	for _, name := range sortedServiceNames(m.Services) {
		svc := m.Services[name]
		r := results[name]
		if svc.Adapter != "" {
			fmt.Fprintf(out, "  %s  ->  127.0.0.1:%d  %s\n", name, port, adapterSummary(svc.Adapter, r))
		} else {
			fmt.Fprintf(out, "  %s  ->  127.0.0.1:%d  (%d rules)\n", name, port, r.rules)
		}
		port++
	}
}

func printPlanSubdomain(out interface{ Write([]byte) (int, error) }, m *manifest.Manifest, results map[string]planResult) {
	tld := m.Network.TLD
	if tld == "" {
		tld = "localhost"
	}
	for _, name := range sortedServiceNames(m.Services) {
		svc := m.Services[name]
		r := results[name]
		if svc.Adapter != "" {
			fmt.Fprintf(out, "  %s  ->  https://%s.%s  %s\n", name, name, tld, adapterSummary(svc.Adapter, r))
		} else {
			fmt.Fprintf(out, "  %s  ->  https://%s.%s  (%d rules)\n", name, name, tld, r.rules)
		}
	}
}

// adapterSummary renders the parenthesised summary for an adapter-backed
// service. When the adapter loaded successfully it shows endpoint, gRPC
// method, WebSocket route, and rule counts (e.g. "(adapter: ./x, 11
// endpoints, 4 grpc methods, 1 ws route, 2 rules)"). gRPC and WS counts are
// omitted when zero so HTTP-only adapters stay concise. When loading failed
// it shows the adapter spec with a warning marker.
func adapterSummary(spec string, r planResult) string {
	if r.loadError != nil {
		return fmt.Sprintf("(adapter: %s, NOT LOADABLE — see WARNING above)", spec)
	}
	parts := []string{fmt.Sprintf("%d endpoints", r.endpoints)}
	if r.grpcMethods > 0 {
		parts = append(parts, fmt.Sprintf("%d grpc methods", r.grpcMethods))
	}
	if r.wsRoutes > 0 {
		parts = append(parts, fmt.Sprintf("%d ws routes", r.wsRoutes))
	}
	parts = append(parts, fmt.Sprintf("%d rules", r.rules))
	return fmt.Sprintf("(adapter: %s, %s)", spec, strings.Join(parts, ", "))
}

// defaultAdapterCacheRoot returns the adapter cache root, honoring the
// STUNT_ADAPTER_CACHE environment variable, falling back to
// ~/.stunt/adapters.
func defaultAdapterCacheRoot() string {
	if root := os.Getenv("STUNT_ADAPTER_CACHE"); root != "" {
		return root
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), ".stunt", "adapters")
	}
	return filepath.Join(home, ".stunt", "adapters")
}

func sortedServiceNames(services map[string]manifest.Service) []string {
	names := make([]string, 0, len(services))
	for n := range services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
