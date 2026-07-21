package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestGCalendarStyleAdapter exercises the gcalendar-style adapter:
//
//   - List calendars (seeded primary)
//   - Create event → appears in list (timeMin/timeMax)
//   - GET single event
//   - DELETE event → 204
//   - 401 without bearer
func TestGCalendarStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "gcalendar-style")
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
			"cal": {Adapter: absAdapterDir},
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

	base := addrs["cal"]
	token := "mock-oauth2-token"

	// ===== Get primary calendar =====

	body, status := gcalGet(t, base+"/calendar/v3/calendars/primary", token)
	if status != 200 {
		t.Fatalf("get primary calendar -> status %d, want 200; body %s", status, body)
	}
	var cal map[string]any
	if err := json.Unmarshal([]byte(body), &cal); err != nil {
		t.Fatalf("unmarshal primary calendar: %v (body %s)", err, body)
	}
	calID, ok := cal["id"].(string)
	if !ok || calID == "" {
		t.Fatalf("id = %v, want non-empty string", cal["id"])
	}

	// ===== List calendars =====

	body, status = gcalGet(t, base+"/calendar/v3/users/me/calendarList", token)
	if status != 200 {
		t.Fatalf("list calendars -> status %d, want 200; body %s", status, body)
	}
	var calList map[string]any
	if err := json.Unmarshal([]byte(body), &calList); err != nil {
		t.Fatalf("unmarshal calendar list: %v (body %s)", err, body)
	}
	items, ok := calList["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("items = %v, want non-empty list", calList["items"])
	}

	// ===== Create event =====

	eventBody := map[string]any{
		"summary": "Project Review",
		"start":   map[string]any{"dateTime": "2025-06-15T10:00:00Z"},
		"end":     map[string]any{"dateTime": "2025-06-15T11:00:00Z"},
		"attendees": []map[string]any{
			{"email": "alice@example.com"},
			{"email": "bob@example.com"},
		},
	}
	body, status = gcalPostJSON(t, base+"/calendar/v3/calendars/"+calID+"/events", token, eventBody)
	if status != 200 {
		t.Fatalf("create event -> status %d, want 200; body %s", status, body)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("unmarshal created event: %v (body %s)", err, body)
	}
	eventID, ok := created["id"].(string)
	if !ok || eventID == "" {
		t.Fatalf("event id = %v, want non-empty string", created["id"])
	}
	if created["summary"] != "Project Review" {
		t.Fatalf("summary = %v, want 'Project Review'", created["summary"])
	}
	_, ok = created["iCalUID"].(string)
	if !ok {
		t.Fatalf("iCalUID = %v, want string", created["iCalUID"])
	}
	attendees, ok := created["attendees"].([]any)
	if !ok || len(attendees) != 2 {
		t.Fatalf("attendees = %v, want 2", created["attendees"])
	}

	// ===== List events (with timeMin) =====

	body, status = gcalGet(t, base+"/calendar/v3/calendars/"+calID+"/events?timeMin=2025-06-01T00:00:00Z&timeMax=2025-06-30T23:59:59Z", token)
	if status != 200 {
		t.Fatalf("list events -> status %d, want 200; body %s", status, body)
	}
	var eventList map[string]any
	if err := json.Unmarshal([]byte(body), &eventList); err != nil {
		t.Fatalf("unmarshal event list: %v (body %s)", err, body)
	}
	events, ok := eventList["items"].([]any)
	if !ok || len(events) == 0 {
		t.Fatalf("events = %v, want non-empty list", eventList["items"])
	}

	// Find the created event in the list.
	found := false
	for _, e := range events {
		em := e.(map[string]any)
		if em["id"] == eventID {
			found = true
		}
	}
	if !found {
		t.Fatalf("created event %q not found in listing", eventID)
	}

	// ===== GET single event =====

	body, status = gcalGet(t, base+"/calendar/v3/calendars/"+calID+"/events/"+eventID, token)
	if status != 200 {
		t.Fatalf("get event -> status %d, want 200; body %s", status, body)
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(body), &ev); err != nil {
		t.Fatalf("unmarshal event: %v (body %s)", err, body)
	}
	if ev["id"] != eventID {
		t.Fatalf("event id = %v, want %s", ev["id"], eventID)
	}

	// ===== DELETE event =====

	body, status = gcalDo(t, "DELETE", base+"/calendar/v3/calendars/"+calID+"/events/"+eventID, token, nil)
	if status != 204 {
		t.Fatalf("delete event -> status %d, want 204; body %s", status, body)
	}

	// Verify deleted: GET should 404.
	body, status = gcalGet(t, base+"/calendar/v3/calendars/"+calID+"/events/"+eventID, token)
	if status != 404 {
		t.Fatalf("get deleted event -> status %d, want 404; body %s", status, body)
	}

	// ===== 401 without bearer =====

	body, status = gcalGet(t, base+"/calendar/v3/calendars/"+calID+"/events", "")
	if status != 401 {
		t.Fatalf("list events without token -> status %d, want 401; body %s", status, body)
	}
	if !strings.Contains(body, "UNAUTHENTICATED") {
		t.Fatalf("error body should contain UNAUTHENTICATED: %s", body)
	}
}

// === Helpers ===

func gcalGet(t *testing.T, url, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
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

func gcalPostJSON(t *testing.T, url, token string, body map[string]any) (string, int) {
	t.Helper()
	return gcalDo(t, "POST", url, token, body)
}

func gcalDo(t *testing.T, method, url, token string, body map[string]any) (string, int) {
	t.Helper()
	var req *http.Request
	var err error
	if body != nil {
		data, _ := json.Marshal(body)
		req, err = http.NewRequest(method, url, bytes.NewReader(data))
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
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
