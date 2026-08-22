package conformance

import (
	"context"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/searchconsole/v1"
)

// TestSearchConsoleConformance drives Google's generated Search Console
// client (google.golang.org/api/searchconsole/v1) against the
// gsearchconsole-style adapter: site lifecycle, search analytics query, and
// sitemap submission through the stock SDK.
func TestSearchConsoleConformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "gsearchconsole-style")

	svc, err := searchconsole.NewService(ctx,
		option.WithEndpoint(base),
		// The adapter validates bearers against its token store.
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "mock-oauth2-token"})),
	)
	if err != nil {
		t.Fatalf("searchconsole.NewService: %v", err)
	}

	domain := "sc-domain:example.com"

	// ===== Sites.List returns the seeded properties =====
	sites, err := svc.Sites.List().Do()
	if err != nil {
		t.Fatalf("sites.list: %v", err)
	}
	found := false
	for _, s := range sites.SiteEntry {
		if s.SiteUrl == domain {
			found = true
		}
	}
	if !found {
		t.Fatalf("sites.list: %q missing from %+v", domain, sites.SiteEntry)
	}
	Record(t, "google-api-go-client", "gsearchconsole-style", "Sites.List returns the seeded properties")

	// ===== Sites.Get resolves a seeded property =====
	site, err := svc.Sites.Get(domain).Do()
	if err != nil {
		t.Fatalf("sites.get: %v", err)
	}
	if site.PermissionLevel != "siteOwner" {
		t.Errorf("sites.get: permissionLevel=%q", site.PermissionLevel)
	}
	Record(t, "google-api-go-client", "gsearchconsole-style", "Sites.Get returns the property with permissionLevel")

	// ===== Sites.Add registers a new property for verification =====
	if err := svc.Sites.Add("sc-domain:added-by-sdk.com").Do(); err != nil {
		t.Fatalf("sites.add: %v", err)
	}
	added, err := svc.Sites.Get("sc-domain:added-by-sdk.com").Do()
	if err != nil {
		t.Fatalf("sites.get added: %v", err)
	}
	if added.PermissionLevel != "siteUnverifiedUser" {
		t.Errorf("sites.get added: permissionLevel=%q (verification derives on the clock)", added.PermissionLevel)
	}
	Record(t, "google-api-go-client", "gsearchconsole-style", "Sites.Add starts unverified and lists immediately")

	// ===== Searchanalytics.Query aggregates rows over the date range =====
	rows, err := svc.Searchanalytics.Query(domain, &searchconsole.SearchAnalyticsQueryRequest{
		StartDate:  "2026-01-01",
		EndDate:    "2026-01-07",
		Dimensions: []string{"query"},
		RowLimit:   10,
	}).Do()
	if err != nil {
		t.Fatalf("searchanalytics.query: %v", err)
	}
	if len(rows.Rows) == 0 {
		t.Fatalf("searchanalytics.query: no rows")
	}
	for _, r := range rows.Rows {
		if len(r.Keys) != 1 || r.Clicks < 0 || r.Impressions <= 0 {
			t.Errorf("searchanalytics.query row: keys=%v clicks=%v impressions=%v", r.Keys, r.Clicks, r.Impressions)
		}
	}
	Record(t, "google-api-go-client", "gsearchconsole-style", "Searchanalytics.Query returns keyed metric rows")

	// ===== Sitemaps.List + Submit round-trip =====
	sitemaps, err := svc.Sitemaps.List(domain).Do()
	if err != nil {
		t.Fatalf("sitemaps.list: %v", err)
	}
	hasSeeded := false
	for _, sm := range sitemaps.Sitemap {
		if sm.Path == "https://example.com/sitemap.xml" {
			hasSeeded = true
		}
	}
	if !hasSeeded {
		t.Errorf("sitemaps.list: seeded sitemap missing (%+v)", sitemaps.Sitemap)
	}
	if err := svc.Sitemaps.Submit(domain, "sdk-sitemap.xml").Do(); err != nil {
		t.Fatalf("sitemaps.submit: %v", err)
	}
	after, err := svc.Sitemaps.List(domain).Do()
	if err != nil {
		t.Fatalf("sitemaps.list after submit: %v", err)
	}
	submitted := false
	for _, sm := range after.Sitemap {
		if sm.Path == "https://example.com/sdk-sitemap.xml" {
			submitted = true
		}
	}
	if !submitted {
		t.Errorf("sitemaps.list after submit: %q missing", "https://example.com/sdk-sitemap.xml")
	}
	Record(t, "google-api-go-client", "gsearchconsole-style", "Sitemaps.Submit + List round-trip")

	// ===== UrlInspection.Index.Inspect returns a verdict =====
	inspection, err := svc.UrlInspection.Index.Inspect(&searchconsole.InspectUrlIndexRequest{
		InspectionUrl: "https://example.com/",
		SiteUrl:       domain,
	}).Do()
	if err != nil {
		t.Fatalf("urlInspection.index.inspect: %v", err)
	}
	if inspection.InspectionResult == nil || inspection.InspectionResult.IndexStatusResult == nil {
		t.Fatalf("urlInspection.inspect: %+v", inspection)
	}
	Record(t, "google-api-go-client", "gsearchconsole-style", "UrlInspection.Index.Inspect returns an index-status verdict")

	// ===== Unknown site decodes googleapi 404 =====
	_, err = svc.Sites.Get("sc-domain:missing.com").Do()
	if gErr, ok := err.(*googleapi.Error); !ok || gErr.Code != 404 {
		t.Errorf("sites.get unknown: want googleapi 404, got %v", err)
	}
	Record(t, "google-api-go-client", "gsearchconsole-style", "Unknown site Get -> googleapi 404")
}
