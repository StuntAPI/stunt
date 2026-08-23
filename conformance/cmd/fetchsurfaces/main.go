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
	Format      string // "oas" (default) | "zip"
	StripPrefix string // provider path prefix the adapter does not serve
	AddPrefix   string // version prefix the spec keeps in its server URL
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
		// v68 matches the adapter's pinned API version.
		Sources: []source{
			{URL: "https://raw.githubusercontent.com/Adyen/adyen-openapi/main/yaml/CheckoutService-v68.yaml"},
			{URL: "https://raw.githubusercontent.com/Adyen/adyen-openapi/main/yaml/PaymentService-v68.yaml"},
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
		Name:     "sendgrid-oai tsg_mail_v3",
		Sources:  []source{{URL: "https://raw.githubusercontent.com/twilio/sendgrid-oai/main/spec/yaml/tsg_mail_v3.yaml"}},
		License:  "MIT",
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
		if s.Format == "zip" {
			data, err = unzipJSON(data)
			if err != nil {
				return fmt.Errorf("%s: %w", p.Name, err)
			}
		}
		// Azure specs ship a UTF-8 BOM.
		data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
		parsed, version, err := sdkmap.ParseOpenAPI(data)
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
