// Command fetchsurfaces vendors provider route tables as pre-normalized
// artifacts under conformance/surfaces/. It is the ONLY network-touching
// piece of the conformance matrix: run it manually via
// `just surfaces-fetch [adapter-id...]` when onboarding an adapter or
// refreshing its spec. CI never fetches — genmatrix reads the committed
// artifacts network-free, and the freshness gate keeps the derived numbers
// in sync with whatever was vendored.
//
// Sources are the providers' own published specs (OpenAPI 2/3, Discovery,
// zip-packaged). Each artifact records provenance (URL, version, license,
// fetch date). Route-table facts are interface data; sources under
// restrictive terms (proprietary, CC BY-ND) are deliberately not vendored.
package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"stuntapi.com/stunt/conformance/sdkmap"
)

type source struct {
	URL         string
	Format      string // "oas" (default) | "zip" | "stone" | "lexicon"
	StripPrefix string // provider path prefix the adapter does not serve
	AddPrefix   string // version prefix the spec keeps in its server URL
	// Prefixes narrows a "lexicon" source to NSID subtrees ("app/bsky/"
	// matches lexicons/app/bsky/**); URL is the git-trees API endpoint that
	// enumerates them.
	Prefixes []string
}

type provider struct {
	Adapters []string
	Name     string // display identity, incl. which files a union covers
	Sources  []source
	License  string
}

// providers is the vendoring registry. Mirrors genmatrix's surfaceSpec
// registry: an adapter listed here and missing its artifact makes
// genmatrix fail loudly.
var providers = []provider{
	{
		Adapters: []string{"microsoft-graph-style", "entra-id-style"},
		Name:     "msgraph-metadata v1.0",
		// The spec keeps /v1.0 in its server URL; the adapter serves it in
		// the path, like the real host does.
		Sources: []source{{URL: "https://raw.githubusercontent.com/microsoftgraph/msgraph-metadata/master/openapi/v1.0/openapi.yaml", AddPrefix: "/v1.0"}},
		License: "MIT",
	},
	{
		Adapters: []string{"discord-style"},
		Name:     "discord-api-spec",
		Sources:  []source{{URL: "https://raw.githubusercontent.com/discord/discord-api-spec/main/specs/openapi.json"}},
		License:  "MIT",
	},
	{
		Adapters: []string{"twitter-style", "x-articles-style"},
		Name:     "X API v2 openapi (published at the API origin)",
		Sources:  []source{{URL: "https://api.x.com/2/openapi.json"}},
		License:  "X Developer Agreement (route-table facts)",
	},
	{
		Adapters: []string{"cloudflare-style"},
		Name:     "cloudflare api-schemas (paths /client/v4 prefix stripped)",
		// The adapter serves zone-rooted paths; the deviation is curated in
		// matrix.yaml.
		Sources: []source{{URL: "https://raw.githubusercontent.com/cloudflare/api-schemas/main/openapi.json", StripPrefix: "/client/v4"}},
		License: "BSD-3-Clause",
	},
	{
		Adapters: []string{"auth0-style"},
		Name:     "Auth0 Management API OAS",
		Sources:  []source{{URL: "https://auth0.com/docs/oas/management/v2/management-api-oas.json", AddPrefix: "/api/v2"}},
		License:  "none stated",
	},
	{
		Adapters: []string{"apple-appstoreconnect-style"},
		Name:     "App Store Connect OpenAPI Specification",
		Sources:  []source{{URL: "https://developer.apple.com/sample-code/app-store-connect/app-store-connect-openapi-specification.zip", Format: "zip"}},
		License:  "none stated",
	},
	{
		Adapters: []string{"adyen-style"},
		Name:     "adyen-openapi CheckoutService-v68 + PaymentService-v68",
		// v68 matches the adapter's pinned API version; the version lives
		// in the server URL, not the paths.
		Sources: []source{
			{URL: "https://raw.githubusercontent.com/Adyen/adyen-openapi/main/yaml/CheckoutService-v68.yaml", AddPrefix: "/v68"},
			{URL: "https://raw.githubusercontent.com/Adyen/adyen-openapi/main/yaml/PaymentService-v68.yaml", AddPrefix: "/v68"},
		},
		License: "MIT",
	},
	{
		Adapters: []string{"xero-style"},
		Name:     "Xero-OpenAPI xero_accounting",
		Sources:  []source{{URL: "https://raw.githubusercontent.com/XeroAPI/Xero-OpenAPI/master/xero_accounting.yaml", AddPrefix: "/api.xro/2.0"}},
		License:  "MIT",
	},
	{
		Adapters: []string{"fattureincloud-style"},
		Name:     "openapi-fattureincloud",
		Sources:  []source{{URL: "https://raw.githubusercontent.com/fattureincloud/openapi-fattureincloud/master/openapi.yaml"}},
		License:  "MIT",
	},
	{
		Adapters: []string{"revenuecat-style"},
		Name:     "RevenueCat API v1 reference",
		Sources:  []source{{URL: "https://www.revenuecat.com/docs/redocusaurus/openapi-v1.yaml", AddPrefix: "/v1"}},
		License:  "none stated",
	},
	{
		Adapters: []string{"sendgrid-style"},
		Name:     "sendgrid-oai tsg_mail_v3 + tsg_email_activity_v3 + tsg_webhooks_v3",
		Sources: []source{
			{URL: "https://raw.githubusercontent.com/twilio/sendgrid-oai/main/spec/yaml/tsg_mail_v3.yaml"},
			{URL: "https://raw.githubusercontent.com/twilio/sendgrid-oai/main/spec/yaml/tsg_email_activity_v3.yaml"},
			{URL: "https://raw.githubusercontent.com/twilio/sendgrid-oai/main/spec/yaml/tsg_webhooks_v3.yaml"},
		},
		License: "MIT",
	},
	{
		Adapters: []string{"paypal-style"},
		Name:     "paypal-rest-api-specifications checkout_orders_v2 + payments_payment_v2",
		Sources: []source{
			{URL: "https://raw.githubusercontent.com/paypal/paypal-rest-api-specifications/main/openapi/checkout_orders_v2.json"},
			{URL: "https://raw.githubusercontent.com/paypal/paypal-rest-api-specifications/main/openapi/payments_payment_v2.json"},
		},
		License: "Apache-2.0",
	},
	{
		Adapters: []string{"onfido-style"},
		Name:     "onfido-openapi-spec",
		Sources:  []source{{URL: "https://raw.githubusercontent.com/onfido/onfido-openapi-spec/master/generated/artifacts/openapi/openapi.json", AddPrefix: "/v3.6"}},
		License:  "MIT (declared in spec)",
	},
	{
		Adapters: []string{"persona-style"},
		Name:     "Persona API reference",
		Sources:  []source{{URL: "https://docs.withpersona.com/openapi/api-reference.json", AddPrefix: "/api/inquiry/v1"}},
		License:  "none stated",
	},
	{
		Adapters: []string{"printful-style"},
		Name:     "Printful OpenAPI",
		Sources:  []source{{URL: "https://developers.printful.com/docs/openapi.json"}},
		License:  "none stated",
	},
	{
		Adapters: []string{"printify-style"},
		Name:     "Printify OpenAPI",
		Sources:  []source{{URL: "https://developers.printify.com/openapi.json"}},
		License:  "ISC (declared in spec)",
	},
	{
		Adapters: []string{"opensea-style"},
		Name:     "OpenSea ReadMe registry spec (semi-official)",
		Sources:  []source{{URL: "https://dash.readme.com/api/v1/api-registry/4c25mt2f2f8m"}},
		License:  "none stated",
	},
	{
		Adapters: []string{"azure-storage-style"},
		Name:     "azure-rest-api-specs BlobStorage 2026-06-06",
		Sources:  []source{{URL: "https://raw.githubusercontent.com/Azure/azure-rest-api-specs/main/specification/storage/data-plane/Microsoft.BlobStorage/stable/2026-06-06/blob.json"}},
		License:  "MIT",
	},
	{
		Adapters: []string{"dropbox-style"},
		Name:     "dropbox-api-spec files.stone + users.stone (every route POST — API v2 is RPC-style, Stone declares no verb)",
		// The adapter spans the files and users namespaces; path is
		// /2/<namespace>/<route name> with Stone's :N version suffix
		// rendered as the _vN Dropbox serves.
		Sources: []source{
			{URL: "https://raw.githubusercontent.com/dropbox/dropbox-api-spec/main/files.stone", Format: "stone"},
			{URL: "https://raw.githubusercontent.com/dropbox/dropbox-api-spec/main/users.stone", Format: "stone"},
		},
		License: "MIT",
	},
	{
		Adapters: []string{"bluesky-style"},
		Name:     "atproto lexicons app.bsky.* + com.atproto.* (XRPC: query=GET, procedure=POST; records/objects/subscriptions skipped)",
		// The adapter serves /xrpc/<nsid> for both namespaces it simulates;
		// the git-trees API enumerates every lexicon under the two subtrees
		// in one request.
		Sources: []source{{
			URL:      "https://api.github.com/repos/bluesky-social/atproto/git/trees/main?recursive=1",
			Format:   "lexicon",
			Prefixes: []string{"app/bsky/", "com/atproto/"},
		}},
		License: "mixed MIT/Apache-2.0",
	},
	{
		Adapters: []string{"azure-devops-style"},
		// The adapter spans five areas; the spec repo publishes one file
		// per area (core is version-foldered differently from the rest).
		Name: "vsts-rest-api-specs core 7.2 + pipelines/git/wit/hooks/work 7.1",
		Sources: []source{
			{URL: "https://raw.githubusercontent.com/MicrosoftDocs/vsts-rest-api-specs/master/specification/core/7.2/core.json"},
			{URL: "https://raw.githubusercontent.com/MicrosoftDocs/vsts-rest-api-specs/master/specification/pipelines/azure-devops-server-7.1/pipelines-onprem.json"},
			{URL: "https://raw.githubusercontent.com/MicrosoftDocs/vsts-rest-api-specs/master/specification/git/azure-devops-server-7.1/git-onprem.json"},
			{URL: "https://raw.githubusercontent.com/MicrosoftDocs/vsts-rest-api-specs/master/specification/wit/azure-devops-server-7.1/workItemTracking-onprem.json"},
			{URL: "https://raw.githubusercontent.com/MicrosoftDocs/vsts-rest-api-specs/master/specification/hooks/azure-devops-server-7.1/serviceHooks-onprem.json"},
			{URL: "https://raw.githubusercontent.com/MicrosoftDocs/vsts-rest-api-specs/master/specification/work/azure-devops-server-7.1/work-onprem.json"},
		},
		License: "MIT",
	},
}

func main() {
	want := map[string]bool{}
	for _, id := range os.Args[1:] {
		want[id] = true
	}
	outDir := filepath.Join("conformance", "surfaces")
	if _, err := os.Stat(filepath.Join("conformance", "go.mod")); err != nil {
		// Run from the conformance/ dir instead of the repo root.
		outDir = "surfaces"
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fatal(err)
	}

	failed := 0
	for _, p := range providers {
		if len(want) > 0 && !matchesAny(p.Adapters, want) {
			continue
		}
		if err := fetchProvider(p, outDir); err != nil {
			fmt.Fprintf(os.Stderr, "fetchsurfaces: %v\n", err)
			failed++
		}
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "fetchsurfaces: %d provider(s) failed\n", failed)
		os.Exit(1)
	}
	fmt.Println("fetchsurfaces: all providers vendored")
}

func matchesAny(ids []string, want map[string]bool) bool {
	for _, id := range ids {
		if want[id] || want[strings.TrimSuffix(id, "-style")] {
			return true
		}
	}
	return false
}

func fetchProvider(p provider, outDir string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	seen := map[string]bool{}
	var routes []sdkmap.Route
	var versions, urls []string
	for _, s := range p.Sources {
		data, err := fetch(client, s.URL)
		if err != nil {
			return fmt.Errorf("%s: %w", p.Name, err)
		}
		var parsed []sdkmap.Route
		var version string
		switch s.Format {
		case "stone":
			parsed, err = sdkmap.ParseStone(data)
		case "lexicon":
			parsed, err = fetchLexicons(client, data, s.Prefixes)
		default:
			if s.Format == "zip" {
				data, err = unzipJSON(data)
				if err != nil {
					return fmt.Errorf("%s: %w", p.Name, err)
				}
			}
			// Azure specs ship a UTF-8 BOM.
			data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
			parsed, version, err = sdkmap.ParseOpenAPI(data)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", p.Name, err)
		}
		if version != "" {
			versions = append(versions, version)
		}
		urls = append(urls, s.URL)
		for _, r := range parsed {
			path := r.Path
			if s.StripPrefix != "" {
				path = strings.TrimPrefix(path, s.StripPrefix)
			}
			if s.AddPrefix != "" {
				path = s.AddPrefix + path
			}
			k := r.Method + " " + path
			if !seen[k] {
				seen[k] = true
				routes = append(routes, sdkmap.Route{Method: r.Method, Path: path})
			}
		}
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Path != routes[j].Path {
			return routes[i].Path < routes[j].Path
		}
		return routes[i].Method < routes[j].Method
	})
	// House rule: an extractor that loses contact with the source layout
	// must fail, not vendor an empty (covers-everything) table.
	if len(routes) == 0 {
		return fmt.Errorf("%s: 0 routes — upstream layout changed?", p.Name)
	}

	src := p.Name
	if len(versions) > 0 {
		src += " @ " + strings.Join(versions, ", ")
	}
	artifact := sdkmap.SurfaceFile{
		Source:  "spec " + src,
		URL:     strings.Join(urls, " "),
		Fetched: time.Now().UTC().Format("2006-01-02"),
		License: p.License,
		Routes:  routes,
	}
	for _, id := range p.Adapters {
		data, err := json.MarshalIndent(artifact, "", "  ")
		if err != nil {
			return err
		}
		path := filepath.Join(outDir, id+".json")
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Printf("fetchsurfaces: %s — %d routes → %s\n", id, len(routes), path)
	}
	return nil
}

var userAgent = "stunt-fetchsurfaces/1.0 (https://github.com/StuntAPI/stunt)"

func fetch(client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

// githubTree is the slice of the GitHub git-trees API response fetchLexicons
// needs: the file list plus the truncation flag (recursive listings of very
// large repos come back incomplete — vendoring half a namespace silently is
// worse than failing).
type githubTree struct {
	Truncated bool `json:"truncated"`
	Tree      []struct {
		Path string `json:"path"`
		Type string `json:"type"` // "blob" | "tree"
	} `json:"tree"`
}

// fetchLexicons takes an already-fetched git-trees document (from the
// source URL) and fetches every lexicon under the given NSID subtrees,
// returning the union of their routes. The repo layout (lexicons/<domain
// path>/<name>.json) is atproto's own, so the raw-URL base is pinned here.
func fetchLexicons(client *http.Client, treeJSON []byte, prefixes []string) ([]sdkmap.Route, error) {
	var tree githubTree
	if err := json.Unmarshal(treeJSON, &tree); err != nil {
		return nil, fmt.Errorf("git trees: %w", err)
	}
	if tree.Truncated {
		return nil, fmt.Errorf("git trees: listing truncated — repo too large to enumerate reliably")
	}
	var paths []string
	for _, e := range tree.Tree {
		if e.Type != "blob" || !strings.HasSuffix(e.Path, ".json") {
			continue
		}
		rel := strings.TrimPrefix(e.Path, "lexicons/")
		for _, p := range prefixes {
			if strings.HasPrefix(rel, p) {
				paths = append(paths, e.Path)
				break
			}
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("git trees: 0 lexicons under %v — upstream layout changed?", prefixes)
	}
	sort.Strings(paths)
	var routes []sdkmap.Route
	for _, path := range paths {
		data, err := fetch(client, "https://raw.githubusercontent.com/bluesky-social/atproto/main/"+path)
		if err != nil {
			return nil, err
		}
		parsed, err := sdkmap.ParseLexicon(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		routes = append(routes, parsed...)
	}
	return routes, nil
}

// unzipJSON picks the first .json member (Apple ships the spec as a zip of
// one document plus macOS resource-fork noise).
func unzipJSON(data []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".json") && !strings.Contains(f.Name, "__MACOSX") {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, 200<<20))
		}
	}
	return nil, fmt.Errorf("zip carries no .json member")
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "fetchsurfaces: %v\n", err)
	os.Exit(1)
}
