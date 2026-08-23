package sdkmap

import (
	"fmt"
	"regexp"
	"strings"
)

// Stone declares routes as bare names inside a namespace, not as HTTP
// paths or verbs: `route list_folder (Arg, Result, Error)` under
// `namespace files` is served at POST /2/files/list_folder. The arg/result
// types, attrs block (host/style/scope) and docstring all live on the
// lines below the declaration — only the opening line matters here.
var (
	stoneRouteRe     = regexp.MustCompile(`^route\s+([A-Za-z0-9_/]+(?::[0-9]+)?)\s*\(`)
	stoneNamespaceRe = regexp.MustCompile(`^namespace\s+([A-Za-z0-9_]+)\s*$`)
	stoneVersionRe   = regexp.MustCompile(`^([A-Za-z0-9_/]+):([0-9]+)$`)
)

// ParseStone extracts the route inventory from a Dropbox Stone IDL file
// (dropbox-api-spec). Stone carries no HTTP verb and API v2 is RPC-style —
// every endpoint, content-host uploads and downloads included, is POST —
// so every route is emitted as POST (the artifact's source string states
// the choice). Non-route constructs (struct/union/alias/import/attrs/doc
// lines) are skipped by design; a route line that fails to parse, a route
// before any namespace, and 0 routes found are hard errors.
func ParseStone(data []byte) ([]Route, error) {
	namespace := ""
	var out []Route
	for i, line := range strings.Split(string(data), "\n") {
		if m := stoneNamespaceRe.FindStringSubmatch(line); m != nil {
			namespace = m[1]
			continue
		}
		m := stoneRouteRe.FindStringSubmatch(line)
		if m == nil {
			continue // struct/union/alias/import/attrs/doc — not a route
		}
		if namespace == "" {
			return nil, fmt.Errorf("sdkmap: stone route %q before any namespace (line %d)", m[1], i+1)
		}
		out = append(out, Route{Method: "POST", Path: "/2/" + namespace + "/" + stonePath(m[1])})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("sdkmap: stone file carries 0 routes — layout changed?")
	}
	return out, nil
}

// stonePath renders a route name as its served path segment: Stone's
// version suffix `name:2` is metadata, and Dropbox spells the versioned
// endpoint as a suffix on the last segment (search:2 -> search_v2,
// copy_batch/check:2 -> copy_batch/check_v2).
func stonePath(name string) string {
	if m := stoneVersionRe.FindStringSubmatch(name); m != nil {
		return m[1] + "_v" + m[2]
	}
	return name
}
