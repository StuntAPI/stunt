// Package adapter defines the on-disk format for a stunt adapter: an
// adapter.yaml manifest plus convention directories (endpoints/, scripts/,
// fixtures/, templates/, schemas/). Load reads an adapter directory and
// returns a populated *Adapter with resolved paths.
package adapter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stunt-adapters/stunt/internal/rules"
)

// Adapter is the parsed in-memory representation of an adapter directory.
type Adapter struct {
	// Dir is the absolute path to the adapter directory on disk (set by Load).
	Dir string `yaml:"-"`

	ID         string              `yaml:"id"`
	Name       string              `yaml:"name"`
	RealHosts  []string            `yaml:"real_hosts"`
	Version    string              `yaml:"version"`
	Endpoints  []Endpoint          `yaml:"endpoints"`
	Resources  []Resource          `yaml:"resources"`
	Rules      []rules.Rule        `yaml:"rules"`
	Identity   *Identity           `yaml:"identity"`
	Grpc       *GrpcSpec           `yaml:"grpc"`
	Websockets []WebsocketEndpoint `yaml:"ws"`
}

// WebsocketEndpoint declares a WebSocket route served by a connection-
// lifetime Starlark handler. The handler is invoked once per WebSocket
// connection and receives a `ws` object with recv()/send()/close() methods.
type WebsocketEndpoint struct {
	Route        string   `yaml:"route"`
	Handler      string   `yaml:"handler"`
	Subprotocols []string `yaml:"subprotocols"`
}

// GrpcSpec declares an optional gRPC service served from a protobuf
// FileDescriptorSet. Each method is routed to a Starlark handler function.
// When Grpc is nil the adapter is HTTP-only.
type GrpcSpec struct {
	// Service is the fully-qualified protobuf service name, e.g.
	// "stunt.test.Greeter".
	Service string `yaml:"service"`

	// Descriptor is the path to a compiled FileDescriptorSet (.desc file),
	// relative to the adapter directory. Resolved to absolute by Load.
	Descriptor string `yaml:"descriptor"`

	// Methods maps bare gRPC method names (e.g. "SayHello") to Starlark
	// handler specs (e.g. "scripts/greeter.star#on_say_hello").
	Methods []GrpcMethod `yaml:"methods"`
}

// GrpcMethod maps a single gRPC method to a Starlark handler function.
type GrpcMethod struct {
	Name    string `yaml:"name"`    // bare method name, e.g. "SayHello"
	Handler string `yaml:"handler"` // "scripts/greeter.star#on_say_hello"
}

// Endpoint is one route declaration in adapter.yaml. An endpoint may either
// declare a Starlark handler (stateful) or use the declarative rules engine
// (rules overlay) — or both.
type Endpoint struct {
	Route   string       `yaml:"route"`
	Method  string       `yaml:"method"`  // HTTP verb, or "" for any
	Handler string       `yaml:"handler"` // "scripts/x.star#on_post"
	Rules   []rules.Rule `yaml:"rules"`
}

// Resource declares a backing store the adapter's handlers can use.
type Resource struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind"` // "collection" | "kv"
	Seed string `yaml:"seed"` // optional path to a JSONL fixture
}

// Identity is a placeholder for auth scheme metadata (no behavior yet).
type Identity struct {
	TokenScheme string `yaml:"token_scheme"`
}

// ReadFile reads a file referenced by a relative path (relative to the
// adapter directory). It rejects paths that escape the adapter directory via
// traversal (e.g. ../../etc/passwd) to prevent unauthorized file access (I4).
func (a *Adapter) ReadFile(rel string) ([]byte, error) {
	full := rel
	if !filepath.IsAbs(rel) {
		full = filepath.Join(a.Dir, rel)
	}
	// Security: verify the cleaned path is within a.Dir (I4).
	cleanPath := filepath.Clean(full)
	relChecked, err := filepath.Rel(a.Dir, cleanPath)
	if err != nil || strings.HasPrefix(relChecked, "..") || filepath.IsAbs(relChecked) {
		return nil, fmt.Errorf("adapter: path %q escapes adapter directory", rel)
	}
	return os.ReadFile(cleanPath)
}

// DescriptorBytes reads the compiled protobuf FileDescriptorSet (.desc) file
// referenced by the gRPC spec. Returns an error if no gRPC spec is declared
// or the descriptor file cannot be read. The path is validated against the
// same directory-containment check used by ReadFile to reject traversal
// attacks (e.g. ../../etc/passwd).
func (a *Adapter) DescriptorBytes() ([]byte, error) {
	if a.Grpc == nil || a.Grpc.Descriptor == "" {
		return nil, fmt.Errorf("adapter: no grpc descriptor configured")
	}
	return a.ReadFile(a.Grpc.Descriptor)
}

// validate checks basic structural invariants after parsing.
func (a *Adapter) validate() error {
	if a.ID == "" {
		return fmt.Errorf("adapter: id is required")
	}
	if a.Grpc != nil {
		if a.Grpc.Service == "" {
			return fmt.Errorf("adapter: grpc.service is required when grpc is declared")
		}
		if a.Grpc.Descriptor == "" {
			return fmt.Errorf("adapter: grpc.descriptor is required when grpc is declared")
		}
		for i, m := range a.Grpc.Methods {
			if m.Name == "" {
				return fmt.Errorf("adapter: grpc.methods[%d].name is required", i)
			}
			if m.Handler == "" {
				return fmt.Errorf("adapter: grpc.methods[%d].handler is required", i)
			}
		}
	}
	// Validate websocket endpoints.
	wsRoutes := make(map[string]bool)
	for i, ws := range a.Websockets {
		if ws.Route == "" {
			return fmt.Errorf("adapter: ws[%d].route is required", i)
		}
		if wsRoutes[ws.Route] {
			return fmt.Errorf("adapter: ws[%d].route %q is duplicated", i, ws.Route)
		}
		if ws.Handler == "" {
			return fmt.Errorf("adapter: ws[%d].handler is required", i)
		}
		if !strings.Contains(ws.Handler, "#") {
			return fmt.Errorf("adapter: ws[%d].handler %q must be in \"scripts/x.star#fn\" form", i, ws.Handler)
		}
		wsRoutes[ws.Route] = true
	}
	return nil
}

// resolveHandlerPaths converts any endpoint handler script path from relative
// (to the adapter dir) to absolute, preserving the "#function" fragment. It
// rejects paths that escape the adapter directory via traversal (e.g.
// ../../etc/passwd), applying the same containment check as ReadFile.
func (a *Adapter) resolveHandlerPaths() error {
	for i := range a.Endpoints {
		h := a.Endpoints[i].Handler
		if h == "" {
			continue
		}
		path, fn := splitHandler(h)
		if path == "" {
			continue
		}
		resolved, err := a.resolveContainedPath(path)
		if err != nil {
			return err
		}
		a.Endpoints[i].Handler = resolved
		if fn != "" {
			a.Endpoints[i].Handler += "#" + fn
		}
	}

	// Resolve gRPC method handler paths (same format as endpoint handlers).
	if a.Grpc != nil {
		for i := range a.Grpc.Methods {
			h := a.Grpc.Methods[i].Handler
			if h == "" {
				continue
			}
			path, fn := splitHandler(h)
			if path == "" {
				continue
			}
			resolved, err := a.resolveContainedPath(path)
			if err != nil {
				return err
			}
			a.Grpc.Methods[i].Handler = resolved
			if fn != "" {
				a.Grpc.Methods[i].Handler += "#" + fn
			}
		}
	}

	// Resolve websocket handler paths (same format as endpoint handlers).
	for i := range a.Websockets {
		h := a.Websockets[i].Handler
		if h == "" {
			continue
		}
		path, fn := splitHandler(h)
		if path == "" {
			continue
		}
		resolved, err := a.resolveContainedPath(path)
		if err != nil {
			return err
		}
		a.Websockets[i].Handler = resolved
		if fn != "" {
			a.Websockets[i].Handler += "#" + fn
		}
	}
	return nil
}

// resolveContainedPath resolves a relative path to an absolute path within
// a.Dir, rejecting paths that escape the adapter directory via traversal.
func (a *Adapter) resolveContainedPath(path string) (string, error) {
	full := path
	if !filepath.IsAbs(path) {
		full = filepath.Join(a.Dir, path)
	}
	cleanPath := filepath.Clean(full)
	rel, err := filepath.Rel(a.Dir, cleanPath)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("adapter: handler script path %q escapes adapter directory", path)
	}
	return cleanPath, nil
}

// splitHandler splits "scripts/x.star#on_post" into ("scripts/x.star", "on_post").
// Exported as SplitHandler for reuse by the engine package (M7).
func splitHandler(h string) (path, fn string) {
	idx := strings.Index(h, "#")
	if idx < 0 {
		return h, ""
	}
	return h[:idx], h[idx+1:]
}

// SplitHandler splits "scripts/x.star#on_post" into ("scripts/x.star", "on_post").
func SplitHandler(h string) (path, fn string) {
	return splitHandler(h)
}
