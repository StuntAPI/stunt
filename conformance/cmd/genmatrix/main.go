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
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"stuntapi.com/stunt/internal/adapter"
)

func main() {
	root := flag.String("root", "..", "repo root (default: run from conformance/)")
	flag.Parse()
	if err := run(*root); err != nil {
		fmt.Fprintln(os.Stderr, "genmatrix:", err)
		os.Exit(1)
	}
}

func run(root string) error {
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
	deviations, err := loadSidecar(filepath.Join(root, "conformance", "matrix.yaml"), ids)
	if err != nil {
		return err
	}

	doc, err := render(adapters, checks, sdkVer, deviations, root)
	if err != nil {
		return err
	}
	out := filepath.Join(root, "CONFORMANCE.md")
	if err := os.WriteFile(out, []byte(doc), 0o644); err != nil {
		return err
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

var recordRe = regexp.MustCompile(`Record\(t,\s*"([^"]*)"\s*,\s*"([^"]*)"\s*,\s*"([^"]*)"\)`)

// parseGoRecords extracts Record literals from the Go suites. Non-literal
// calls (fmt.Sprintf...) are invisible to static extraction, so a count
// mismatch is a hard error rather than a silently missing row entry.
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
		total := strings.Count(string(data), "Record(t,")
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
	"github.test.ts": "octokit",
	"stripe.test.ts": "stripe-node",
	"twilio.test.ts": "twilio-node",
}

var (
	nodeBootRe = regexp.MustCompile(`bootAdapter\("([^"]+)"\)`)
	nodeSectRe = regexp.MustCompile(`(?s)//\s*=====\s*(.*?)\s*=====`)
	nodeTestRe = regexp.MustCompile(`test\(\s*"([^"]+)"`)
)

func parseNodeTests(dir string) ([]check, error) {
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
			// No section markers: fall back to the enclosing test name.
			if m := nodeTestRe.FindStringSubmatch(string(data)); m != nil {
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
}

type sidecar struct {
	Adapters map[string]sidecarAdapter `yaml:"adapters"`
}

// loadSidecar validates that the sidecar covers exactly the given adapter
// ids: a stale entry or a missing adapter is an error, never a silent hole.
func loadSidecar(path string, ids []string) (map[string][]string, error) {
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
	out := map[string][]string{}
	for _, id := range ids {
		entry, ok := sc.Adapters[id]
		if !ok {
			return nil, fmt.Errorf("%s: adapter %q has no entry — every adapter must carry a deviations list (empty is fine)", path, id)
		}
		out[id] = entry.Deviations
	}
	return out, nil
}

// ---- version resolution -------------------------------------------------

// goModuleBases maps a major-stripped SDK label to its module repo. Labels
// carrying "/vN" (go-github/v89) resolve to repo+vN — the label itself
// pins the major, so bumping an SDK needs no edit here.
var goModuleBases = map[string]string{
	"aws-sdk-go-v2": "github.com/aws/aws-sdk-go-v2",
	"go-github":     "github.com/google/go-github",
	"go-shopify":    "github.com/bold-commerce/go-shopify",
	"stripe-go":     "github.com/stripe/stripe-go",
	"twilio-go":     "github.com/twilio/twilio-go",
	"x/oauth2":      "golang.org/x/oauth2",
	// The google suite labels the subpackage, not the umbrella module.
	"google-api-go-client/idtoken": "google.golang.org/api",
}

var nodePackages = map[string]string{
	"octokit":     "octokit",
	"stripe-node": "stripe",
	"twilio-node": "twilio",
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

// ---- rendering ----------------------------------------------------------

type sdkGroup struct {
	label     string
	order     []string
	byAdapter map[string][]string
}

func render(adapters []*adapter.Adapter, checks []check, sdkVer map[string]string, deviations map[string][]string, root string) (string, error) {
	byAdapterChecks := map[string][]check{}
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

	b.WriteString("| Adapter | API | Routes | Verification | Official SDK(s) | Behaviors | Deviations |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
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
		devCell := "—"
		if n := len(deviations[a.ID]); n > 0 {
			devCell = fmt.Sprintf("[%d](#documented-deviations)", n)
		}
		fmt.Fprintf(&b, "| [%s](adapters/%s/) | %s | %s | %s | %s | %s | %s |\n",
			a.ID, a.ID, api, routes, tierOf(a.ID), sdkCell, checksCell, devCell)
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

	b.WriteString(`## Documented deviations

Curated in ` + "`conformance/matrix.yaml`" + ` (the generator fails if an adapter is
missing from it). These are the intentional divergences from the real API
each adapter documents; everything else mirrors the provider wherever it is
observable over the wire.

`)
	for _, a := range adapters {
		devs := deviations[a.ID]
		if len(devs) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### %s\n\n", a.ID)
		for _, d := range devs {
			fmt.Fprintf(&b, "- %s\n", d)
		}
		b.WriteString("\n")
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
