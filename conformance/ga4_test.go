package conformance

import (
	"context"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/analyticsadmin/v1beta"
	"google.golang.org/api/analyticsdata/v1beta"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// TestGA4Conformance drives Google's own generated GA4 clients — the Admin
// API (analyticsadmin) and the Data API (analyticsdata) — against the
// ga4-style adapter: hierarchy discovery, filtered property listing, and
// runReport/runRealtimeReport through the stock SDKs.
func TestGA4Conformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "ga4-style")

	tokenOpt := []option.ClientOption{
		option.WithEndpoint(base),
		// The adapter validates bearers against its token store.
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "ya29.mock-token"})),
	}
	admin, err := analyticsadmin.NewService(ctx, tokenOpt...)
	if err != nil {
		t.Fatalf("analyticsadmin.NewService: %v", err)
	}
	data, err := analyticsdata.NewService(ctx, tokenOpt...)
	if err != nil {
		t.Fatalf("analyticsdata.NewService: %v", err)
	}

	// ===== Accounts.List returns the seeded hierarchy =====
	accounts, err := admin.Accounts.List().Do()
	if err != nil {
		t.Fatalf("accounts.list: %v", err)
	}
	if len(accounts.Accounts) == 0 || accounts.Accounts[0].Name != "accounts/100001" {
		t.Fatalf("accounts.list: %+v", accounts.Accounts)
	}
	Record(t, "google-api-go-client", "ga4-style", "Admin Accounts.List returns the seeded account")

	// ===== Properties.List filters by parent account =====
	props, err := admin.Properties.List().Filter("parent:accounts/100001").Do()
	if err != nil {
		t.Fatalf("properties.list: %v", err)
	}
	if len(props.Properties) == 0 || props.Properties[0].Name != "properties/123456789" {
		t.Fatalf("properties.list: %+v", props.Properties)
	}
	Record(t, "google-api-go-client", "ga4-style", "Admin Properties.List filters by parent account")

	// ===== DataStreams.List walks under the property =====
	streams, err := admin.Properties.DataStreams.List("properties/123456789").Do()
	if err != nil {
		t.Fatalf("datastreams.list: %v", err)
	}
	if len(streams.DataStreams) == 0 {
		t.Fatalf("datastreams.list: empty")
	}
	Record(t, "google-api-go-client", "ga4-style", "Admin DataStreams.List returns the property streams")

	// ===== runReport shapes dimension and metric headers =====
	report, err := data.Properties.RunReport("properties/123456789", &analyticsdata.RunReportRequest{
		DateRanges: []*analyticsdata.DateRange{{StartDate: "2026-01-01", EndDate: "2026-01-31"}},
		Dimensions: []*analyticsdata.Dimension{{Name: "country"}},
		Metrics:    []*analyticsdata.Metric{{Name: "sessions"}, {Name: "activeUsers"}},
	}).Do()
	if err != nil {
		t.Fatalf("properties.runReport: %v", err)
	}
	if len(report.DimensionHeaders) != 1 || report.DimensionHeaders[0].Name != "country" {
		t.Errorf("runReport: dimensionHeaders=%+v", report.DimensionHeaders)
	}
	if len(report.MetricHeaders) != 2 || report.RowCount <= 0 || len(report.Rows) == 0 {
		t.Errorf("runReport: metricHeaders=%d rowCount=%d rows=%d", len(report.MetricHeaders), report.RowCount, len(report.Rows))
	}
	Record(t, "google-api-go-client", "ga4-style", "Data runReport returns headers, rows, and rowCount")

	// ===== runReport limit/offset pages deterministically =====
	full := report.Rows
	page1, err := data.Properties.RunReport("properties/123456789", &analyticsdata.RunReportRequest{
		DateRanges: []*analyticsdata.DateRange{{StartDate: "2026-01-01", EndDate: "2026-01-31"}},
		Dimensions: []*analyticsdata.Dimension{{Name: "country"}},
		Metrics:    []*analyticsdata.Metric{{Name: "sessions"}},
		Limit:      1,
	}).Do()
	if err != nil {
		t.Fatalf("runReport page1: %v", err)
	}
	if len(page1.Rows) != 1 || len(full) < 2 || page1.Rows[0].DimensionValues[0].Value != full[0].DimensionValues[0].Value {
		t.Fatalf("runReport limit=1: rows=%d", len(page1.Rows))
	}
	Record(t, "google-api-go-client", "ga4-style", "Data runReport limit pages rows deterministically")

	// ===== runRealtimeReport returns live-shaped rows =====
	rt, err := data.Properties.RunRealtimeReport("properties/123456789", &analyticsdata.RunRealtimeReportRequest{
		Dimensions: []*analyticsdata.Dimension{{Name: "country"}},
		Metrics:    []*analyticsdata.Metric{{Name: "activeUsers"}},
	}).Do()
	if err != nil {
		t.Fatalf("properties.runRealtimeReport: %v", err)
	}
	if len(rt.Rows) == 0 {
		t.Fatalf("runRealtimeReport: no rows")
	}
	Record(t, "google-api-go-client", "ga4-style", "Data runRealtimeReport returns rows")

	// ===== Unknown dimension surfaces googleapi 400 =====
	_, err = data.Properties.RunReport("properties/123456789", &analyticsdata.RunReportRequest{
		DateRanges: []*analyticsdata.DateRange{{StartDate: "2026-01-01", EndDate: "2026-01-31"}},
		Dimensions: []*analyticsdata.Dimension{{Name: "notADimension"}},
	}).Do()
	if gErr, ok := err.(*googleapi.Error); !ok || gErr.Code != 400 {
		t.Errorf("runReport invalid dimension: want googleapi 400, got %v", err)
	}
	Record(t, "google-api-go-client", "ga4-style", "Unknown dimension runReport -> googleapi 400")
}
