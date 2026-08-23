// Package astscan statically derives per-endpoint behavior tags from an
// adapter's Starlark scripts: which handlers read the request body, gate
// auth, persist state, paginate, emit webhooks, and so on.
//
// The tags are DERIVED facts about the code ("what the handler does"),
// not conformance results — nothing here proves a client observed the
// behavior. Consumers must label them as static/derived (genmatrix does).
//
// Attribution follows the engine's own module seam: scripts/lib.star is
// preloaded into every handler script as predeclared globals
// (internal/engine getOrLoadVM), so a handler's effective body is its own
// def plus same-file helpers it calls plus lib.star defs those call —
// resolved to a fixpoint, never crossing into other script files. Handlers
// are house-style `def on_x(req)`; a differently-named parameter is not
// recognized (acceptable: all reference adapters use `req`).
package astscan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.starlark.net/syntax"

	"stuntapi.com/stunt/internal/adapter"
)

// Tag is one statically derived behavior signal. The set is closed; the
// order below is the canonical emission order.
type Tag string

const (
	TagBody     Tag = "body"     // reads req body
	TagQuery    Tag = "query"    // reads query params
	TagParams   Tag = "params"   // reads path params
	TagAuth     Tag = "auth"     // bearer gate (401/403 path or Authorization header read)
	TagStateful Tag = "stateful" // store_collection / store_kv_* / store_blob
	TagPaginate Tag = "paginate" // paginate builtin
	TagFilter   Tag = "filter"   // query_select builtin
	TagWebhooks Tag = "webhooks" // events_emit*
	TagErrors   Tag = "errors"   // respond with a 4xx/5xx literal
	TagClock    Tag = "clock"    // clock.now_* (time-derived state)
)

var canonicalTags = []Tag{TagBody, TagQuery, TagParams, TagAuth, TagStateful, TagPaginate, TagFilter, TagWebhooks, TagErrors, TagClock}

// EndpointTags is the derived tag set for one manifest endpoint.
type EndpointTags struct {
	Method  string
	Route   string
	Handler string
	Tags    []Tag
}

// Scan derives tags for every endpoint of a loaded adapter, in manifest
// order. It fails loudly — a script that will not parse, or a handler
// function that cannot be found, is a broken adapter the boot guards
// should have caught; a scan that silently skipped it would corrupt the
// matrix with "no behaviors".
func Scan(a *adapter.Adapter) ([]EndpointTags, error) {
	files := &fileCache{}

	out := make([]EndpointTags, 0, len(a.Endpoints))
	total := 0
	for _, ep := range a.Endpoints {
		path, fn := adapter.SplitHandler(ep.Handler)
		if path == "" || fn == "" {
			return nil, fmt.Errorf("astscan: %s: endpoint %s %q has no script handler", a.ID, ep.Method, ep.Route)
		}
		script, err := files.get(path)
		if err != nil {
			return nil, fmt.Errorf("astscan: %s: %w", a.ID, err)
		}
		def, ok := script.defs[fn]
		if !ok {
			return nil, fmt.Errorf("astscan: %s: %s: handler %q not defined", a.ID, path, fn)
		}

		tags, err := closureTags(files, script, def)
		if err != nil {
			return nil, fmt.Errorf("astscan: %s: %w", a.ID, err)
		}
		total += len(tags)
		out = append(out, EndpointTags{Method: ep.Method, Route: ep.Route, Handler: ep.Handler, Tags: tags})
	}
	if total == 0 && len(a.Endpoints) > 0 {
		// Every real adapter at least stores, gates auth, or shapes an
		// error; zero tags across the board means the scan went wrong,
		// not that the adapter is trivial. GraphQL/gRPC-only adapters
		// with no HTTP endpoints legitimately yield nothing here.
		return nil, fmt.Errorf("astscan: %s: no tags derived from any endpoint — scan is wrong, not the adapter", a.ID)
	}
	return out, nil
}

// fileCache parses each script once per Scan and remembers its top-level
// defs.
type fileCache struct {
	files map[string]*scriptAST
}

type scriptAST struct {
	path  string
	defs  map[string]*syntax.DefStmt
	isLib bool
}

func (c *fileCache) get(path string) (*scriptAST, error) {
	if c.files == nil {
		c.files = map[string]*scriptAST{}
	}
	if s, ok := c.files[path]; ok {
		return s, nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Mode 0: parse only, no resolution — matches the QC all-scripts-parse
	// guard (the resolver would reject e.g. while-loops the VM allows).
	f, err := syntax.Parse(path, src, 0)
	if err != nil {
		return nil, err
	}
	s := &scriptAST{path: path, defs: map[string]*syntax.DefStmt{}, isLib: filepath.Base(path) == "lib.star"}
	for _, stmt := range f.Stmts {
		if d, ok := stmt.(*syntax.DefStmt); ok {
			s.defs[d.Name.Name] = d
		}
	}
	c.files[path] = s
	return s, nil
}

// closureTags walks the handler def plus every same-file or lib.star def
// it (transitively) calls, collecting tags. The worklist is bounded by the
// def count of two files, so the fixpoint terminates; defs in other
// scripts are never entered (the engine never preloads them).
func closureTags(files *fileCache, entry *scriptAST, def *syntax.DefStmt) ([]Tag, error) {
	lib, libErr := files.get(filepath.Join(filepath.Dir(entry.path), "lib.star"))
	if libErr != nil && !os.IsNotExist(libErr) {
		return nil, libErr
	}

	set := map[Tag]bool{}
	visited := map[string]bool{}
	var walk func(script *scriptAST, name string)
	walk = func(script *scriptAST, name string) {
		key := script.path + "#" + name
		if visited[key] {
			return
		}
		visited[key] = true
		d, ok := script.defs[name]
		if !ok {
			return
		}
		var calls []string
		collectTags(d, set, &calls)
		for _, callee := range calls {
			// A same-file def shadows a predeclared lib def, matching
			// Starlark scoping.
			if _, ok := script.defs[callee]; ok {
				walk(script, callee)
				continue
			}
			if lib != nil && libErr == nil {
				if _, ok := lib.defs[callee]; ok {
					walk(lib, callee)
				}
			}
		}
	}
	walk(entry, def.Name.Name)

	var out []Tag
	for _, t := range canonicalTags {
		if set[t] {
			out = append(out, t)
		}
	}
	return out, nil
}

// collectTags walks one def body, recording tag evidence and the plain
// helper calls it makes (callee idents resolved by the caller).
func collectTags(def *syntax.DefStmt, set map[Tag]bool, calls *[]string) {
	walk(def, func(n syntax.Node) {
		switch e := n.(type) {
		case *syntax.IndexExpr:
			noteIndexExpr(e, set)
		case *syntax.DotExpr:
			if base, ok := dotBase(e); ok && base == "req" {
				noteReqKey(set, e.Name.Name)
			}
		case *syntax.CallExpr:
			switch fn := e.Fn.(type) {
			case *syntax.Ident:
				noteBuiltin(set, fn.Name, e.Args)
				*calls = append(*calls, fn.Name)
			case *syntax.DotExpr:
				// req.get("body") method form.
				if base, ok := dotBase(fn); ok && base == "req" && fn.Name.Name == "get" {
					if len(e.Args) > 0 {
						if lit, ok := e.Args[0].(*syntax.Literal); ok && lit.Token == syntax.STRING {
							if s, ok := lit.Value.(string); ok {
								noteReqKey(set, s)
							}
						}
					}
				}
				// clock.now_* reads.
				if base, ok := dotBase(fn); ok && base == "clock" && strings.HasPrefix(fn.Name.Name, "now_") {
					set[TagClock] = true
				}
			}
		}
	})
}

// walk is a full-tree visitor replacing syntax.Walk, which has no
// WhileStmt case and panics on while-loops (the walker predates them;
// the VM accepts them, and reference adapters use them).
func walk(n syntax.Node, f func(syntax.Node)) {
	if n == nil {
		return
	}
	f(n)
	switch n := n.(type) {
	case *syntax.File:
		for _, s := range n.Stmts {
			walk(s, f)
		}
	case *syntax.ExprStmt:
		walk(n.X, f)
	case *syntax.BranchStmt, *syntax.Ident, *syntax.Literal:
		// leaves
	case *syntax.IfStmt:
		walk(n.Cond, f)
		for _, s := range n.True {
			walk(s, f)
		}
		for _, s := range n.False {
			walk(s, f)
		}
	case *syntax.ForStmt:
		walk(n.Vars, f)
		walk(n.X, f)
		for _, s := range n.Body {
			walk(s, f)
		}
	case *syntax.WhileStmt:
		walk(n.Cond, f)
		for _, s := range n.Body {
			walk(s, f)
		}
	case *syntax.AssignStmt:
		walk(n.LHS, f)
		walk(n.RHS, f)
	case *syntax.DefStmt:
		walk(n.Name, f)
		for _, p := range n.Params {
			walk(p, f)
		}
		for _, s := range n.Body {
			walk(s, f)
		}
	case *syntax.ReturnStmt:
		walk(n.Result, f)
	case *syntax.LoadStmt:
		walk(n.Module, f)
	case *syntax.ListExpr:
		for _, x := range n.List {
			walk(x, f)
		}
	case *syntax.CondExpr:
		walk(n.Cond, f)
		walk(n.True, f)
		walk(n.False, f)
	case *syntax.IndexExpr:
		walk(n.X, f)
		walk(n.Y, f)
	case *syntax.DictEntry:
		walk(n.Key, f)
		walk(n.Value, f)
	case *syntax.SliceExpr:
		walk(n.X, f)
		walk(n.Lo, f)
		walk(n.Hi, f)
		walk(n.Step, f)
	case *syntax.Comprehension:
		walk(n.Body, f)
		for _, c := range n.Clauses {
			walk(c, f)
		}
	case *syntax.IfClause:
		walk(n.Cond, f)
	case *syntax.ForClause:
		walk(n.Vars, f)
		walk(n.X, f)
	case *syntax.TupleExpr:
		for _, x := range n.List {
			walk(x, f)
		}
	case *syntax.DictExpr:
		for _, e := range n.List {
			walk(e, f)
		}
	case *syntax.UnaryExpr:
		walk(n.X, f)
	case *syntax.BinaryExpr:
		walk(n.X, f)
		walk(n.Y, f)
	case *syntax.DotExpr:
		walk(n.X, f)
		walk(n.Name, f)
	case *syntax.CallExpr:
		walk(n.Fn, f)
		for _, a := range n.Args {
			walk(a, f)
		}
	case *syntax.LambdaExpr:
		for _, p := range n.Params {
			walk(p, f)
		}
		walk(n.Body, f)
	case *syntax.ParenExpr:
		walk(n.X, f)
	}
}

// noteIndexExpr handles subscript forms: req["body"] and
// req.headers["Authorization"].
func noteIndexExpr(e *syntax.IndexExpr, set map[Tag]bool) {
	lit, ok := e.Y.(*syntax.Literal)
	if !ok || lit.Token != syntax.STRING {
		return
	}
	key, _ := lit.Value.(string)
	if id, ok := e.X.(*syntax.Ident); ok && id.Name == "req" {
		noteReqKey(set, key)
		return
	}
	if dot, ok := e.X.(*syntax.DotExpr); ok {
		if base, ok := dotBase(dot); ok && base == "req" && dot.Name.Name == "headers" && strings.EqualFold(key, "authorization") {
			set[TagAuth] = true
		}
	}
}

// dotBase returns the root ident of a DotExpr chain (req.body → "req").
func dotBase(e *syntax.DotExpr) (string, bool) {
	if id, ok := e.X.(*syntax.Ident); ok {
		return id.Name, true
	}
	return "", false
}

func noteReqKey(set map[Tag]bool, key string) {
	switch key {
	case "body":
		set[TagBody] = true
	case "query":
		set[TagQuery] = true
	case "params":
		set[TagParams] = true
	}
}

func noteBuiltin(set map[Tag]bool, name string, args []syntax.Expr) {
	switch {
	case name == "paginate":
		set[TagPaginate] = true
	case name == "query_select":
		set[TagFilter] = true
	case strings.HasPrefix(name, "store_"):
		set[TagStateful] = true
	case strings.HasPrefix(name, "events_emit"):
		set[TagWebhooks] = true
	case name == "respond":
		if len(args) > 0 {
			if lit, ok := args[0].(*syntax.Literal); ok && lit.Token == syntax.INT {
				if code, ok := lit.Value.(int64); ok && code >= 400 {
					if code == 401 || code == 403 {
						// Auth-gate responses own the auth tag only —
						// errors then means validation/not-found shaping,
						// which every gated endpoint would otherwise
						// drown out.
						set[TagAuth] = true
					} else {
						set[TagErrors] = true
					}
				}
			}
		}
	}
}

// TagsString renders a tag set in canonical order for display.
func TagsString(tags []Tag) string {
	parts := make([]string, len(tags))
	for i, t := range tags {
		parts[i] = string(t)
	}
	return strings.Join(parts, ", ")
}
