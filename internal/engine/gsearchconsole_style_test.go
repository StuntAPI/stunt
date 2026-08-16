package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// gscSiteURL percent-encodes a Search Console property identifier for use in
// a request path.
func gscSiteURL(s string) string {
	return url.QueryEscape(s)
}

// gscQuery posts a searchAnalytics query and decodes the response.
func gscQuery(t *testing.T, base, token, site string, body map[string]any) (map[string]any, int) {
	t.Helper()
	raw, status := gscPost(t, base+"/webmasters/v3/sites/"+gscSiteURL(site)+"/searchAnalytics/query", token, body)
	var resp map[string]any
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal query response: %v (body %s)", err, raw)
	}
	return resp, status
}

// gscInspect posts to the real URL Inspection endpoint and decodes the response.
func gscInspect(t *testing.T, base, token string, body map[string]any) (map[string]any, int) {
	t.Helper()
	raw, status := gscPost(t, base+"/v1/urlInspection/index:inspect", token, body)
	var resp map[string]any
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal inspect response: %v (body %s)", err, raw)
	}
	return resp, status
}

// gscRows returns the rows array (nil when the response omits it).
func gscRows(t *testing.T, resp map[string]any) []any {
	t.Helper()
	rows, _ := resp["rows"].([]any)
	return rows
}

// TestGSearchConsoleStyleAdapter exercises the gsearchconsole-style adapter:
//
//   - 401 without auth (and for an unknown token)
//   - Search analytics: derived rows, aggregation, dimension cross-product,
//     date-range + dimension filters, rowLimit, responseAverages, 400s
//   - Sites: list, lifecycle add → unverified 403 → verified, delete
//   - Sitemaps: list/get/submit/delete
//   - URL inspection: real verdicts + 403/400 error paths
func TestGSearchConsoleStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "gsearchconsole-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"gsc": {Adapter: absAdapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(50 * time.Millisecond)

	base := addrs["gsc"]
	token := "mock-oauth2-token"
	site := "sc-domain:example.com"

	// ===== 401 without auth, and for an unknown token =====

	_, status := gscGet(t, base+"/webmasters/v3/sites", "")
	if status != 401 {
		t.Fatalf("sites without auth -> status %d, want 401", status)
	}
	_, status = gscGet(t, base+"/webmasters/v3/sites", "some-other-token")
	if status != 401 {
		t.Fatalf("sites with unknown token -> status %d, want 401", status)
	}

	// ===== Search analytics: derived rows with real aggregation =====

	resp, status := gscQuery(t, base, token, site, map[string]any{
		"startDate":  "2024-01-01",
		"endDate":    "2024-01-07",
		"dimensions": []string{"query"},
	})
	if status != 200 {
		t.Fatalf("query -> status %d, want 200; resp %v", status, resp)
	}
	rows := gscRows(t, resp)
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5 (one per synthetic query)", len(rows))
	}
	row := rows[0].(map[string]any)
	keys, ok := row["keys"].([]any)
	if !ok || len(keys) != 1 {
		t.Fatalf("keys = %v, want array of 1", row["keys"])
	}
	if _, ok := keys[0].(string); !ok {
		t.Fatalf("keys[0] = %v, want string", keys[0])
	}
	clicks, ok := row["clicks"].(float64)
	if !ok || clicks <= 0 {
		t.Fatalf("clicks = %v, want positive number", row["clicks"])
	}
	impressions, ok := row["impressions"].(float64)
	if !ok || impressions < clicks {
		t.Fatalf("impressions = %v, want >= clicks %v", row["impressions"], clicks)
	}
	if ctr, ok := row["ctr"].(float64); !ok || ctr <= 0 || ctr > 1 {
		t.Fatalf("ctr = %v, want (0,1]", row["ctr"])
	}
	if pos, ok := row["position"].(float64); !ok || pos < 1 || pos > 10 {
		t.Fatalf("position = %v, want average rank in (1,10)", row["position"])
	}
	// Rows are sorted by clicks desc.
	for i := 1; i < len(rows); i++ {
		prev := rows[i-1].(map[string]any)["clicks"].(float64)
		cur := rows[i].(map[string]any)["clicks"].(float64)
		if cur > prev {
			t.Fatalf("rows not sorted by clicks desc: %v then %v", prev, cur)
		}
	}

	// responseAverages aggregate the whole derived range.
	avgs, ok := resp["responseAverages"].(map[string]any)
	if !ok {
		t.Fatalf("responseAverages = %v, want object", resp["responseAverages"])
	}
	if _, ok := avgs["clicks"].(float64); !ok {
		t.Fatalf("responseAverages.clicks = %v, want number", avgs["clicks"])
	}
	if _, ok := avgs["position"].(float64); !ok {
		t.Fatalf("responseAverages.position = %v, want number", avgs["position"])
	}

	// ===== Dimension cross-product + drill-down consistency =====

	resp2, status := gscQuery(t, base, token, site, map[string]any{
		"startDate":  "2024-01-01",
		"endDate":    "2024-01-07",
		"dimensions": []string{"query", "device"},
	})
	if status != 200 {
		t.Fatalf("query q+d -> status %d, want 200", status)
	}
	rows2 := gscRows(t, resp2)
	if len(rows2) != 15 {
		t.Fatalf("query×device rows = %d, want 15 (5 queries × 3 devices)", len(rows2))
	}
	byQuery := map[string]float64{}
	for _, r := range rows2 {
		k := r.(map[string]any)["keys"].([]any)
		byQuery[k[0].(string)] += r.(map[string]any)["clicks"].(float64)
	}
	for _, r := range rows {
		k := r.(map[string]any)["keys"].([]any)[0].(string)
		if byQuery[k] != r.(map[string]any)["clicks"].(float64) {
			t.Fatalf("drill-down mismatch for %q: query=%v q+d=%v", k,
				r.(map[string]any)["clicks"], byQuery[k])
		}
	}

	// Date dimension produces one row per day × query.
	resp3, status := gscQuery(t, base, token, site, map[string]any{
		"startDate":  "2024-01-01",
		"endDate":    "2024-01-07",
		"dimensions": []string{"date", "query"},
	})
	if status != 200 {
		t.Fatalf("query date+q -> status %d, want 200", status)
	}
	if rows3 := gscRows(t, resp3); len(rows3) != 35 {
		t.Fatalf("date×query rows = %d, want 35 (7 days × 5 queries)", len(rows3))
	}

	// ===== Date range honored: narrower range changes the numbers =====

	respNarrow, _ := gscQuery(t, base, token, site, map[string]any{
		"startDate":  "2024-01-01",
		"endDate":    "2024-01-01",
		"dimensions": []string{"query"},
	})
	wideClicks := avgs["clicks"].(float64)
	narrowClicks := respNarrow["responseAverages"].(map[string]any)["clicks"].(float64)
	if narrowClicks <= 0 || narrowClicks > wideClicks {
		t.Fatalf("1-day clicks %v should be <= 7-day clicks %v", narrowClicks, wideClicks)
	}

	// ===== Dimension filters =====

	respF, _ := gscQuery(t, base, token, site, map[string]any{
		"startDate":  "2024-01-01",
		"endDate":    "2024-01-07",
		"dimensions": []string{"query"},
		"dimensionFilterGroups": []any{
			map[string]any{
				"groupType": "and",
				"filters": []any{
					map[string]any{"dimension": "query", "operator": "equals", "expression": "python tutorial"},
				},
			},
		},
	})
	rowsF := gscRows(t, respF)
	if len(rowsF) != 1 || rowsF[0].(map[string]any)["keys"].([]any)[0] != "python tutorial" {
		t.Fatalf("filtered rows = %v, want exactly the python tutorial row", rowsF)
	}

	// A filter matching nothing omits rows entirely (real behavior).
	respNone, _ := gscQuery(t, base, token, site, map[string]any{
		"startDate":  "2024-01-01",
		"endDate":    "2024-01-07",
		"dimensions": []string{"query"},
		"dimensionFilterGroups": []any{
			map[string]any{
				"filters": []any{
					map[string]any{"dimension": "query", "operator": "equals", "expression": "no such query"},
				},
			},
		},
	})
	if rowsNone := gscRows(t, respNone); rowsNone != nil {
		t.Fatalf("no-match filter rows = %v, want omitted", rowsNone)
	}

	// ===== rowLimit / startRow =====

	respL, _ := gscQuery(t, base, token, site, map[string]any{
		"startDate":  "2024-01-01",
		"endDate":    "2024-01-07",
		"dimensions": []string{"query"},
		"rowLimit":   2,
		"startRow":   1,
	})
	if rowsL := gscRows(t, respL); len(rowsL) != 2 {
		t.Fatalf("rowLimit=2 startRow=1 rows = %d, want 2", len(rowsL))
	}

	// ===== Analytics error paths =====

	for name, body := range map[string]map[string]any{
		"unknown dimension": {"startDate": "2024-01-01", "endDate": "2024-01-07", "dimensions": []string{"country"}},
		"missing dates":     {"dimensions": []string{"query"}},
		"reversed range":    {"startDate": "2024-01-07", "endDate": "2024-01-01"},
	} {
		_, st := gscQuery(t, base, token, site, body)
		if st != 400 {
			t.Fatalf("%s -> status %d, want 400", name, st)
		}
	}
	_, status = gscQuery(t, base, token, "sc-domain:not-owned.example", map[string]any{
		"startDate": "2024-01-01", "endDate": "2024-01-07",
	})
	if status != 403 {
		t.Fatalf("query on unknown property -> status %d, want 403", status)
	}

	// ===== Sites: list + lifecycle =====

	body, status := gscGet(t, base+"/webmasters/v3/sites", token)
	if status != 200 {
		t.Fatalf("sites -> status %d, want 200; body %s", status, body)
	}
	var sitesResp map[string]any
	if err := json.Unmarshal([]byte(body), &sitesResp); err != nil {
		t.Fatalf("unmarshal sites: %v (body %s)", err, body)
	}
	siteEntry, ok := sitesResp["siteEntry"].([]any)
	if !ok || len(siteEntry) < 1 {
		t.Fatalf("siteEntry = %v, want non-empty array", sitesResp["siteEntry"])
	}
	first := siteEntry[0].(map[string]any)
	if _, ok := first["siteUrl"].(string); !ok {
		t.Fatalf("siteUrl = %v, want string", first["siteUrl"])
	}
	if _, ok := first["permissionLevel"].(string); !ok {
		t.Fatalf("permissionLevel = %v, want string", first["permissionLevel"])
	}

	// GET one seeded site.
	body, status = gscGet(t, base+"/webmasters/v3/sites/"+gscSiteURL(site), token)
	if status != 200 {
		t.Fatalf("get site -> status %d, want 200; body %s", status, body)
	}

	// PUT a new property → 204, unverified (403 on analytics).
	newSite := "sc-domain:added.example.org"
	if body, status = gscDo(t, "PUT", base+"/webmasters/v3/sites/"+gscSiteURL(newSite), token, nil); status != 204 {
		t.Fatalf("add site -> status %d, want 204; body %s", status, body)
	}
	body, status = gscGet(t, base+"/webmasters/v3/sites/"+gscSiteURL(newSite), token)
	if status != 200 {
		t.Fatalf("get added site -> status %d, want 200; body %s", status, body)
	}
	var added map[string]any
	if err := json.Unmarshal([]byte(body), &added); err != nil {
		t.Fatalf("unmarshal added site: %v", err)
	}
	if added["permissionLevel"] != "siteUnverifiedUser" {
		t.Fatalf("added site permissionLevel = %v, want siteUnverifiedUser", added["permissionLevel"])
	}
	_, status = gscQuery(t, base, token, newSite, map[string]any{
		"startDate": "2024-01-01", "endDate": "2024-01-07",
	})
	if status != 403 {
		t.Fatalf("analytics on unverified property -> status %d, want 403", status)
	}

	// Verification derives from the clock (~2s).
	time.Sleep(2300 * time.Millisecond)
	body, status = gscGet(t, base+"/webmasters/v3/sites/"+gscSiteURL(newSite), token)
	if status != 200 {
		t.Fatalf("get verified site -> status %d, want 200", status)
	}
	if err := json.Unmarshal([]byte(body), &added); err != nil {
		t.Fatalf("unmarshal verified site: %v", err)
	}
	if added["permissionLevel"] != "siteFullUser" {
		t.Fatalf("verified permissionLevel = %v, want siteFullUser", added["permissionLevel"])
	}
	_, status = gscQuery(t, base, token, newSite, map[string]any{
		"startDate": "2024-01-01", "endDate": "2024-01-07",
	})
	if status != 200 {
		t.Fatalf("analytics on verified property -> status %d, want 200", status)
	}

	// Invalid property form → 400.
	if body, status = gscDo(t, "PUT", base+"/webmasters/v3/sites/not-a-site", token, nil); status != 400 {
		t.Fatalf("add invalid site -> status %d, want 400; body %s", status, body)
	}

	// DELETE → 204 then 404.
	if body, status = gscDo(t, "DELETE", base+"/webmasters/v3/sites/"+gscSiteURL(newSite), token, nil); status != 204 {
		t.Fatalf("delete site -> status %d, want 204; body %s", status, body)
	}
	if body, status = gscDo(t, "DELETE", base+"/webmasters/v3/sites/"+gscSiteURL(newSite), token, nil); status != 404 {
		t.Fatalf("delete again -> status %d, want 404; body %s", status, body)
	}

	// ===== Sitemaps: list / get / submit / delete =====

	body, status = gscGet(t, base+"/webmasters/v3/sites/"+gscSiteURL(site)+"/sitemaps", token)
	if status != 200 {
		t.Fatalf("sitemaps -> status %d, want 200; body %s", status, body)
	}
	var smResp map[string]any
	if err := json.Unmarshal([]byte(body), &smResp); err != nil {
		t.Fatalf("unmarshal sitemaps: %v (body %s)", err, body)
	}
	sitemaps, ok := smResp["sitemap"].([]any)
	if !ok || len(sitemaps) < 1 {
		t.Fatalf("sitemap = %v, want non-empty array", smResp["sitemap"])
	}
	sm := sitemaps[0].(map[string]any)
	if _, ok := sm["path"].(string); !ok {
		t.Fatalf("sitemap path = %v, want string", sm["path"])
	}
	if _, ok := sm["lastSubmitted"].(string); !ok {
		t.Fatalf("lastSubmitted = %v, want string", sm["lastSubmitted"])
	}

	// Get one (200) and an unknown feedpath (404).
	if body, status = gscGet(t, base+"/webmasters/v3/sites/"+gscSiteURL(site)+"/sitemaps/sitemap.xml", token); status != 200 {
		t.Fatalf("get sitemap -> status %d, want 200; body %s", status, body)
	}
	if body, status = gscGet(t, base+"/webmasters/v3/sites/"+gscSiteURL(site)+"/sitemaps/missing.xml", token); status != 404 {
		t.Fatalf("get missing sitemap -> status %d, want 404; body %s", status, body)
	}

	// Submit → 204, then appears in the list; delete → 204, then 404.
	if body, status = gscDo(t, "PUT", base+"/webmasters/v3/sites/"+gscSiteURL(site)+"/sitemaps/news.xml", token, nil); status != 204 {
		t.Fatalf("submit sitemap -> status %d, want 204; body %s", status, body)
	}
	body, _ = gscGet(t, base+"/webmasters/v3/sites/"+gscSiteURL(site)+"/sitemaps", token)
	_ = json.Unmarshal([]byte(body), &smResp)
	found := false
	for _, s := range smResp["sitemap"].([]any) {
		if s.(map[string]any)["path"] == "https://example.com/news.xml" {
			found = true
		}
	}
	if !found {
		t.Fatalf("submitted sitemap not listed: %v", smResp["sitemap"])
	}
	if body, status = gscDo(t, "DELETE", base+"/webmasters/v3/sites/"+gscSiteURL(site)+"/sitemaps/news.xml", token, nil); status != 204 {
		t.Fatalf("delete sitemap -> status %d, want 204; body %s", status, body)
	}
	if body, status = gscDo(t, "DELETE", base+"/webmasters/v3/sites/"+gscSiteURL(site)+"/sitemaps/news.xml", token, nil); status != 404 {
		t.Fatalf("delete missing sitemap -> status %d, want 404; body %s", status, body)
	}

	// ===== URL inspection (real endpoint shape) =====

	insp, status := gscInspect(t, base, token, map[string]any{
		"inspectionUrl": "https://www.example.com/page",
		"siteUrl":       site,
		"languageCode":  "en",
	})
	if status != 200 {
		t.Fatalf("inspect -> status %d, want 200; resp %v", status, insp)
	}
	ir, ok := insp["inspectionResult"].(map[string]any)
	if !ok {
		t.Fatalf("inspectionResult = %v, want object", insp["inspectionResult"])
	}
	if _, ok := ir["inspectionResultLink"].(string); !ok {
		t.Fatalf("inspectionResultLink = %v, want string", ir["inspectionResultLink"])
	}
	idx, ok := ir["indexStatusResult"].(map[string]any)
	if !ok {
		t.Fatalf("indexStatusResult = %v, want object", ir["indexStatusResult"])
	}
	if idx["verdict"] != "PASS" {
		t.Fatalf("verdict = %v, want PASS", idx["verdict"])
	}
	if idx["coverageState"] != "Indexed" {
		t.Fatalf("coverageState = %v, want Indexed", idx["coverageState"])
	}
	if idx["robotsTxtState"] != "ALLOWED" {
		t.Fatalf("robotsTxtState = %v, want ALLOWED", idx["robotsTxtState"])
	}
	if _, ok := idx["lastCrawlTime"].(string); !ok {
		t.Fatalf("lastCrawlTime = %v, want string", idx["lastCrawlTime"])
	}
	if _, ok := idx["googleCanonical"].(string); !ok {
		t.Fatalf("googleCanonical = %v, want string", idx["googleCanonical"])
	}
	if mu, ok := ir["mobileUsabilityResult"].(map[string]any); !ok || mu["verdict"] == nil {
		t.Fatalf("mobileUsabilityResult = %v, want verdict", ir["mobileUsabilityResult"])
	}

	// Deterministic alternate verdict: noindex → NEUTRAL / BLOCKED_BY_META_TAG.
	insp, _ = gscInspect(t, base, token, map[string]any{
		"inspectionUrl": "https://www.example.com/noindex",
		"siteUrl":       site,
	})
	idx = insp["inspectionResult"].(map[string]any)["indexStatusResult"].(map[string]any)
	if idx["verdict"] != "NEUTRAL" || idx["indexingState"] != "BLOCKED_BY_META_TAG" {
		t.Fatalf("noindex inspection = %v, want NEUTRAL/BLOCKED_BY_META_TAG", idx)
	}

	// Unknown property → 403 PERMISSION_DENIED; URL outside property → 400.
	_, status = gscInspect(t, base, token, map[string]any{
		"inspectionUrl": "https://www.example.org/",
		"siteUrl":       "sc-domain:not-owned.example",
	})
	if status != 403 {
		t.Fatalf("inspect unknown property -> status %d, want 403", status)
	}
	_, status = gscInspect(t, base, token, map[string]any{
		"inspectionUrl": "https://elsewhere.example.net/page",
		"siteUrl":       site,
	})
	if status != 400 {
		t.Fatalf("inspect outside property -> status %d, want 400", status)
	}
	_, status = gscInspect(t, base, token, map[string]any{
		"siteUrl": site,
	})
	if status != 400 {
		t.Fatalf("inspect without inspectionUrl -> status %d, want 400", status)
	}

	// Legacy simulator route still inspects against the property.
	raw, status := gscPost(t, base+"/webmasters/v3/sites/"+gscSiteURL(site)+"/inspect", token,
		map[string]any{"inspectionUrl": "https://www.example.com/page"})
	if status != 200 {
		t.Fatalf("legacy inspect -> status %d, want 200; body %s", status, raw)
	}
	var legacy map[string]any
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
		t.Fatalf("unmarshal legacy inspect: %v", err)
	}
	if legacy["inspectionResult"].(map[string]any)["indexStatusResult"].(map[string]any)["verdict"] != "PASS" {
		t.Fatalf("legacy inspect verdict = %v, want PASS", legacy)
	}

	// Undecodable body → 400 (not a 500).
	raw, status = gscPostRaw(t, base+"/webmasters/v3/sites/"+gscSiteURL(site)+"/searchAnalytics/query", token, "not json")
	if status != 400 {
		t.Fatalf("undecodable query body -> status %d, want 400; body %s", status, raw)
	}
}

// === GSC test helpers ===

func gscGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	return gscDo(t, "GET", rawurl, token, nil)
}

func gscPost(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	return gscPostRaw(t, rawurl, token, string(data))
}

func gscPostRaw(t *testing.T, rawurl, token, body string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func gscDo(t *testing.T, method, rawurl, token string, payload map[string]any) (string, int) {
	t.Helper()
	var reader io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, rawurl, reader)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
