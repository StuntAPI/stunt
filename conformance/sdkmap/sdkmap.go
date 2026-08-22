// Package sdkmap extracts provider route tables from the SDKs the
// conformance suites already pin, and diffs them against adapter routes.
// The real API's surface — as encoded by its own official client — becomes
// ground truth that cannot drift: the tables ship inside the pinned
// dependency, so an SDK bump refreshes them with no manual step and no
// network at generation time.
package sdkmap

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Route is one provider route. Path keeps the provider's own spelling for
// display; matching happens on the normalized form.
type Route struct {
	Method string `json:"method"`
	Path   string `json:"route"`
	norm   string
}

// AdapterRoute is an adapter manifest endpoint offered up for matching.
// Method "" is the manifest's any-method wildcard.
type AdapterRoute struct {
	Method string
	Path   string
}

// Result is the diff of a provider table against an adapter's routes.
type Result struct {
	Provider int
	Covered  int
	Missing  []Route
}

// ErrNodeModules reports that conformance/node/node_modules is absent: the
// node extractors refuse to run rather than silently return an empty table.
var ErrNodeModules = fmt.Errorf("conformance/node/node_modules not found — run `just conformance-node` first")

// Extract pulls the provider route table named by kind ("google" service
// dir under the google-api-go-client module, or "node" npm package) from
// the sources under confDir (the conformance/ directory).
func Extract(kind, ref, confDir string) ([]Route, error) {
	var routes []Route
	var err error
	switch kind {
	case "google":
		routes, err = googleRoutes(confDir, ref)
	case "node":
		routes, err = nodeRoutes(confDir, ref)
	default:
		return nil, fmt.Errorf("sdkmap: unknown source kind %q", kind)
	}
	if err != nil {
		return nil, err
	}
	// An empty table means the walker lost contact with the source layout —
	// never a "covers everything" result.
	if len(routes) == 0 {
		return nil, fmt.Errorf("sdkmap: %s %s yielded 0 routes — upstream layout changed?", kind, ref)
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Path != routes[j].Path {
			return routes[i].Path < routes[j].Path
		}
		return routes[i].Method < routes[j].Method
	})
	return routes, nil
}

// Diff reports how much of the provider table the adapter covers. A
// provider route is covered when the adapter serves the same normalized
// path and either the same method or a wildcard (empty-method) route.
// Spec routes whose params carry literal suffixes ({id}.json, {x}:verb)
// also match a bare-param adapter path: the engine lets one param segment
// capture dot-bearing text, so /Messages/{sid} really does serve
// /Messages/SM123.json requests — the suffix lives in the handler.
func Diff(table []Route, adapter []AdapterRoute) Result {
	type key struct{ norm, method string }
	have := map[string]map[string]bool{}
	for _, r := range adapter {
		n := Normalize(r.Path)
		if have[n] == nil {
			have[n] = map[string]bool{}
		}
		have[n][strings.ToUpper(strings.TrimSpace(r.Method))] = true
	}
	seen := map[key]bool{}
	res := Result{Provider: len(table)}
	for _, r := range table {
		k := key{r.normOf(), strings.ToUpper(r.Method)}
		if seen[k] {
			continue
		}
		seen[k] = true
		if methods, ok := have[r.normOf()]; ok && (methods[k.method] || methods[""]) {
			res.Covered++
			continue
		}
		if methods, ok := have[NormalizeLoose(r.Path)]; ok && (methods[k.method] || methods[""]) {
			res.Covered++
			continue
		}
		res.Missing = append(res.Missing, r)
	}
	return res
}

func (r *Route) normOf() string {
	if r.norm == "" {
		r.norm = Normalize(r.Path)
	}
	return r.norm
}

var (
	// queryRe drops a query string before normalization: discovery "path"
	// values may carry ?alt=media.
	queryRe = regexp.MustCompile(`\?.*$`)
	// tmplAnyRe matches any JS template interpolation; displayPath rewrites
	// it into brace form using the identifier it ultimately parameterizes.
	tmplAnyRe = regexp.MustCompile(`\$\{([^}]+)\}`)
)

// Normalize canonicalizes a route path for matching: every parameter
// spelling ({x}, {+x}, %v, ${x}) becomes *, a parameter with a literal
// suffix keeps it ({id}.json -> *.json), runs of * collapse to a single *
// (multi-segment {+x} wildcards and discovery's {x}/{x1}/{x2} expansions
// both degrade to one), and literals (users/me, @me) and :verb suffixes
// stay verbatim.
func Normalize(p string) string {
	p = queryRe.ReplaceAllString(strings.TrimSpace(p), "")
	segs := strings.Split(strings.Trim(p, "/"), "/")
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		s = normSegment(s)
		if s == "*" && len(out) > 0 && out[len(out)-1] == "*" {
			continue
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return "/" + strings.Join(out, "/")
}

func normSegment(s string) string {
	if s == "%v" {
		return "*"
	}
	if strings.HasPrefix(s, "{") || strings.HasPrefix(s, "${") {
		if j := strings.IndexByte(s, '}'); j >= 0 {
			return "*" + s[j+1:]
		}
	}
	return s
}

// NormalizeLoose drops the literal suffixes of param segments
// ({id}.json -> *, {x}:runReport -> *) — the form a bare-param adapter
// route would match, per the engine's dot-capturing params.
func NormalizeLoose(p string) string {
	p = queryRe.ReplaceAllString(strings.TrimSpace(p), "")
	segs := strings.Split(strings.Trim(p, "/"), "/")
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		s = normSegment(s)
		if i := strings.IndexByte(s, '*'); i >= 0 {
			s = "*"
		}
		if s == "*" && len(out) > 0 && out[len(out)-1] == "*" {
			continue
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return "/" + strings.Join(out, "/")
}

// displayPath rewrites template interpolations into brace form using the
// identifier they parameterize: ${encodeURIComponent(id)} and
// ${parameters.issueIdOrKey} both collapse to their final identifier.
func displayPath(p string) string {
	return tmplAnyRe.ReplaceAllStringFunc(p, func(m string) string {
		body := strings.TrimSpace(m[2 : len(m)-1])
		if strings.ContainsAny(body, "()") {
			if i, j := strings.LastIndexByte(body, '('), strings.LastIndexByte(body, ')'); i >= 0 && j > i {
				body = strings.TrimSpace(body[i+1 : j])
			}
		}
		if i := strings.LastIndexByte(body, '.'); i >= 0 {
			body = body[i+1:]
		}
		return "{" + body + "}"
	})
}

// nodeModulesDir is where the node suites install their SDKs.
func nodeModulesDir(confDir string) string {
	return filepath.Join(confDir, "node", "node_modules")
}
