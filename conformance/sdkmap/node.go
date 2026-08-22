package sdkmap

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// nodeRoutes dispatches to the per-package walker. Each walker reads the
// route table the SDK itself embeds (codegen literals or a generated
// endpoint table) — shapes verified against the pinned versions.
func nodeRoutes(confDir, pkg string) ([]Route, error) {
	root := filepath.Join(nodeModulesDir(confDir), filepath.FromSlash(pkg))
	if _, err := os.Stat(root); err != nil {
		return nil, ErrNodeModules
	}
	walk, ok := nodeWalkers[pkg]
	if !ok {
		return nil, fmt.Errorf("sdkmap: no node walker for %q", pkg)
	}
	return walk(root)
}

type nodeWalker func(root string) ([]Route, error)

var nodeWalkers = map[string]nodeWalker{
	"@hubspot/api-client":                   hubspotRoutes,
	"@slack/web-api":                        slackRoutes,
	"@octokit/plugin-rest-endpoint-methods": octokitRoutes,
	"jira.js":                               jiraRoutes,
	"openai":                                openaiRoutes,
	"plaid":                                 plaidRoutes,
	"resend":                                resendRoutes,
	"square":                                squareRoutes,
	"stripe":                                stripeRoutes,
	"twilio":                                twilioRoutes,
}

// walkJS visits every .js/.ts file under root (skipping .map/.d.ts),
// in deterministic order.
func walkJS(root string, fn func(path, src string)) error {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".ts") || strings.HasSuffix(name, ".mjs") {
			if !strings.HasSuffix(name, ".map") && !strings.HasPrefix(name, ".") {
				files = append(files, p)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		fn(f, string(data))
	}
	return nil
}

type routeSet struct {
	seen  map[string]bool
	route map[string]Route
}

func newRouteSet() *routeSet {
	return &routeSet{seen: map[string]bool{}, route: map[string]Route{}}
}

func (s *routeSet) add(method, path string) {
	if method == "" || path == "" || !strings.HasPrefix(path, "/") {
		return
	}
	k := strings.ToUpper(method) + " " + path
	if s.seen[k] {
		return
	}
	s.seen[k] = true
	s.route[k] = Route{Method: strings.ToUpper(method), Path: path}
}

func (s *routeSet) list() []Route {
	out := make([]Route, 0, len(s.route))
	for _, r := range s.route {
		out = append(out, r)
	}
	return out
}

// octokit ships GitHub's OpenAPI as a generated literal table:
// "POST /repos/{owner}/{repo}/...".
var octokitRe = regexp.MustCompile(`"(GET|POST|PUT|PATCH|DELETE|HEAD) (/[^"]+)"`)

func octokitRoutes(root string) ([]Route, error) {
	s := newRouteSet()
	err := walkJS(root, func(_, src string) {
		for _, m := range octokitRe.FindAllStringSubmatch(src, -1) {
			s.add(m[1], m[2])
		}
	})
	return s.list(), err
}

// stripe pairs _makeRequest verbs with literal or template paths.
var stripeRe = regexp.MustCompile("_makeRequest\\('([A-Z]+)', ('[^']+'|`[^`]+`)")

func stripeRoutes(root string) ([]Route, error) {
	s := newRouteSet()
	err := walkJS(root, func(_, src string) {
		for _, m := range stripeRe.FindAllStringSubmatch(src, -1) {
			p := strings.Trim(m[2], "'`")
			s.add(m[1], displayPath(p))
		}
	})
	return s.list(), err
}

// twilio: verbs live next to uri assignments inside each generated class
// or closure; attributing them per container keeps collection/list uris
// (create=post, page=list=get) distinct from instance uris (fetch=get,
// update=post, remove=delete). Only the classic /2010-04-01 API is
// extracted — the other lib/rest products are separate services the
// adapter does not simulate.
var (
	twilioContainerRe = regexp.MustCompile(`^(?:class|function)\s+(\w+)`)
	twilioURIRe       = regexp.MustCompile(`^\s*(?:this|instance)\._uri\s*=\s*` + "`([^`]+)`")
	twilioMethRe      = regexp.MustCompile(`method:\s*"(\w+)"`)
)

func twilioRoutes(root string) ([]Route, error) {
	apiDir := filepath.Join(root, "lib", "rest", "api", "v2010")
	if _, err := os.Stat(apiDir); err != nil {
		return nil, fmt.Errorf("sdkmap: twilio: %w", err)
	}
	s := newRouteSet()
	err := filepath.WalkDir(apiDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".js") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		container := ""
		uris := map[string][]string{} // container -> uri templates
		verbs := map[string]map[string]bool{}
		for _, line := range strings.Split(string(src), "\n") {
			if m := twilioContainerRe.FindStringSubmatch(line); m != nil {
				container = m[1]
				continue
			}
			if m := twilioURIRe.FindStringSubmatch(line); m != nil && container != "" {
				if !contains(uris[container], m[1]) {
					uris[container] = append(uris[container], m[1])
				}
			}
			if m := twilioMethRe.FindStringSubmatch(line); m != nil && container != "" {
				if verbs[container] == nil {
					verbs[container] = map[string]bool{}
				}
				verbs[container][strings.ToUpper(m[1])] = true
			}
		}
		for c, list := range uris {
			for _, u := range list {
				for v := range verbs[c] {
					s.add(v, "/2010-04-01"+displayPath(u))
				}
			}
		}
		return nil
	})
	return s.list(), err
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// slack: every Web API method is POST /api/<name>; the method list is a
// literal table of dotted names in dist/methods.js (scanning other dist
// files picks up unrelated dotted strings).
var slackRe = regexp.MustCompile(`'([a-z][a-zA-Z0-9]*(?:\.[a-zA-Z0-9]+)+)'`)

func slackRoutes(root string) ([]Route, error) {
	s := newRouteSet()
	err := walkJS(filepath.Join(root, "dist", "methods.js"), func(_, src string) {
		for _, m := range slackRe.FindAllStringSubmatch(src, -1) {
			s.add("POST", "/api/"+m[1])
		}
	})
	return s.list(), err
}

// jira.js: url: '<path>' or a template literal, adjacent to
// method: '<VERB>'.
var jiraRe = regexp.MustCompile(`url:\s*('([^']+)'|` + "`([^`]+)`" + `)(?s).{0,120}?method:\s*'([A-Z]+)'`)

func jiraRoutes(root string) ([]Route, error) {
	s := newRouteSet()
	err := walkJS(root, func(_, src string) {
		for _, m := range jiraRe.FindAllStringSubmatch(src, -1) {
			path := m[2]
			if path == "" {
				path = displayPath(m[3])
			}
			s.add(m[4], path)
		}
	})
	return s.list(), err
}

// hubspot: localVarPath literals paired with the HttpMethod token used in
// the adjacent request context.
var hubspotRe = regexp.MustCompile(`localVarPath = '([^']+)'(?s).{0,200}?HttpMethod\.(GET|POST|PUT|PATCH|DELETE)`)

func hubspotRoutes(root string) ([]Route, error) {
	s := newRouteSet()
	err := walkJS(root, func(_, src string) {
		for _, m := range hubspotRe.FindAllStringSubmatch(src, -1) {
			s.add(m[2], m[1])
		}
	})
	return s.list(), err
}

// plaid: backtick localVarPath templates paired with the method literal in
// the request options that follow (the URL-construction block sits between
// them, so the window is generous).
var plaidRe = regexp.MustCompile("localVarPath = `([^`]+)`(?s).{0,600}?method: '(GET|POST|PUT|DELETE|PATCH)'")

func plaidRoutes(root string) ([]Route, error) {
	s := newRouteSet()
	err := walkJS(root, func(_, src string) {
		for _, m := range plaidRe.FindAllStringSubmatch(src, -1) {
			s.add(m[2], m[1])
		}
	})
	return s.list(), err
}

// square: the error-handler call carries the verb and full path as two
// adjacent string literals.
var squareRe = regexp.MustCompile(`"(GET|POST|PUT|PATCH|DELETE)", "(/v2/[^"]+)"`)

func squareRoutes(root string) ([]Route, error) {
	s := newRouteSet()
	err := walkJS(root, func(_, src string) {
		for _, m := range squareRe.FindAllStringSubmatch(src, -1) {
			s.add(m[1], m[2])
		}
	})
	return s.list(), err
}

// openai: this._client.<verb>('<path>'), the tagged-template form
// this._client.<verb>((0, path_1.path) `/x/${id}`), or getAPIList for
// paginated GETs — verb from the callee. The client's baseUrl carries the
// /v1 version, so extracted paths get it prepended (that is how requests
// hit the wire).
var (
	openaiRe   = regexp.MustCompile(`this\._client\.(get|post|put|patch|delete)\(\s*(?:\(0, path_1\.path\)\s*)?('([^']+)'|` + "`([^`]+)`" + `)`)
	openaiList = regexp.MustCompile(`this\._client\.getAPIList\(\s*('([^']+)'|` + "`([^`]+)`" + `)`)
)

func openaiRoutes(root string) ([]Route, error) {
	s := newRouteSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// internal/ holds the transport plumbing, not resource routes.
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".map") || strings.Contains(p, "/internal/") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range openaiRe.FindAllStringSubmatch(string(src), -1) {
			path := m[3]
			if path == "" {
				path = displayPath(m[4])
			}
			s.add(strings.ToUpper(m[1]), "/v1"+path)
		}
		for _, m := range openaiList.FindAllStringSubmatch(string(src), -1) {
			path := m[2]
			if path == "" {
				path = displayPath(m[3])
			}
			s.add("GET", "/v1"+path)
		}
		return nil
	})
	return s.list(), err
}

// resend: this.resend.<verb>("<path>").
var resendRe = regexp.MustCompile(`this\.resend\.(get|post|patch|delete)\("([^"]+)"`)

func resendRoutes(root string) ([]Route, error) {
	s := newRouteSet()
	err := walkJS(root, func(_, src string) {
		for _, m := range resendRe.FindAllStringSubmatch(src, -1) {
			s.add(m[1], m[2])
		}
	})
	return s.list(), err
}
