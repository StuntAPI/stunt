// Command genmatrix generates the repo-root CONFORMANCE.md — one row per
// reference adapter stating which official SDKs (at which versions) the
// conformance suites drive against it, the behaviors they cover, the
// adapter's documented deviations, and how it is verified.
//
// Every column is derived from sources that cannot drift on their own:
//
//	adapters/*/adapter.yaml        via internal/adapter (load re-validates)
//	conformance/*_test.go          Record(t, "sdk", "adapter", "check") literals
//	conformance/node/tests/*       bootAdapter("...") + // ===== section markers
//	conformance/go.mod             Go SDK versions (exact pins)
//	conformance/node/package.json  Node SDK versions (declared floors)
//	conformance/matrix.yaml        the ONE hand-maintained input: per-adapter
//	                               documented deviations. Generation fails if
//	                               an adapter is missing from it, so a new
//	                               adapter cannot ship uncurated.
//
// Run from the repo root via `just conformance-matrix`; CI regenerates and
// fails on drift, so the committed file can never go stale.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"stuntapi.com/stunt/conformance/sdkmap"
	"stuntapi.com/stunt/internal/adapter"
	"stuntapi.com/stunt/internal/adapter/astscan"
)

func main() {
	root := flag.String("root", "..", "repo root (default: run from conformance/)")
	jsonOut := flag.String("json", "", "also write the matrix as JSON to this path (stuntapi.com consumes it)")
	flag.Parse()
	if err := run(*root, *jsonOut); err != nil {
		fmt.Fprintln(os.Stderr, "genmatrix:", err)
		os.Exit(1)
	}
}

func run(root, jsonOut string) error {
	adapters, err := loadAdapters(filepath.Join(root, "adapters"))
	if err != nil {
		return err
	}
	goChecks, err := parseGoRecords(filepath.Join(root, "conformance"))
	if err != nil {
		return err
	}
	nodeChecks, err := parseNodeTests(filepath.Join(root, "conformance", "node", "tests"))
	if err != nil {
		return err
	}
	checks := append(goChecks, nodeChecks...)

	versions, err := loadVersions(filepath.Join(root, "conformance"))
	if err != nil {
		return err
	}
	sdkVer, err := resolveVersions(checks, versions)
	if err != nil {
		return err
	}
	ids := make([]string, len(adapters))
	for i, a := range adapters {
		ids[i] = a.ID
	}
	gaps, err := loadSidecar(filepath.Join(root, "conformance", "matrix.yaml"), ids)
	if err != nil {
		return err
	}
	surfaces, err := deriveSurfaces(adapters, sdkVer, filepath.Join(root, "conformance"))
	if err != nil {
		return err
	}
	derived, err := scanBehaviors(adapters)
	if err != nil {
		return err
	}

	doc, err := render(adapters, checks, sdkVer, gaps, surfaces, derived, root)
	if err != nil {
		return err
	}
	out := filepath.Join(root, "CONFORMANCE.md")
	if err := os.WriteFile(out, []byte(doc), 0o644); err != nil {
		return err
	}
	if jsonOut != "" {
		data, err := renderJSON(adapters, checks, sdkVer, gaps, surfaces, derived, root)
		if err != nil {
			return err
		}
		if err := os.WriteFile(jsonOut, data, 0o644); err != nil {
			return err
		}
		fmt.Printf("genmatrix: wrote %s\n", jsonOut)
	}
	fmt.Printf("genmatrix: wrote %s (%d adapters, %d checks)\n", out, len(adapters), len(checks))
	return nil
}

// ---- sources -----------------------------------------------------------

func loadAdapters(dir string) ([]*adapter.Adapter, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*adapter.Adapter
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(sub, "adapter.yaml")); err != nil {
			continue
		}
		a, err := adapter.Load(sub) // full validation: what `stunt up` would do
		if err != nil {
			return nil, fmt.Errorf("adapter %s: %w (stunt up would refuse it)", e.Name(), err)
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

type check struct {
	SDK     string
	Adapter string
	Name    string
}

var (
	recordRe     = regexp.MustCompile(`Record\(t,\s*"([^"]*)"\s*,\s*"([^"]*)"\s*,\s*"([^"]*)"\)`)
	recordCallRe = regexp.MustCompile(`Record\(\s*t,`)
)

// parseGoRecords extracts Record literals from the Go suites. Non-literal
// calls (fmt.Sprintf...) and line-broken argument lists are invisible to
// the literal regex, so the call count (matched permissively) must equal
// the literal count — any mismatch is a hard error rather than a silently
// missing row entry.
func parseGoRecords(dir string) ([]check, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []check
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		matches := recordRe.FindAllStringSubmatch(string(data), -1)
		total := len(recordCallRe.FindAllString(string(data), -1))
		if len(matches) != total {
			return nil, fmt.Errorf("%s: %d Record calls but only %d literal — genmatrix parses literals only",
				filepath.Base(f), total, len(matches))
		}
		for _, m := range matches {
			out = append(out, check{SDK: m[1], Adapter: m[2], Name: m[3]})
		}
	}
	return out, nil
}

// nodeSuiteSDK maps a node test file to the SDK label its sections are
// attributed to.
var nodeSuiteSDK = map[string]string{
	"bluesky.test.ts":        "atproto",
	"discord.test.ts":        "discord-node",
	"entraid.test.ts":        "microsoft-graph-client",
	"github.test.ts":         "octokit",
	"hubspot.test.ts":        "hubspot-node",
	"jira.test.ts":           "jira-js",
	"llm.test.ts":            "openai-node",
	"microsoftgraph.test.ts": "microsoft-graph-client",
	"plaid.test.ts":          "plaid-node",
	"resend.test.ts":         "resend-node",
	"salesforce.test.ts":     "jsforce",
	"slack.test.ts":          "slack-node",
	"square.test.ts":         "square-node",
	"stripe.test.ts":         "stripe-node",
	"twilio.test.ts":         "twilio-node",
	"zendesk.test.ts":        "node-zendesk",
}

var (
	nodeBootRe = regexp.MustCompile(`bootAdapter\("([^"]+)"\)`)
	nodeSectRe = regexp.MustCompile(`(?s)//\s*=====\s*(.*?)\s*=====`)
	// VM suites share the node marker convention: // ===== name =====
	// blocks in adapters/<name>_style_test.go name the behaviors the
	// engine-level suite verifies (no SDK involved).
	vmSectRe   = regexp.MustCompile(`(?m)^\s*//\s*=====\s*(.*?)\s*=====\s*$`)
	nodeTestRe = regexp.MustCompile(`test\(\s*"([^"]+)"`)
)

// loadVMBehaviors reads the // ===== name ===== section markers from each
// adapter's engine-level suite (adapters/<name>_style_test.go). A suite
// with no markers yields no entries — the convention is additive.
func loadVMBehaviors(root string, adapters []*adapter.Adapter) (map[string][]string, error) {
	out := map[string][]string{}
	for _, a := range adapters {
		name := strings.ReplaceAll(a.ID, "-", "_") + "_test.go"
		data, err := os.ReadFile(filepath.Join(root, "adapters", name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, m := range vmSectRe.FindAllStringSubmatch(string(data), -1) {
			out[a.ID] = append(out[a.ID], collapse(m[1]))
		}
	}
	return out, nil
}

func parseNodeTests(dir string) ([]check, error) { // An unmapped *.test.ts in the dir would silently vanish from the
	// matrix — fail instead, mirroring the sidecar stale-entry check.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".test.ts") && nodeSuiteSDK[name] == "" {
			return nil, fmt.Errorf("%s: no SDK label mapping — add it to nodeSuiteSDK", name)
		}
	}
	// Deterministic order: by filename.
	files := make([]string, 0, len(nodeSuiteSDK))
	for base := range nodeSuiteSDK {
		files = append(files, base)
	}
	sort.Strings(files)
	var out []check
	for _, base := range files {
		data, err := os.ReadFile(filepath.Join(dir, base))
		if err != nil {
			return nil, err
		}
		boots := nodeBootRe.FindAllStringSubmatch(string(data), -1)
		if len(boots) != 1 {
			return nil, fmt.Errorf("%s: %d bootAdapter calls, want exactly 1", base, len(boots))
		}
		adapterID := boots[0][1]
		var names []string
		for _, m := range nodeSectRe.FindAllStringSubmatch(string(data), -1) {
			names = append(names, collapse(m[1]))
		}
		if len(names) == 0 {
			// No section markers: fall back to the enclosing test names
			// (all of them — a second test() block must not vanish).
			for _, m := range nodeTestRe.FindAllStringSubmatch(string(data), -1) {
				names = append(names, m[1])
			}
		}
		if len(names) == 0 {
			return nil, fmt.Errorf("%s: no // ===== sections and no test name found", base)
		}
		for _, n := range names {
			out = append(out, check{SDK: nodeSuiteSDK[base], Adapter: adapterID, Name: n})
		}
	}
	return out, nil
}

var goModLineRe = regexp.MustCompile(`^\t(\S+) (v\S+)( // indirect)?$`)

// loadVersions merges go.mod requires (exact pins) with package.json
// dependencies (declared floors) into one lookup keyed by module path /
// npm package name.
func loadVersions(confDir string) (map[string]string, error) {
	out := map[string]string{}
	gomod, err := os.ReadFile(filepath.Join(confDir, "go.mod"))
	if err != nil {
		return nil, err
	}
	inRequire := false
	for _, line := range strings.Split(string(gomod), "\n") {
		switch {
		case strings.TrimSpace(line) == "require (":
			inRequire = true
		case inRequire && strings.TrimSpace(line) == ")":
			inRequire = false
		case inRequire:
			if m := goModLineRe.FindStringSubmatch(line); m != nil && m[3] == "" {
				out[m[1]] = m[2]
			}
		}
	}
	pj, err := os.ReadFile(filepath.Join(confDir, "node", "package.json"))
	if err != nil {
		return nil, err
	}
	blk := depsBlockRe.FindStringSubmatch(string(pj))
	if blk == nil {
		return nil, fmt.Errorf("%s: no dependencies block", filepath.Join(confDir, "node", "package.json"))
	}
	for _, m := range depLineRe.FindAllStringSubmatch(blk[1], -1) {
		out[m[1]] = strings.TrimPrefix(m[2], "^")
	}
	return out, nil
}

var (
	depsBlockRe = regexp.MustCompile(`(?s)"dependencies":\s*\{(.*?)\}`)
	depLineRe   = regexp.MustCompile(`"([^"]+)":\s*"(\^?[^"]+)"`)
)

type sidecarAdapter struct {
	Deviations []string `yaml:"deviations"`
	Missing    []string `yaml:"missing"`
}

// gapEntry is the curated gap record for one adapter: capabilities of the
// real API that are absent (missing) vs covered-but-different (deviations).
type gapEntry struct {
	Deviations []string
	Missing    []string
}

type sidecar struct {
	Adapters map[string]sidecarAdapter `yaml:"adapters"`
}

// loadSidecar validates that the sidecar covers exactly the given adapter
// ids: a stale entry or a missing adapter is an error, never a silent hole.
func loadSidecar(path string, ids []string) (map[string]gapEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sc sidecar
	if err := yaml.Unmarshal(data, &sc); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	known := map[string]bool{}
	for _, id := range ids {
		known[id] = true
	}
	for id := range sc.Adapters {
		if !known[id] {
			return nil, fmt.Errorf("%s: entry %q matches no adapter — stale entry", path, id)
		}
	}
	out := map[string]gapEntry{}
	for _, id := range ids {
		entry, ok := sc.Adapters[id]
		if !ok {
			return nil, fmt.Errorf("%s: adapter %q has no entry — every adapter must carry deviations and missing lists (empty is fine)", path, id)
		}
		out[id] = gapEntry{Deviations: entry.Deviations, Missing: entry.Missing}
	}
	return out, nil
}

// ---- version resolution -------------------------------------------------

// goModuleBases maps a major-stripped SDK label to its module repo. Labels
// carrying "/vN" (go-github/v89) resolve to repo+vN — the label itself
// pins the major, so bumping an SDK needs no edit here.
var goModuleBases = map[string]string{
	"aws-sdk-go-v2": "github.com/aws/aws-sdk-go-v2",
	"cloudflare-go": "github.com/cloudflare/cloudflare-go",
	"go-github":     "github.com/google/go-github",
	"go-shopify":    "github.com/bold-commerce/go-shopify",
	"stripe-go":     "github.com/stripe/stripe-go",
	"twilio-go":     "github.com/twilio/twilio-go",
	"x/oauth2":      "golang.org/x/oauth2",
	// The google suites label the umbrella module (the data-plane
	// services) or the subpackage (idtoken) — same module either way.
	"google-api-go-client":         "google.golang.org/api",
	"google-api-go-client/idtoken": "google.golang.org/api",
	// go-ethereum is the canonical Go client for JSON-RPC chains (there is
	// no single "provider" — the client library is the standard surface).
	"go-ethereum": "github.com/ethereum/go-ethereum",
}

var nodePackages = map[string]string{
	"discord-node":           "@discordjs/rest",
	"hubspot-node":           "@hubspot/api-client",
	"jira-js":                "jira.js",
	"jsforce":                "jsforce",
	"octokit":                "octokit",
	"openai-node":            "openai",
	"plaid-node":             "plaid",
	"resend-node":            "resend",
	"slack-node":             "@slack/web-api",
	"atproto":                "@atproto/api",
	"microsoft-graph-client": "@microsoft/microsoft-graph-client",
	"node-zendesk":           "node-zendesk",
	"square-node":            "square",
	"stripe-node":            "stripe",
	"twilio-node":            "twilio",
}

// resolveVersions resolves the display version for every SDK label in the
// checks, failing loud on an unmapped label so a new suite cannot silently
// drop out of the matrix.
func resolveVersions(checks []check, versions map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for _, c := range checks {
		if _, done := out[c.SDK]; done {
			continue
		}
		label := c.SDK
		if pkg, ok := nodePackages[label]; ok {
			v, ok := versions[pkg]
			if !ok {
				return nil, fmt.Errorf("package.json has no %q dependency for sdk %q", pkg, label)
			}
			out[label] = v + " (floor)"
			continue
		}
		base, suffix := label, ""
		if i := strings.Index(label, "/v"); i > 0 {
			base, suffix = label[:i], label[i:]
		}
		repo, ok := goModuleBases[base]
		if !ok {
			return nil, fmt.Errorf("sdk label %q has no module mapping — add it to goModuleBases", label)
		}
		v, ok := versions[repo+suffix]
		if !ok {
			return nil, fmt.Errorf("go.mod has no %q require for sdk %q", repo+suffix, label)
		}
		out[label] = v
	}
	return out, nil
}

// ---- derived provider surfaces -------------------------------------------

// surfaceSpec names the route table an adapter's provider surface is
// derived from: a Google service dir inside the google-api-go-client
// module (the Discovery doc ships in the module), an npm package under
// conformance/node (its embedded codegen route table), or a vendored
// spec artifact under conformance/surfaces/ (produced by fetchsurfaces —
// `just surfaces-fetch`). Label resolves the display version via the
// usual maps; vendored specs carry their own provenance string.
type surfaceSpec struct {
	Kind  string // "google" | "node" | "spec"
	Ref   string
	Also  string // optional second google table unioned in (multi-service adapters)
	Label string
}

// surfaceSource intentionally lists only adapters with a trustworthy
// full-API table: embedded in the pinned SDK (Google Discovery docs in
// the module; generated tables in the Node clients) or vendored from the
// provider's own published spec (conformance/surfaces/, via
// `just surfaces-fetch`). cloudflare-go and go-shopify are hand-written
// subsets; zendesk ships minified; salesforce exposes none — salesforce
// stays curated, the rest now ride vendored specs.
var surfaceSource = map[string]surfaceSpec{
	// Google Discovery docs, read from the pinned module.
	"apps-script-style": {Kind: "google", Ref: "script/v1", Label: "google-api-go-client"},
	// ga4 spans two real services: the Data API and the Admin API the
	// adapter's /v1beta aliases serve.
	"ga4-style":            {Kind: "google", Ref: "analyticsdata/v1beta", Also: "analyticsadmin/v1beta", Label: "google-api-go-client"},
	"gcalendar-style":      {Kind: "google", Ref: "calendar/v3", Label: "google-api-go-client"},
	"gdocs-style":          {Kind: "google", Ref: "docs/v1", Label: "google-api-go-client"},
	"gmail-style":          {Kind: "google", Ref: "gmail/v1", Label: "google-api-go-client"},
	"google-admin-style":   {Kind: "google", Ref: "admin/directory/v1", Label: "google-api-go-client"},
	"google-iam-style":     {Kind: "google", Ref: "iam/v1", Label: "google-api-go-client"},
	"gsearchconsole-style": {Kind: "google", Ref: "searchconsole/v1", Label: "google-api-go-client"},
	"gsheets-style":        {Kind: "google", Ref: "sheets/v4", Label: "google-api-go-client"},
	"gtasks-style":         {Kind: "google", Ref: "tasks/v1", Label: "google-api-go-client"},
	"drive-style":          {Kind: "google", Ref: "drive/v3", Label: "google-api-go-client"},
	"youtube-style":        {Kind: "google", Ref: "youtube/v3", Label: "google-api-go-client"},
	// Node SDK embedded tables.
	"github-style":  {Kind: "node", Ref: "@octokit/plugin-rest-endpoint-methods", Label: "octokit"},
	"hubspot-style": {Kind: "node", Ref: "@hubspot/api-client", Label: "hubspot-node"},
	"jira-style":    {Kind: "node", Ref: "jira.js", Label: "jira-js"},
	"llm-style":     {Kind: "node", Ref: "openai", Label: "openai-node"},
	"plaid-style":   {Kind: "node", Ref: "plaid", Label: "plaid-node"},
	"resend-style":  {Kind: "node", Ref: "resend", Label: "resend-node"},
	"slack-style":   {Kind: "node", Ref: "@slack/web-api", Label: "slack-node"},
	"square-style":  {Kind: "node", Ref: "square", Label: "square-node"},
	"stripe-style":  {Kind: "node", Ref: "stripe", Label: "stripe-node"},
	// Vendored official specs (conformance/surfaces/, network-free at
	// generation time — fetchsurfaces produced them from the URLs recorded
	// in each artifact).
	"apple-appstoreconnect-style": {Kind: "spec", Ref: "apple-appstoreconnect-style.json"},
	"auth0-style":                 {Kind: "spec", Ref: "auth0-style.json"},
	"azure-devops-style":          {Kind: "spec", Ref: "azure-devops-style.json"},
	"azure-storage-style":         {Kind: "spec", Ref: "azure-storage-style.json"},
	"bluesky-style":               {Kind: "spec", Ref: "bluesky-style.json"},
	"cloudflare-style":            {Kind: "spec", Ref: "cloudflare-style.json"},
	"discord-style":               {Kind: "spec", Ref: "discord-style.json"},
	"dropbox-style":               {Kind: "spec", Ref: "dropbox-style.json"},
	"entra-id-style":              {Kind: "spec", Ref: "entra-id-style.json"},
	"fattureincloud-style":        {Kind: "spec", Ref: "fattureincloud-style.json"},
	"microsoft-graph-style":       {Kind: "spec", Ref: "microsoft-graph-style.json"},
	"onfido-style":                {Kind: "spec", Ref: "onfido-style.json"},
	"opensea-style":               {Kind: "spec", Ref: "opensea-style.json"},
	"paypal-style":                {Kind: "spec", Ref: "paypal-style.json"},
	"persona-style":               {Kind: "spec", Ref: "persona-style.json"},
	"printful-style":              {Kind: "spec", Ref: "printful-style.json"},
	"printify-style":              {Kind: "spec", Ref: "printify-style.json"},
	"revenuecat-style":            {Kind: "spec", Ref: "revenuecat-style.json"},
	"sendgrid-style":              {Kind: "spec", Ref: "sendgrid-style.json"},
	"adyen-style":                 {Kind: "spec", Ref: "adyen-style.json"},
	"twitter-style":               {Kind: "spec", Ref: "twitter-style.json"},
	"x-articles-style":            {Kind: "spec", Ref: "x-articles-style.json"},
	"xero-style":                  {Kind: "spec", Ref: "xero-style.json"},
	"twilio-style":                {Kind: "node", Ref: "twilio", Label: "twilio-node"},
}

// surfaceOut is the derived coverage for one adapter.
type surfaceOut struct {
	Source   string // "sdk @ version"
	Provider int
	Covered  int
	Pct      int
	Missing  []sdkmap.Route
}

// deriveSurfaces extracts each mapped provider table and diffs it against
// the adapter's manifest routes. Any failure is fatal: a missing module
// cache or an extractor that lost the source layout must never render as
// an empty (100%-covered) table.
func deriveSurfaces(adapters []*adapter.Adapter, sdkVer map[string]string, confDir string) (map[string]*surfaceOut, error) {
	out := map[string]*surfaceOut{}
	tables := map[string][]sdkmap.Route{}
	for _, a := range adapters {
		spec, ok := surfaceSource[a.ID]
		if !ok {
			continue
		}
		var source string
		var table []sdkmap.Route
		if spec.Kind == "spec" {
			// Vendored artifacts carry their own provenance; one file per
			// adapter, so nothing is shared to cache.
			f, err := sdkmap.LoadSurface(filepath.Join(confDir, "surfaces", spec.Ref))
			if err != nil {
				return nil, fmt.Errorf("surface for %s: %w — run `just surfaces-fetch`", a.ID, err)
			}
			table, source = f.Routes, f.Source
		} else {
			ver, ok := sdkVer[spec.Label]
			if !ok {
				return nil, fmt.Errorf("surface source for %s: sdk label %q has no resolved version — is its suite installed?", a.ID, spec.Label)
			}
			source = "sdk " + spec.Label + " @ " + ver
			cacheKey := spec.Kind + " " + spec.Ref + " " + spec.Also
			cached, done := tables[cacheKey]
			if !done {
				var err error
				cached, err = sdkmap.Extract(spec.Kind, spec.Ref, confDir)
				if err != nil {
					return nil, fmt.Errorf("surface for %s: %w", a.ID, err)
				}
				if spec.Also != "" {
					extra, err := sdkmap.Extract(spec.Kind, spec.Also, confDir)
					if err != nil {
						return nil, fmt.Errorf("surface for %s: %w", a.ID, err)
					}
					cached = append(append([]sdkmap.Route{}, cached...), extra...)
				}
				tables[cacheKey] = cached
			}
			table = cached
		}
		var adapterRoutes []sdkmap.AdapterRoute
		for _, ep := range a.Endpoints {
			adapterRoutes = append(adapterRoutes, sdkmap.AdapterRoute{Method: ep.Method, Path: ep.Route})
		}
		diff := sdkmap.Diff(table, adapterRoutes)
		pct := 0
		if diff.Provider > 0 {
			pct = diff.Covered * 100 / diff.Provider
		}
		out[a.ID] = &surfaceOut{
			Source:   source,
			Provider: diff.Provider,
			Covered:  diff.Covered,
			Pct:      pct,
			Missing:  diff.Missing,
		}
	}
	// An artifact on disk that no registry entry consumes would silently
	// rot — fail instead, mirroring the sidecar and suite guards.
	referenced := map[string]bool{}
	for _, spec := range surfaceSource {
		if spec.Kind == "spec" {
			referenced[spec.Ref] = true
		}
	}
	entries, err := os.ReadDir(filepath.Join(confDir, "surfaces"))
	if err != nil {
		return nil, fmt.Errorf("reading surfaces dir: %w — run `just surfaces-fetch`", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") || referenced[e.Name()] {
			continue
		}
		return nil, fmt.Errorf("surfaces/%s has no surfaceSource registry entry — register it or delete the artifact", e.Name())
	}
	return out, nil
}

// scanBehaviors derives the static tag set for every adapter. Fail-loud:
// astscan errors (unparseable script, missing handler) propagate — a
// silent skip would publish an empty behaviors picture that looks like
// "nothing happens here".
func scanBehaviors(adapters []*adapter.Adapter) (map[string][]astscan.EndpointTags, error) {
	out := map[string][]astscan.EndpointTags{}
	for _, a := range adapters {
		tags, err := astscan.Scan(a)
		if err != nil {
			return nil, err
		}
		if len(tags) > 0 {
			out[a.ID] = tags
		}
	}
	return out, nil
}

// ---- rendering ----------------------------------------------------------

type sdkGroup struct {
	label     string
	order     []string
	byAdapter map[string][]string
}

func render(adapters []*adapter.Adapter, checks []check, sdkVer map[string]string, gaps map[string]gapEntry, surfaces map[string]*surfaceOut, derived map[string][]astscan.EndpointTags, root string) (string, error) {
	byAdapterChecks := map[string][]check{}
	vmBehav, err := loadVMBehaviors(root, adapters)
	if err != nil {
		return "", err
	}
	vmNames := make([]string, 0, len(vmBehav))
	for id := range vmBehav {
		vmNames = append(vmNames, id)
	}
	sort.Strings(vmNames)
	groups := map[string]*sdkGroup{}
	var groupOrder []string
	for _, c := range checks {
		if _, knownSDK := sdkVer[c.SDK]; !knownSDK {
			return "", fmt.Errorf("sdk label %q appears in checks but has no resolved version", c.SDK)
		}
		byAdapterChecks[c.Adapter] = append(byAdapterChecks[c.Adapter], c)
		g, ok := groups[c.SDK]
		if !ok {
			g = &sdkGroup{label: c.SDK, byAdapter: map[string][]string{}}
			groups[c.SDK] = g
			groupOrder = append(groupOrder, c.SDK)
		}
		if _, seen := g.byAdapter[c.Adapter]; !seen {
			g.order = append(g.order, c.Adapter)
		}
		g.byAdapter[c.Adapter] = append(g.byAdapter[c.Adapter], c.Name)
	}
	sort.Strings(groupOrder)

	vm := map[string]bool{}
	for _, a := range adapters {
		name := strings.ReplaceAll(a.ID, "-", "_") + "_test.go"
		if _, err := os.Stat(filepath.Join(root, "adapters", name)); err == nil {
			vm[a.ID] = true
		}
	}

	var b bytes.Buffer
	b.WriteString(`<!-- GENERATED by conformance/cmd/genmatrix — do not edit by hand.
     Regenerate with ` + "`just conformance-matrix`" + `; CI regenerates and fails on drift.
     SDK versions are read live from conformance/go.mod and
     conformance/node/package.json, so they cannot go stale here. -->

# SDK conformance matrix

Every reference adapter, and how it is verified. The conformance suites drive
**real official SDKs** — their serialization, their request signing, their
client-side validation — against booted stunt adapters, not hand-rolled HTTP
calls.

Verification tiers:

- **SDK** — an official provider SDK is driven against the adapter in CI (Go
  suites run against the engine; Node suites boot the real ` + "`stunt`" + ` binary
  and go through the CLI end to end).
- **VM** — handler-level Go tests (` + "`adapters/<name>_test.go`" + `) execute the
  adapter's Starlark handlers directly.
- **boot** — clears ` + "`stunt adapter lint`" + `, the all-scripts-parse guard and
  the all-adapters-boot guard on every CI run; no SDK suite drives it yet.
- Every adapter additionally documents its behavior in depth in its README.

`)
	tierOf := func(id string) string {
		_, isSDK := byAdapterChecks[id]
		switch {
		case isSDK && vm[id]:
			return "SDK + VM"
		case isSDK:
			return "SDK"
		case vm[id]:
			return "VM"
		}
		return "boot"
	}
	nBoth, nSDK, nVM := 0, 0, 0
	for _, a := range adapters {
		switch tierOf(a.ID) {
		case "SDK + VM":
			nBoth++
		case "SDK":
			nSDK++
		case "VM":
			nVM++
		}
	}
	fmt.Fprintf(&b, "**%d adapters** — %d SDK+VM, %d SDK-only, %d VM-only, %d boot-tier.\n\n",
		len(adapters), nBoth, nSDK, nVM, len(adapters)-nBoth-nSDK-nVM)

	if len(surfaces) > 0 {
		fmt.Fprintf(&b, "**%d adapters carry derived provider-surface coverage**: their real-API route totals come from the route tables embedded in the pinned official SDKs (Google Discovery docs inside `google-api-go-client`; generated tables inside the Node clients) or from official specs vendored under `conformance/surfaces/` (refreshed by `just surfaces-fetch`) — mechanical and network-free at generation time. For those rows the derived not-implemented list supplements the curated Missing column; adapters without one have no trustworthy machine-readable surface and stay fully curated.\n\n", len(surfaces))
	}
	fmt.Fprintf(&b, "Behavior columns come in two kinds: **verified** (an official SDK was driven against the adapter and the check passed — the Behaviors counts in the matrix above) and **derived** (static analysis of the handler scripts — every adapter's *Derived behavior tags* block below says what the code does, not that a client confirmed it).\n\n")

	b.WriteString("| Adapter | API | Routes | Verification | Official SDK(s) | Behaviors | Missing | Deviations |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, a := range adapters {
		api := "—"
		if a.API != nil {
			api = fmt.Sprintf("%s `%s`", a.API.Name, a.API.Version)
		}
		routes := fmt.Sprintf("%d", len(a.Endpoints))
		if n := len(a.Websockets); n > 0 {
			routes += fmt.Sprintf(" (+%d ws)", n)
		}
		if a.Graphql != nil {
			routes += " +GQL"
		}
		sdkCell := "—"
		if cs := byAdapterChecks[a.ID]; len(cs) > 0 {
			seen := map[string]bool{}
			var parts []string
			for _, c := range cs {
				if !seen[c.SDK] {
					seen[c.SDK] = true
					parts = append(parts, fmt.Sprintf("%s @ %s", c.SDK, sdkVer[c.SDK]))
				}
			}
			sdkCell = strings.Join(parts, "<br>")
		}
		checksCell := "—"
		if n := len(byAdapterChecks[a.ID]); n > 0 {
			checksCell = fmt.Sprintf("%d", n)
		}
		missingCell := "—"
		if n := len(gaps[a.ID].Missing); n > 0 {
			missingCell = fmt.Sprintf("[%d](#%s)", n, a.ID)
		}
		devCell := "—"
		if n := len(gaps[a.ID].Deviations); n > 0 {
			devCell = fmt.Sprintf("[%d](#%s)", n, a.ID)
		}
		fmt.Fprintf(&b, "| [%s](adapters/%s/) | %s | %s | %s | %s | %s | %s | %s |\n",
			a.ID, a.ID, api, routes, tierOf(a.ID), sdkCell, checksCell, missingCell, devCell)
	}

	b.WriteString(`
## Verified behaviors

What each suite actually asserts, as written in the test sources
(` + "`Record(...)`" + ` literals in ` + "`conformance/*_test.go`" + `; ` + "`// =====`" + `
sections in ` + "`conformance/node/tests/*.test.ts`" + `).

`)
	for _, label := range groupOrder {
		g := groups[label]
		fmt.Fprintf(&b, "### %s @ %s\n\n", label, sdkVer[label])
		for _, adapterID := range g.order {
			fmt.Fprintf(&b, "**%s**\n\n", adapterID)
			for _, n := range g.byAdapter[adapterID] {
				fmt.Fprintf(&b, "- %s\n", n)
			}
			b.WriteString("\n")
		}
	}

	if len(vmNames) > 0 {
		b.WriteString(`
### vm (handler-level Go suites)

What the engine-level suites in ` + "`adapters/<name>_style_test.go`" + ` assert — the
adapter's real handlers execute against the real engine; no SDK is involved.
Named by their ` + "`// =====`" + ` section markers.

`)
		for _, id := range vmNames {
			fmt.Fprintf(&b, "**%s**\n\n", id)
			for _, n := range vmBehav[id] {
				fmt.Fprintf(&b, "- %s\n", n)
			}
			b.WriteString("\n")
		}
	}

	fmt.Fprintf(&b, "## Adapter surface detail\n\n")
	fmt.Fprintf(&b, "Per adapter: the **covered surface** — the exact routes served, read\n")
	fmt.Fprintf(&b, "straight from `adapter.yaml` — and the curated gaps from\n")
	fmt.Fprintf(&b, "`conformance/matrix.yaml`: **missing** (a real-API capability that is\n")
	fmt.Fprintf(&b, "absent) vs **deviations** (covered, but documented to differ). Deep\n")
	fmt.Fprintf(&b, "behavior notes live in each adapter's README.\n\n")
	for _, a := range adapters {
		g := gaps[a.ID]
		fmt.Fprintf(&b, "### %s\n\n", a.ID)
		covered := fmt.Sprintf("%d routes", len(a.Endpoints))
		if n := len(a.Websockets); n > 0 {
			covered += fmt.Sprintf(" (+%d ws)", n)
		}
		if a.Grpc != nil {
			covered += fmt.Sprintf(" · gRPC `%s`", a.Grpc.Service)
		}
		if a.Graphql != nil {
			covered += " · GraphQL schema"
		}
		fmt.Fprintf(&b, "**Covered** — %s\n\n", covered)
		if len(a.Endpoints) > 0 || len(a.Websockets) > 0 {
			fmt.Fprintf(&b, "<details><summary>Routes</summary>\n\n")
			fmt.Fprintf(&b, "| Method | Route |\n")
			fmt.Fprintf(&b, "|---|---|\n")
			for _, ep := range a.Endpoints {
				fmt.Fprintf(&b, "| %s | `%s` |\n", escapeCell(ep.Method), escapeCell(ep.Route))
			}
			for _, ws := range a.Websockets {
				fmt.Fprintf(&b, "| WS | `%s` |\n", escapeCell(ws.Route))
			}
			fmt.Fprintf(&b, "\n</details>\n\n")
		}
		if s, ok := surfaces[a.ID]; ok {
			fmt.Fprintf(&b, "**Provider surface** — derived from %s: %d real routes · %d covered · %d%% · %d not implemented\n\n",
				s.Source, s.Provider, s.Covered, s.Pct, len(s.Missing))
			if len(s.Missing) > 0 {
				fmt.Fprintf(&b, "<details><summary>not implemented (%d)</summary>\n\n", len(s.Missing))
				shown := len(s.Missing)
				if shown > 50 {
					shown = 50
				}
				for i, m := range s.Missing {
					if i >= shown {
						fmt.Fprintf(&b, "… and %d more\n\n", len(s.Missing)-shown)
						break
					}
					fmt.Fprintf(&b, "- `%s` `%s`\n", m.Method, m.Path)
				}
				fmt.Fprintf(&b, "\n</details>\n\n")
			}
		}
		if len(g.Missing) > 0 {
			fmt.Fprintf(&b, "**Missing** (%d)\n\n", len(g.Missing))
			for _, m := range g.Missing {
				fmt.Fprintf(&b, "- %s\n", m)
			}
			b.WriteString("\n")
		}
		if len(g.Deviations) > 0 {
			fmt.Fprintf(&b, "**Deviations** (%d)\n\n", len(g.Deviations))
			for _, d := range g.Deviations {
				fmt.Fprintf(&b, "- %s\n", d)
			}
			b.WriteString("\n")
		}
		if dtags, ok := derived[a.ID]; ok && len(dtags) > 0 {
			fmt.Fprintf(&b, "<details><summary>Derived behavior tags (static — from scripts/*.star, not SDK-verified)</summary>\n\n")
			for _, et := range dtags {
				tags := astscan.TagsString(et.Tags)
				if tags == "" {
					tags = "—"
				}
				fmt.Fprintf(&b, "- `%s` `%s` — %s\n", et.Method, et.Route, tags)
			}
			fmt.Fprintf(&b, "\n</details>\n\n")
		}
	}

	b.WriteString(`---

Maintenance: add deviations to ` + "`conformance/matrix.yaml`" + `; new ` + "`Record(...)`" + `
calls and ` + "`// =====`" + ` sections appear here automatically on regeneration; SDK
version bumps flow from ` + "`go.mod`" + `/` + "`package.json`" + ` with no edit here. A new
adapter without a sidecar entry fails generation, so it cannot ship
uncurated.
`)
	return b.String(), nil
}

// collapse normalizes a section marker's captured text: continuation
// lines of a wrapped // comment keep their own "// " prefix, which is not
// part of the section name.
// escapeCell keeps a route or method containing a pipe from splitting
// the markdown row.
func escapeCell(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}

func collapse(s string) string {
	var parts []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "//"))
		if line != "" {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, " ")
}

// matrixJSON is the JSON shape stuntapi.com renders (src/data/
// conformance.json). It mirrors the markdown matrix one-to-one; versions
// are resolved from the same sources so they cannot drift.
type matrixJSON struct {
	Generated struct {
		Adapters int `json:"adapters"`
		Checks   int `json:"checks"`
		Tiers    struct {
			SDKAndVM int `json:"sdk_and_vm"`
			SDKOnly  int `json:"sdk_only"`
			VMOnly   int `json:"vm_only"`
			Boot     int `json:"boot"`
		} `json:"tiers"`
	} `json:"summary"`
	Adapters []adapterJSON `json:"adapters"`
}

type adapterJSON struct {
	ID string `json:"id"`
	// Manifest identity for the website catalog sync (stunt-www
	// scripts/sync-catalog.mjs): the catalog must derive these from this
	// file, never re-parse adapter.yaml with a second parser.
	DisplayName    string    `json:"display_name"`
	AdapterVersion string    `json:"adapter_version"`
	RealHosts      []string  `json:"real_hosts"`
	APIName        string    `json:"api_name"`
	APIVersion     string    `json:"api_version"`
	Routes         int       `json:"routes"`
	WSRoutes       int       `json:"ws_routes,omitempty"`
	GraphQL        bool      `json:"graphql,omitempty"`
	GRPCService    string    `json:"grpc_service,omitempty"`
	Verification   string    `json:"verification"`
	SDKs           []sdkJSON `json:"sdks"`
	Behaviors      []string  `json:"behaviors"`
	// VMBehaviors are verified by the engine-level Go suite (markers in
	// adapters/<name>_style_test.go) — real handler execution, no SDK.
	VMBehaviors []string `json:"vm_behaviors,omitempty"`
	Missing     []string `json:"missing"`
	Deviations  []string `json:"deviations"`
	// Covered is the adapter's exposed API surface, straight from its
	// manifest — the programmatic half of "what stunt provides".
	Covered []routeJSON `json:"covered"`
	// Surface is the derived provider-surface coverage (real-API route
	// table diffed against the manifest), present only where the pinned
	// SDK embeds a trustworthy table.
	Surface *surfaceJSON `json:"surface,omitempty"`
	// DerivedBehaviorTags are STATIC findings from the handler scripts —
	// what each endpoint's code does. Distinct from Behaviors, which are
	// SDK-verified outcomes; never merge the two.
	DerivedBehaviorTags []derivedJSON `json:"derived_behaviors,omitempty"`
}

type derivedJSON struct {
	Method string   `json:"method"`
	Route  string   `json:"route"`
	Tags   []string `json:"tags"`
}

type surfaceJSON struct {
	Source         string      `json:"source"` // "sdk <label> @ <version>"
	ProviderRoutes int         `json:"provider_routes"`
	CoveredRoutes  int         `json:"covered_routes"`
	CoveragePct    int         `json:"coverage_pct"`
	MissingRoutes  []routeJSON `json:"missing_routes"`
	Derived        bool        `json:"derived"`
}

type routeJSON struct {
	Method string `json:"method"`
	Route  string `json:"route"`
}

type sdkJSON struct {
	Label   string `json:"label"`
	Version string `json:"version"`
}

func renderJSON(adapters []*adapter.Adapter, checks []check, sdkVer map[string]string, gaps map[string]gapEntry, surfaces map[string]*surfaceOut, derived map[string][]astscan.EndpointTags, root string) ([]byte, error) {
	byAdapterChecks := map[string][]check{}
	for _, c := range checks {
		byAdapterChecks[c.Adapter] = append(byAdapterChecks[c.Adapter], c)
	}
	vm := map[string]bool{}
	for _, a := range adapters {
		name := strings.ReplaceAll(a.ID, "-", "_") + "_test.go"
		if _, err := os.Stat(filepath.Join(root, "adapters", name)); err == nil {
			vm[a.ID] = true
		}
	}
	// VM suite markers name the behaviors each engine-level test verifies.
	vmBehav, err := loadVMBehaviors(root, adapters)
	if err != nil {
		return nil, err
	}
	var m matrixJSON
	m.Generated.Adapters = len(adapters)
	m.Generated.Checks = len(checks)
	for _, a := range adapters {
		tier := "boot"
		isSDK := len(byAdapterChecks[a.ID]) > 0
		switch {
		case isSDK && vm[a.ID]:
			tier = "SDK + VM"
			m.Generated.Tiers.SDKAndVM++
		case isSDK:
			tier = "SDK"
			m.Generated.Tiers.SDKOnly++
		case vm[a.ID]:
			tier = "VM"
			m.Generated.Tiers.VMOnly++
		default:
			m.Generated.Tiers.Boot++
		}
		row := adapterJSON{
			ID:             a.ID,
			DisplayName:    a.Name,
			AdapterVersion: a.Version,
			RealHosts:      a.RealHosts,
			Routes:         len(a.Endpoints),
			WSRoutes:       len(a.Websockets),
			Verification:   tier,
			Behaviors:      []string{},
			VMBehaviors:    vmBehav[a.ID],
			Missing:        gaps[a.ID].Missing,
			Deviations:     gaps[a.ID].Deviations,
		}
		if row.RealHosts == nil {
			row.RealHosts = []string{}
		}
		if row.Missing == nil {
			row.Missing = []string{}
		}
		if row.Deviations == nil {
			row.Deviations = []string{}
		}
		if a.API != nil {
			row.APIName = a.API.Name
			row.APIVersion = a.API.Version
		}
		if a.Graphql != nil {
			row.GraphQL = true
		}
		if a.Grpc != nil {
			row.GRPCService = a.Grpc.Service
		}
		row.Covered = []routeJSON{}
		for _, ep := range a.Endpoints {
			row.Covered = append(row.Covered, routeJSON{Method: ep.Method, Route: ep.Route})
		}
		for _, ws := range a.Websockets {
			row.Covered = append(row.Covered, routeJSON{Method: "WS", Route: ws.Route})
		}
		if dtags, ok := derived[a.ID]; ok && len(dtags) > 0 {
			row.DerivedBehaviorTags = []derivedJSON{}
			for _, et := range dtags {
				tags := make([]string, len(et.Tags))
				for i, t := range et.Tags {
					tags[i] = string(t)
				}
				row.DerivedBehaviorTags = append(row.DerivedBehaviorTags, derivedJSON{Method: et.Method, Route: et.Route, Tags: tags})
			}
		}
		if s, ok := surfaces[a.ID]; ok {
			sj := &surfaceJSON{
				Source:         s.Source,
				ProviderRoutes: s.Provider,
				CoveredRoutes:  s.Covered,
				CoveragePct:    s.Pct,
				Derived:        true,
			}
			for _, m := range s.Missing {
				sj.MissingRoutes = append(sj.MissingRoutes, routeJSON{Method: m.Method, Route: m.Path})
			}
			if sj.MissingRoutes == nil {
				sj.MissingRoutes = []routeJSON{}
			}
			row.Surface = sj
		}
		seen := map[string]bool{}
		for _, c := range byAdapterChecks[a.ID] {
			row.Behaviors = append(row.Behaviors, c.Name)
			if !seen[c.SDK] {
				seen[c.SDK] = true
				row.SDKs = append(row.SDKs, sdkJSON{Label: c.SDK, Version: sdkVer[c.SDK]})
			}
		}
		if row.SDKs == nil {
			row.SDKs = []sdkJSON{}
		}
		m.Adapters = append(m.Adapters, row)
	}
	return json.MarshalIndent(m, "", "  ")
}
