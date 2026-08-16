package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestBrazeStyleAdapter exercises the braze-style adapter:
//
//   - 401 without auth / with a bogus key
//   - users/track persists attributes, events, purchases (per-record
//     non-fatal errors, 75-id fatal cap, malformed body)
//   - users/export/ids (full profile incl. custom_attributes/custom_events/
//     purchases aggregates, fields_to_export, invalid ids, 50-id cap)
//   - users/alias/new + users/identify (real new/existing/merge semantics)
//   - users/delete hard-deletes
//   - messages/send + campaigns/trigger/send validate against the campaigns
//     store (Braze fatal-error vocabulary) and mint 32-hex dispatch ids
//   - segments/list
func TestBrazeStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "braze-style")
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
			"braze": {Adapter: absAdapterDir},
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

	base := addrs["braze"]
	token := "test-app-group-api-key"

	// ===== 401 without auth =====

	_, status := brazePost(t, base+"/users/track", "", map[string]any{
		"attributes": []map[string]any{{"external_id": "user1"}},
	})
	if status != 401 {
		t.Fatalf("track without auth -> status %d, want 401", status)
	}

	// ===== 401 with an unknown (bogus) API key =====

	_, status = brazePost(t, base+"/users/track", "bogus-app-group-key", map[string]any{
		"attributes": []map[string]any{{"external_id": "user1"}},
	})
	if status != 401 {
		t.Fatalf("track with bogus key -> status %d, want 401", status)
	}

	// ===== users/track: attributes + events + purchases persisted =====

	body, status := brazePost(t, base+"/users/track", token, map[string]any{
		"attributes": []map[string]any{
			{"external_id": "user001", "first_name": "Alice", "email": "alice@example.com", "loyalty_tier": "gold"},
			{"external_id": "user002", "first_name": "Bob", "email": "bob@example.com"},
		},
		"events": []map[string]any{
			{"external_id": "user001", "app_id": "app-001", "name": "purchase", "time": "2024-01-01T00:00:00Z",
				"properties": map[string]any{"sku": "widget"}},
			{"external_id": "user001", "app_id": "app-001", "name": "purchase", "time": "2024-02-02T00:00:00Z"},
		},
		"purchases": []map[string]any{
			{"external_id": "user001", "app_id": "app-001", "product_id": "product_x", "currency": "USD",
				"price": 12.5, "quantity": 2, "time": "2024-03-03T10:00:00Z"},
		},
	})
	if status != 200 {
		t.Fatalf("track -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	if resp["message"] != "success" {
		t.Fatalf("message = %v, want success", resp["message"])
	}
	if resp["attributes_processed"] != float64(2) {
		t.Fatalf("attributes_processed = %v, want 2", resp["attributes_processed"])
	}
	if resp["events_processed"] != float64(2) {
		t.Fatalf("events_processed = %v, want 2", resp["events_processed"])
	}
	if resp["purchases_processed"] != float64(1) {
		t.Fatalf("purchases_processed = %v, want 1", resp["purchases_processed"])
	}

	// ===== users/export/ids: full profile with aggregates =====

	body, status = brazePost(t, base+"/users/export/ids", token, map[string]any{
		"external_ids": []string{"user001", "nope"},
	})
	if status != 200 {
		t.Fatalf("export -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal export: %v (body %s)", err, body)
	}
	if resp["message"] != "success" {
		t.Fatalf("export message = %v, want success", resp["message"])
	}
	users, ok := resp["users"].([]any)
	if !ok || len(users) != 1 {
		t.Fatalf("export users = %v, want exactly the 1 known profile", resp["users"])
	}
	u := users[0].(map[string]any)
	if u["external_id"] != "user001" || u["first_name"] != "Alice" {
		t.Fatalf("exported profile = %v", u)
	}
	if _, ok := u["braze_id"].(string); !ok {
		t.Fatalf("braze_id = %v, want string", u["braze_id"])
	}
	ca, ok := u["custom_attributes"].(map[string]any)
	if !ok || ca["loyalty_tier"] != "gold" {
		t.Fatalf("custom_attributes = %v, want loyalty_tier gold", u["custom_attributes"])
	}
	evs, ok := u["custom_events"].([]any)
	if !ok || len(evs) != 1 {
		t.Fatalf("custom_events = %v, want 1 aggregate", u["custom_events"])
	} else {
		ev := evs[0].(map[string]any)
		if ev["name"] != "purchase" || ev["count"] != float64(2) {
			t.Fatalf("custom_events[0] = %v, want purchase x2", ev)
		}
		if ev["first"] != "2024-01-01T00:00:00Z" || ev["last"] != "2024-02-02T00:00:00Z" {
			t.Fatalf("custom_events[0] first/last = %v/%v", ev["first"], ev["last"])
		}
	}
	purs, ok := u["purchases"].([]any)
	if !ok || len(purs) != 1 {
		t.Fatalf("purchases = %v, want 1 aggregate", u["purchases"])
	} else {
		pu := purs[0].(map[string]any)
		if pu["name"] != "product_x" || pu["count"] != float64(1) {
			t.Fatalf("purchases[0] = %v, want product_x x1", pu)
		}
	}
	invalid, ok := resp["invalid_user_ids"].([]any)
	if !ok || len(invalid) != 1 || invalid[0] != "nope" {
		t.Fatalf("invalid_user_ids = %v, want [nope]", resp["invalid_user_ids"])
	}

	// ===== users/export/ids: fields_to_export projection =====

	body, status = brazePost(t, base+"/users/export/ids", token, map[string]any{
		"external_ids":     []string{"user001"},
		"fields_to_export": []string{"external_id", "custom_events"},
	})
	if status != 200 {
		t.Fatalf("export projection -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal export projection: %v (body %s)", err, body)
	}
	u = resp["users"].([]any)[0].(map[string]any)
	if len(u) != 2 {
		t.Fatalf("projected profile = %v, want only external_id + custom_events", u)
	}

	// ===== users/export/ids: >50 ids is a fatal error =====

	tooMany := make([]string, 0, 51)
	for i := 0; i < 51; i++ {
		tooMany = append(tooMany, fmt.Sprintf("u%d", i))
	}
	body, status = brazePost(t, base+"/users/export/ids", token, map[string]any{
		"external_ids": tooMany,
	})
	if status != 400 {
		t.Fatalf("export >50 ids -> status %d, want 400; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal export cap: %v (body %s)", err, body)
	}
	if resp["message"] != "The max number of ids per request was exceeded" {
		t.Fatalf("export cap message = %v", resp["message"])
	}

	// ===== users/track: per-record non-fatal errors =====

	body, status = brazePost(t, base+"/users/track", token, map[string]any{
		"attributes": []map[string]any{
			{"external_id": "user001", "email": "not-an-email"},
			{"external_id": "user003", "email": "carol@example.com"},
		},
		"events": []map[string]any{
			{"external_id": "user001", "name": "bad_time_event", "time": "yesterday"},
		},
	})
	if status != 200 {
		t.Fatalf("track non-fatal -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal non-fatal: %v (body %s)", err, body)
	}
	if resp["message"] != "success" {
		t.Fatalf("non-fatal message = %v, want success (partial ingest)", resp["message"])
	}
	if resp["attributes_processed"] != float64(1) {
		t.Fatalf("non-fatal attributes_processed = %v, want 1", resp["attributes_processed"])
	}
	if resp["events_processed"] != float64(0) {
		t.Fatalf("non-fatal events_processed = %v, want 0", resp["events_processed"])
	}
	errs, ok := resp["errors"].([]any)
	if !ok || len(errs) != 2 {
		t.Fatalf("non-fatal errors = %v, want EMAIL_BAD_FORMAT + BAD_REQUEST entries", resp["errors"])
	}
	names := map[string]bool{}
	for _, e := range errs {
		for k := range e.(map[string]any) {
			names[k] = true
		}
	}
	if !names["EMAIL_BAD_FORMAT"] || !names["BAD_REQUEST"] {
		t.Fatalf("non-fatal error names = %v, want EMAIL_BAD_FORMAT and BAD_REQUEST", names)
	}

	// ===== users/track: _update_existing_only never creates users =====

	body, status = brazePost(t, base+"/users/track", token, map[string]any{
		"attributes": []map[string]any{
			{"external_id": "update-only-ghost", "first_name": "Ghost", "_update_existing_only": true},
		},
	})
	if status != 200 {
		t.Fatalf("track update-only -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal update-only: %v (body %s)", err, body)
	}
	if resp["attributes_processed"] != float64(0) {
		t.Fatalf("update-only attributes_processed = %v, want 0 (unknown user skipped)", resp["attributes_processed"])
	}
	body, _ = brazePost(t, base+"/users/export/ids", token, map[string]any{
		"external_ids": []string{"update-only-ghost"},
	})
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal update-only export: %v (body %s)", err, body)
	}
	if invalid, ok := resp["invalid_user_ids"].([]any); !ok || len(invalid) != 1 {
		t.Fatalf("update-only export invalid_user_ids = %v, want the skipped id", resp["invalid_user_ids"])
	}

	// ===== users/track: braze_id targets the existing profile =====

	body, _ = brazePost(t, base+"/users/export/ids", token, map[string]any{
		"external_ids":     []string{"user001"},
		"fields_to_export": []string{"braze_id"},
	})
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal braze_id export: %v (body %s)", err, body)
	}
	brazeID := resp["users"].([]any)[0].(map[string]any)["braze_id"].(string)
	body, status = brazePost(t, base+"/users/track", token, map[string]any{
		"events": []map[string]any{
			{"braze_id": brazeID, "name": "purchase", "time": "2024-04-04T00:00:00Z"},
		},
	})
	if status != 200 {
		t.Fatalf("track by braze_id -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal track by braze_id: %v (body %s)", err, body)
	}
	if resp["events_processed"] != float64(1) {
		t.Fatalf("track by braze_id events_processed = %v, want 1", resp["events_processed"])
	}
	body, _ = brazePost(t, base+"/users/export/ids", token, map[string]any{
		"external_ids":     []string{"user001"},
		"fields_to_export": []string{"custom_events"},
	})
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal braze_id verify export: %v (body %s)", err, body)
	}
	evs = resp["users"].([]any)[0].(map[string]any)["custom_events"].([]any)
	if evs[0].(map[string]any)["count"] != float64(3) {
		t.Fatalf("purchase count after braze_id track = %v, want 3", evs[0].(map[string]any)["count"])
	}

	// ===== users/track: non-dict records are non-fatal errors, not 500s =====

	body, status = brazePost(t, base+"/users/track", token, map[string]any{
		"attributes": []any{"oops"},
	})
	if status != 200 {
		t.Fatalf("track non-dict record -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal non-dict record: %v (body %s)", err, body)
	}
	if resp["message"] != "success" || resp["attributes_processed"] != float64(0) {
		t.Fatalf("non-dict record response = %v, want success with 0 processed", resp)
	}
	if errs, ok := resp["errors"].([]any); !ok || len(errs) != 1 {
		t.Fatalf("non-dict record errors = %v, want one BAD_REQUEST entry", resp["errors"])
	}

	// ===== users/track: malformed JSON body is a 400, never a 500 =====

	body, status = brazePostRaw(t, base+"/users/track", token, "{not json")
	if status != 400 {
		t.Fatalf("track malformed body -> status %d, want 400; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal malformed: %v (body %s)", err, body)
	}
	if resp["message"] != "Bad Request" {
		t.Fatalf("malformed body message = %v, want Bad Request", resp["message"])
	}

	// ===== users/track: >75 external ids is the documented fatal error =====

	tooManyTrack := make([]map[string]any, 0, 76)
	for i := 0; i < 76; i++ {
		tooManyTrack = append(tooManyTrack, map[string]any{"external_id": fmt.Sprintf("batch-user-%d", i)})
	}
	body, status = brazePost(t, base+"/users/track", token, map[string]any{
		"attributes": tooManyTrack,
	})
	if status != 400 {
		t.Fatalf("track >75 ids -> status %d, want 400; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal track cap: %v (body %s)", err, body)
	}
	if resp["message"] != "Max Input Length Exceeded" {
		t.Fatalf("track cap message = %v, want Max Input Length Exceeded", resp["message"])
	}

	// ===== users/alias/new: alias-only user (external_id omitted) =====

	body, status = brazePost(t, base+"/users/alias/new", token, map[string]any{
		"user_aliases": []map[string]any{
			{"alias_name": "anon-1", "alias_label": "amplitude_id"},
		},
	})
	if status != 200 {
		t.Fatalf("alias/new -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal alias/new: %v (body %s)", err, body)
	}
	if resp["message"] != "success" || resp["aliases_processed"] != float64(1) {
		t.Fatalf("alias/new response = %v", resp)
	}

	// Track data against the alias-only user, then export by alias.
	body, status = brazePost(t, base+"/users/track", token, map[string]any{
		"attributes": []map[string]any{
			{"user_alias": map[string]any{"alias_name": "anon-1", "alias_label": "amplitude_id"}, "home_city": "Chicago"},
		},
		"events": []map[string]any{
			{"user_alias": map[string]any{"alias_name": "anon-1", "alias_label": "amplitude_id"},
				"name": "anonymous_browse", "time": "2024-05-05T00:00:00Z"},
		},
	})
	if status != 200 {
		t.Fatalf("track alias-only -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal track alias-only: %v (body %s)", err, body)
	}
	if resp["attributes_processed"] != float64(1) || resp["events_processed"] != float64(1) {
		t.Fatalf("track alias-only counts = %v/%v, want 1/1", resp["attributes_processed"], resp["events_processed"])
	}
	body, status = brazePost(t, base+"/users/export/ids", token, map[string]any{
		"user_aliases": []map[string]any{{"alias_name": "anon-1", "alias_label": "amplitude_id"}},
	})
	if status != 200 {
		t.Fatalf("export by alias -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal export by alias: %v (body %s)", err, body)
	}
	users = resp["users"].([]any)
	if len(users) != 1 {
		t.Fatalf("export by alias users = %v, want 1", resp["users"])
	}
	u = users[0].(map[string]any)
	if u["home_city"] != "Chicago" {
		t.Fatalf("alias-only profile = %v, want home_city Chicago", u)
	}
	if u["external_id"] != nil {
		t.Fatalf("alias-only external_id = %v, want null before identify", u["external_id"])
	}

	// ===== users/alias/new: existing user gains the alias =====

	body, status = brazePost(t, base+"/users/alias/new", token, map[string]any{
		"user_aliases": []map[string]any{
			{"external_id": "user001", "alias_name": "alice-main", "alias_label": "external_site"},
		},
	})
	if status != 200 {
		t.Fatalf("alias/new existing -> status %d; body %s", status, body)
	}
	body, status = brazePost(t, base+"/users/export/ids", token, map[string]any{
		"external_ids": []string{"user001"},
	})
	if status != 200 {
		t.Fatalf("export after alias -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal export after alias: %v (body %s)", err, body)
	}
	u = resp["users"].([]any)[0].(map[string]any)
	found := false
	for _, a := range u["user_aliases"].([]any) {
		am := a.(map[string]any)
		if am["alias_name"] == "alice-main" && am["alias_label"] == "external_site" {
			found = true
		}
	}
	if !found {
		t.Fatalf("user001 user_aliases = %v, want alice-main/external_site", u["user_aliases"])
	}

	// ===== users/alias/new: unknown external_id adds the alias to no user =====

	body, status = brazePost(t, base+"/users/alias/new", token, map[string]any{
		"user_aliases": []map[string]any{
			{"external_id": "ghost-user", "alias_name": "ghost", "alias_label": "external_site"},
		},
	})
	if status != 200 {
		t.Fatalf("alias/new unknown -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal alias/new unknown: %v (body %s)", err, body)
	}
	if resp["message"] != "success" {
		t.Fatalf("alias/new unknown message = %v, want success (no-op)", resp["message"])
	}
	body, _ = brazePost(t, base+"/users/export/ids", token, map[string]any{
		"user_aliases": []map[string]any{{"alias_name": "ghost", "alias_label": "external_site"}},
	})
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal ghost export: %v (body %s)", err, body)
	}
	if invalid, ok := resp["invalid_user_ids"].([]any); !ok || len(invalid) != 1 {
		t.Fatalf("ghost alias invalid_user_ids = %v, want the alias", resp["invalid_user_ids"])
	}

	// ===== users/identify: alias-only profile becomes identified =====

	body, status = brazePost(t, base+"/users/identify", token, map[string]any{
		"aliases_to_identify": []map[string]any{
			{"external_id": "user100",
				"user_alias": map[string]any{"alias_name": "anon-1", "alias_label": "amplitude_id"}},
		},
	})
	if status != 200 {
		t.Fatalf("identify -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal identify: %v (body %s)", err, body)
	}
	if resp["message"] != "success" || resp["aliases_processed"] != float64(1) {
		t.Fatalf("identify response = %v", resp)
	}
	body, status = brazePost(t, base+"/users/export/ids", token, map[string]any{
		"external_ids": []string{"user100"},
	})
	if status != 200 {
		t.Fatalf("export identified -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal export identified: %v (body %s)", err, body)
	}
	u = resp["users"].([]any)[0].(map[string]any)
	if u["home_city"] != "Chicago" {
		t.Fatalf("identified profile = %v, want merged home_city Chicago", u)
	}
	evs, _ = u["custom_events"].([]any)
	if len(evs) != 1 || evs[0].(map[string]any)["name"] != "anonymous_browse" {
		t.Fatalf("identified custom_events = %v, want the tracked anonymous_browse event", u["custom_events"])
	}
	// The alias-only record is gone, but the alias now resolves to the
	// identified profile (the alias travels with the user).
	body, _ = brazePost(t, base+"/users/export/ids", token, map[string]any{
		"user_aliases": []map[string]any{{"alias_name": "anon-1", "alias_label": "amplitude_id"}},
	})
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal post-identify alias export: %v (body %s)", err, body)
	}
	users, ok = resp["users"].([]any)
	if !ok || len(users) != 1 {
		t.Fatalf("post-identify alias export users = %v, want the single identified profile", resp["users"])
	}
	if au := users[0].(map[string]any); au["external_id"] != "user100" {
		t.Fatalf("alias resolves to %v, want the identified profile user100", au["external_id"])
	}

	// ===== users/identify: merging an alias profile into an existing user =====

	body, status = brazePost(t, base+"/users/alias/new", token, map[string]any{
		"user_aliases": []map[string]any{{"alias_name": "anon-2", "alias_label": "amplitude_id"}},
	})
	if status != 200 {
		t.Fatalf("alias/new anon-2 -> status %d; body %s", status, body)
	}
	body, status = brazePost(t, base+"/users/track", token, map[string]any{
		"attributes": []map[string]any{
			{"user_alias": map[string]any{"alias_name": "anon-2", "alias_label": "amplitude_id"}, "favorite_food": "pizza"},
		},
	})
	if status != 200 {
		t.Fatalf("track anon-2 -> status %d; body %s", status, body)
	}
	body, status = brazePost(t, base+"/users/identify", token, map[string]any{
		"aliases_to_identify": []map[string]any{
			{"external_id": "user002",
				"user_alias": map[string]any{"alias_name": "anon-2", "alias_label": "amplitude_id"}},
		},
	})
	if status != 200 {
		t.Fatalf("identify merge -> status %d; body %s", status, body)
	}
	body, status = brazePost(t, base+"/users/export/ids", token, map[string]any{
		"external_ids": []string{"user002"},
	})
	if status != 200 {
		t.Fatalf("export user002 -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal export user002: %v (body %s)", err, body)
	}
	u = resp["users"].([]any)[0].(map[string]any)
	ca, _ = u["custom_attributes"].(map[string]any)
	if ca == nil || ca["favorite_food"] != "pizza" {
		t.Fatalf("user002 custom_attributes = %v, want merged favorite_food pizza", u["custom_attributes"])
	}
	hasAlias := false
	for _, a := range u["user_aliases"].([]any) {
		if a.(map[string]any)["alias_name"] == "anon-2" {
			hasAlias = true
		}
	}
	if !hasAlias {
		t.Fatalf("user002 user_aliases = %v, want merged anon-2 alias", u["user_aliases"])
	}

	// ===== users/delete hard-deletes (profile + tracked data) =====

	body, status = brazePost(t, base+"/users/delete", token, map[string]any{
		"external_ids": []string{"user002"},
	})
	if status != 200 {
		t.Fatalf("delete -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal delete: %v (body %s)", err, body)
	}
	if resp["deleted"] != float64(1) {
		t.Fatalf("deleted = %v, want 1", resp["deleted"])
	}
	body, _ = brazePost(t, base+"/users/export/ids", token, map[string]any{
		"external_ids": []string{"user002"},
	})
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal post-delete export: %v (body %s)", err, body)
	}
	if invalid, ok := resp["invalid_user_ids"].([]any); !ok || len(invalid) != 1 || invalid[0] != "user002" {
		t.Fatalf("post-delete invalid_user_ids = %v, want [user002]", resp["invalid_user_ids"])
	}
	body, status = brazePost(t, base+"/users/delete", token, map[string]any{
		"external_ids": []string{"user002"},
	})
	if status != 200 {
		t.Fatalf("delete again -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal delete again: %v (body %s)", err, body)
	}
	if resp["deleted"] != float64(0) {
		t.Fatalf("deleted (repeat) = %v, want 0", resp["deleted"])
	}

	// ===== messages/send: success with a real 32-hex dispatch id =====

	body, status = brazePost(t, base+"/messages/send", token, map[string]any{
		"messages": map[string]any{
			"email": map[string]any{
				"app_id":  "app-001",
				"subject": "Hello from Braze!",
				"from":    "noreply@example.com",
				"body":    "This is a test message.",
			},
		},
		"external_user_ids": []string{"user001"},
		"send_id":           "send-001",
	})
	if status != 200 {
		t.Fatalf("send -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal send: %v (body %s)", err, body)
	}
	if resp["message"] != "success" {
		t.Fatalf("send message = %v, want success", resp["message"])
	}
	dispatchID, ok := resp["dispatch_id"].(string)
	if !ok || len(dispatchID) != 32 {
		t.Fatalf("dispatch_id = %v, want 32-char hex (real Braze dispatch id)", resp["dispatch_id"])
	}
	for _, ch := range dispatchID {
		if !strings.ContainsRune("0123456789abcdef", ch) {
			t.Fatalf("dispatch_id %q is not lowercase hex", dispatchID)
		}
	}
	if resp["send_id"] != "send-001" {
		t.Fatalf("send_id = %v, want echo of send-001", resp["send_id"])
	}
	if _, present := resp["recipients"]; present {
		t.Fatalf("send response must not carry a recipients key (not in the real envelope): %v", resp)
	}

	// ===== messages/send: Braze fatal-error vocabulary =====

	cases := []struct {
		name    string
		payload map[string]any
		wantMsg string
	}{
		{"no messages", map[string]any{"external_user_ids": []string{"user001"}}, "No message to send"},
		{"unknown campaign", map[string]any{
			"campaign_id": "cmp999", "external_user_ids": []string{"user001"},
			"messages": map[string]any{"email": map[string]any{"app_id": "app-001", "subject": "x"}}},
			"Invalid Campaign ID"},
		{"variant unspecified", map[string]any{
			"campaign_id": "cmp001", "external_user_ids": []string{"user001"},
			"messages": map[string]any{"email": map[string]any{"app_id": "app-001", "subject": "x"}}},
			"Message Variant Unspecified"},
		{"invalid variant", map[string]any{
			"campaign_id": "cmp001", "external_user_ids": []string{"user001"},
			"messages": map[string]any{"email": map[string]any{"app_id": "app-001", "subject": "x",
				"message_variation_id": "variant-9"}}},
			"Invalid Message Variant"},
		{"no recipients", map[string]any{
			"messages": map[string]any{"email": map[string]any{"app_id": "app-001", "subject": "x"}}},
			"No Recipients"},
		{"too many recipients", map[string]any{
			"external_user_ids": tooMany,
			"messages":          map[string]any{"email": map[string]any{"app_id": "app-001", "subject": "x"}}},
			"The max number of external_ids and aliases per request was exceeded"},
	}
	for _, tc := range cases {
		body, status = brazePost(t, base+"/messages/send", token, tc.payload)
		if status != 400 {
			t.Fatalf("send %s -> status %d, want 400; body %s", tc.name, status, body)
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("unmarshal send %s: %v (body %s)", tc.name, err, body)
		}
		if resp["message"] != tc.wantMsg {
			t.Fatalf("send %s message = %v, want %q", tc.name, resp["message"], tc.wantMsg)
		}
		if _, ok := resp["errors"].([]any); !ok {
			t.Fatalf("send %s errors missing: %v", tc.name, resp)
		}
	}

	// A valid message variation passes campaign validation.
	body, status = brazePost(t, base+"/messages/send", token, map[string]any{
		"campaign_id":       "cmp001",
		"external_user_ids": []string{"user001"},
		"messages": map[string]any{"email": map[string]any{"app_id": "app-001", "subject": "x",
			"message_variation_id": "variant-1"}},
	})
	if status != 200 {
		t.Fatalf("send valid variant -> status %d; body %s", status, body)
	}

	// ===== campaigns/trigger/send =====

	body, status = brazePost(t, base+"/campaigns/trigger/send", token, map[string]any{
		"campaign_id":       "cmp001",
		"external_user_ids": []string{"user001"},
	})
	if status != 200 {
		t.Fatalf("trigger -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal trigger: %v (body %s)", err, body)
	}
	if resp["message"] != "success" {
		t.Fatalf("trigger message = %v, want success", resp["message"])
	}
	if d, ok := resp["dispatch_id"].(string); !ok || len(d) != 32 {
		t.Fatalf("trigger dispatch_id = %v, want 32-char hex", resp["dispatch_id"])
	}

	body, status = brazePost(t, base+"/campaigns/trigger/send", token, map[string]any{
		"external_user_ids": []string{"user001"},
	})
	if status != 400 {
		t.Fatalf("trigger no campaign_id -> status %d, want 400; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal trigger no campaign: %v (body %s)", err, body)
	}
	if resp["message"] != "Invalid Campaign ID" {
		t.Fatalf("trigger no campaign_id message = %v, want Invalid Campaign ID", resp["message"])
	}

	body, status = brazePost(t, base+"/campaigns/trigger/send", token, map[string]any{
		"campaign_id": "cmp404",
	})
	if status != 400 {
		t.Fatalf("trigger unknown campaign -> status %d, want 400; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal trigger unknown: %v (body %s)", err, body)
	}
	if resp["message"] != "Invalid Campaign ID" {
		t.Fatalf("trigger unknown campaign message = %v, want Invalid Campaign ID", resp["message"])
	}

	body, status = brazePost(t, base+"/campaigns/trigger/send", token, map[string]any{
		"campaign_id": "cmp001",
	})
	if status != 400 {
		t.Fatalf("trigger no recipients -> status %d, want 400; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal trigger no recipients: %v (body %s)", err, body)
	}
	if resp["message"] != "No Recipients" {
		t.Fatalf("trigger no recipients message = %v, want No Recipients", resp["message"])
	}

	// ===== segments/list → segments =====

	body, status = brazeGet(t, base+"/segments/list", token)
	if status != 200 {
		t.Fatalf("segments -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal segments: %v (body %s)", err, body)
	}
	segments, ok := resp["segments"].([]any)
	if !ok || len(segments) < 1 {
		t.Fatalf("segments = %v, want non-empty array", resp["segments"])
	}
	seg := segments[0].(map[string]any)
	if _, ok := seg["id"].(string); !ok {
		t.Fatalf("segment id = %v, want string", seg["id"])
	}
	if _, ok := seg["name"].(string); !ok {
		t.Fatalf("segment name = %v, want string", seg["name"])
	}

	// ===== x-authorization header also works =====

	req, err := http.NewRequest("GET", base+"/segments/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-authorization", token)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("segments with x-authorization -> status %d, want 200", resp2.StatusCode)
	}
}

// TestBrazeStyleScheduledLifecycle pins the derive-on-read scheduled-message
// lifecycle: a schedule created for now+1s is listed as an upcoming broadcast
// (real envelope), then — once the clock passes the scheduled time — the
// first read marks it sent, records the dispatch, and emits the unsigned
// message.sent webhook exactly once, removing it from the upcoming list.
func TestBrazeStyleScheduledLifecycle(t *testing.T) {
	var mu sync.Mutex
	var deliveries []map[string]any
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var ev map[string]any
		if err := json.Unmarshal(b, &ev); err == nil {
			mu.Lock()
			deliveries = append(deliveries, ev)
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer sink.Close()

	adapterDir := filepath.Join("..", "..", "adapters", "braze-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"braze": {Adapter: absAdapterDir},
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

	base := addrs["braze"]
	token := "test-app-group-api-key"

	// Register the outbound webhook target (local extension endpoint).
	body, status := brazePost(t, base+"/webhooks", token, map[string]any{
		"url":    sink.URL,
		"events": []string{"message.sent"},
	})
	if status != 200 {
		t.Fatalf("webhooks -> status %d; body %s", status, body)
	}

	// Invalid schedule time -> documented fatal error.
	body, status = brazePost(t, base+"/messages/schedule/create", token, map[string]any{
		"external_user_ids": []string{"user001"},
		"broadcast":         false,
		"schedule":          map[string]any{"time": "next tuesday"},
		"messages":          map[string]any{"email": map[string]any{"app_id": "app-001", "subject": "x"}},
	})
	if status != 400 {
		t.Fatalf("schedule/create bad time -> status %d, want 400; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal schedule bad time: %v (body %s)", err, body)
	}
	if resp["message"] != "Bad Request" {
		t.Fatalf("schedule bad time message = %v, want Bad Request", resp["message"])
	}

	// Unknown campaign -> Invalid Campaign ID.
	body, status = brazePost(t, base+"/messages/schedule/create", token, map[string]any{
		"campaign_id":       "cmp404",
		"external_user_ids": []string{"user001"},
		"schedule":          map[string]any{"time": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
	})
	if status != 400 {
		t.Fatalf("schedule/create unknown campaign -> status %d, want 400; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal schedule unknown campaign: %v (body %s)", err, body)
	}
	if resp["message"] != "Invalid Campaign ID" {
		t.Fatalf("schedule unknown campaign message = %v, want Invalid Campaign ID", resp["message"])
	}

	// Schedule a real send 1s out.
	scheduleTime := time.Now().Add(1 * time.Second).UTC().Format(time.RFC3339)
	body, status = brazePost(t, base+"/messages/schedule/create", token, map[string]any{
		"campaign_id":       "cmp001",
		"external_user_ids": []string{"user001"},
		"schedule":          map[string]any{"time": scheduleTime, "in_local_time": true},
	})
	if status != 200 {
		t.Fatalf("schedule/create -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal schedule/create: %v (body %s)", err, body)
	}
	if resp["message"] != "success" {
		t.Fatalf("schedule/create message = %v, want success", resp["message"])
	}
	dispatchID, _ := resp["dispatch_id"].(string)
	scheduleID, _ := resp["schedule_id"].(string)
	if len(dispatchID) != 32 {
		t.Fatalf("schedule dispatch_id = %v, want 32-char hex", resp["dispatch_id"])
	}
	if len(scheduleID) != 36 {
		t.Fatalf("schedule_id = %v, want UUID-shaped (8-4-4-4-12)", resp["schedule_id"])
	}

	// end_time is required and must parse.
	q := url.Values{}
	body, status = brazeGet(t, base+"/messages/scheduled?"+q.Encode(), token)
	if status != 400 {
		t.Fatalf("scheduled without end_time -> status %d, want 400; body %s", status, body)
	}

	// Before the send time the schedule is an upcoming broadcast (real
	// envelope: name/id/type/tags/next_send_time/schedule_type).
	endTime := url.Values{}
	endTime.Set("end_time", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	body, status = brazeGet(t, base+"/messages/scheduled?"+endTime.Encode(), token)
	if status != 200 {
		t.Fatalf("scheduled -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal scheduled: %v (body %s)", err, body)
	}
	broadcasts, ok := resp["scheduled_broadcasts"].([]any)
	if !ok || len(broadcasts) != 1 {
		t.Fatalf("scheduled_broadcasts = %v, want exactly the 1 upcoming schedule", resp["scheduled_broadcasts"])
	}
	bc := broadcasts[0].(map[string]any)
	if bc["id"] != "cmp001" || bc["type"] != "Campaign" {
		t.Fatalf("broadcast = %v, want id cmp001 type Campaign", bc)
	}
	if bc["next_send_time"] != scheduleTime {
		t.Fatalf("next_send_time = %v, want %v", bc["next_send_time"], scheduleTime)
	}
	if bc["schedule_type"] != "local_time_zones" {
		t.Fatalf("schedule_type = %v, want local_time_zones", bc["schedule_type"])
	}
	if _, ok := bc["tags"].([]any); !ok {
		t.Fatalf("tags = %v, want array", bc["tags"])
	}

	// After the send time, reads derive scheduled -> sent: the broadcast
	// leaves the upcoming list and the webhook fires exactly once.
	time.Sleep(2200 * time.Millisecond)
	for i := 0; i < 3; i++ {
		body, status = brazeGet(t, base+"/messages/scheduled?"+endTime.Encode(), token)
		if status != 200 {
			t.Fatalf("scheduled (post-send read %d) -> status %d; body %s", i, status, body)
		}
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal scheduled post-send: %v (body %s)", err, body)
	}
	if broadcasts, ok := resp["scheduled_broadcasts"].([]any); !ok || len(broadcasts) != 0 {
		t.Fatalf("post-send scheduled_broadcasts = %v, want empty (sent, not upcoming)", resp["scheduled_broadcasts"])
	}

	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	sent := 0
	for _, ev := range deliveries {
		if ev["type"] != "message.sent" {
			t.Errorf("unexpected webhook type %v", ev["type"])
			continue
		}
		payload, _ := ev["payload"].(map[string]any)
		if payload["dispatch_id"] != dispatchID {
			t.Errorf("message.sent dispatch_id = %v, want %v", payload["dispatch_id"], dispatchID)
		}
		if payload["campaign_id"] != "cmp001" {
			t.Errorf("message.sent campaign_id = %v, want cmp001", payload["campaign_id"])
		}
		sent++
	}
	if sent != 1 {
		t.Errorf("message.sent emitted %d times, want exactly 1 (derive-on-read persists before emit)", sent)
	}
}

// === Braze test helpers ===

func brazeGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
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

func brazePost(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	return brazePostRaw(t, rawurl, token, string(data))
}

// brazePostRaw sends an arbitrary (possibly malformed) JSON body.
func brazePostRaw(t *testing.T, rawurl, token, body string) (string, int) {
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
