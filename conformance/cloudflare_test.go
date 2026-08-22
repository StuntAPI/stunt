package conformance

import (
	"context"
	"errors"
	"testing"

	cloudflare "github.com/cloudflare/cloudflare-go"
)

// TestCloudflareConformance drives cloudflare-go (the de-facto standard Go
// client for the Cloudflare v4 API) against the cloudflare-style adapter:
// cloudflare.BaseURL repoints the client and its envelope decoding, error
// surfacing and ResourceContainer routing all stay stock.
func TestCloudflareConformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "cloudflare-style")

	api, err := cloudflare.NewWithAPIToken("conformance-token", cloudflare.BaseURL(base))
	if err != nil {
		t.Fatalf("NewWithAPIToken: %v", err)
	}

	// ===== ListZones returns the seeded zone =====
	zones, err := api.ListZones(ctx)
	if err != nil {
		t.Fatalf("zones.list: %v", err)
	}
	if len(zones) == 0 || zones[0].Name != "stunt.dev" {
		t.Fatalf("zones.list: got %v", zones)
	}
	zoneID := zones[0].ID
	Record(t, "cloudflare-go", "cloudflare-style", "Zones list returns the seeded zone")

	// ===== ListZones by name narrows to that zone =====
	byName, err := api.ListZones(ctx, "stunt.dev")
	if err != nil {
		t.Fatalf("zones.list by name: %v", err)
	}
	if len(byName) != 1 || byName[0].Name != "stunt.dev" {
		t.Errorf("zones.list by name: got %v", byName)
	}
	missing, err := api.ListZones(ctx, "missing.example")
	if err != nil || len(missing) != 0 {
		t.Errorf("zones.list unknown name: zones=%d err=%v", len(missing), err)
	}
	Record(t, "cloudflare-go", "cloudflare-style", "Zones list filters by name")

	// ===== CreateZone + ZoneDetails round-trip =====
	created, err := api.CreateZone(ctx, "sdk-zone.example", false, cloudflare.Account{ID: "stunt-account"}, "stunt-account")
	if err != nil {
		t.Fatalf("zones.create: %v", err)
	}
	details, err := api.ZoneDetails(ctx, created.ID)
	if err != nil {
		t.Fatalf("zone.details: %v", err)
	}
	if details.Name != "sdk-zone.example" || details.Status == "" {
		t.Errorf("zone.details: name=%q status=%q", details.Name, details.Status)
	}
	Record(t, "cloudflare-go", "cloudflare-style", "CreateZone + ZoneDetails round-trip")

	// ===== Duplicate CreateZone decodes the Cloudflare error envelope =====
	_, err = api.CreateZone(ctx, "sdk-zone.example", false, cloudflare.Account{ID: "stunt-account"}, "stunt-account")
	var cfErr *cloudflare.Error
	if !errors.As(err, &cfErr) || len(cfErr.Errors) != 1 || cfErr.Errors[0].Code != 1061 {
		t.Errorf("zones.create duplicate: want cloudflare error 1061, got %v (%+v)", err, cfErr)
	}
	Record(t, "cloudflare-go", "cloudflare-style", "Duplicate CreateZone -> error code 1061")

	// ===== DNS records: create, list, update round-trip =====
	rc := cloudflare.ZoneIdentifier(zoneID)
	rec, err := api.CreateDNSRecord(ctx, rc, cloudflare.CreateDNSRecordParams{
		Type: "A", Name: "sdk.stunt.dev", Content: "203.0.113.10", TTL: 300,
	})
	if err != nil {
		t.Fatalf("dns.create: %v", err)
	}
	recs, _, err := api.ListDNSRecords(ctx, rc, cloudflare.ListDNSRecordsParams{})
	if err != nil {
		t.Fatalf("dns.list: %v", err)
	}
	found := false
	for _, r := range recs {
		if r.ID == rec.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("dns.list: created record %q missing", rec.ID)
	}
	updated, err := api.UpdateDNSRecord(ctx, rc, cloudflare.UpdateDNSRecordParams{
		ID: rec.ID, Content: "203.0.113.99",
	})
	if err != nil {
		t.Fatalf("dns.update: %v", err)
	}
	if updated.Content != "203.0.113.99" {
		t.Errorf("dns.update: content=%q", updated.Content)
	}
	Record(t, "cloudflare-go", "cloudflare-style", "DNS records create + list + update round-trip")

	// ===== Firewall rules: batch create + list =====
	fw, err := api.CreateFirewallRules(ctx, rc, []cloudflare.FirewallRuleCreateParams{{
		Action:      "block",
		Description: "sdk conformance block",
		Filter:      cloudflare.Filter{Expression: `ip.src eq 198.51.100.1`},
	}})
	if err != nil {
		t.Fatalf("firewall.create: %v", err)
	}
	if len(fw) != 1 || fw[0].ID == "" {
		t.Fatalf("firewall.create: got %+v", fw)
	}
	fwList, _, err := api.FirewallRules(ctx, rc, cloudflare.FirewallRuleListParams{})
	if err != nil {
		t.Fatalf("firewall.list: %v", err)
	}
	if len(fwList) != 1 || fwList[0].Filter.Expression != "ip.src eq 198.51.100.1" {
		t.Errorf("firewall.list: got %+v", fwList)
	}
	Record(t, "cloudflare-go", "cloudflare-style", "Firewall rules batch create + list with filter expression")

	// ===== Record delete is observable, zone delete 404s later reads =====
	if err := api.DeleteDNSRecord(ctx, rc, rec.ID); err != nil {
		t.Fatalf("dns.delete: %v", err)
	}
	if _, err := api.DeleteZone(ctx, created.ID); err != nil {
		t.Fatalf("zones.delete: %v", err)
	}
	if _, err := api.ZoneDetails(ctx, created.ID); err == nil {
		t.Errorf("zone.details after delete: want error, got none")
	}
	Record(t, "cloudflare-go", "cloudflare-style", "DNS record delete + zone delete then details fails")
}
