package conformance

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// TestCalendarConformance drives Google's own generated client
// (google.golang.org/api/calendar/v3) against the gcalendar-style adapter:
// option.WithEndpoint points the SDK at the booted engine while its
// serialization, query-param plumbing and googleapi error decoding all
// stay stock.
func TestCalendarConformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "gcalendar-style")

	svc, err := calendar.NewService(ctx,
		// Calendar's basePath carries the version segment (unlike gmail's
		// host-rooted one), so the endpoint must include /calendar/v3/ —
		// the standard way to point this SDK at a proxy.
		option.WithEndpoint(base+"/calendar/v3/"),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "conformance-token"})),
	)
	if err != nil {
		t.Fatalf("calendar.NewService: %v", err)
	}

	// ===== Calendars.Get resolves the primary calendar =====
	prim, err := svc.Calendars.Get("primary").Do()
	if err != nil {
		t.Fatalf("calendars.get: %v", err)
	}
	if prim.Kind != "calendar#calendar" || prim.Id == "" || prim.TimeZone == "" {
		t.Errorf("calendars.get: kind=%q id=%q timeZone=%q", prim.Kind, prim.Id, prim.TimeZone)
	}
	if prim.Etag == "" || !strings.HasPrefix(prim.Etag, `"`) {
		t.Errorf("calendars.get: etag=%q, want a quoted content tag", prim.Etag)
	}
	Record(t, "google-api-go-client", "gcalendar-style", "Calendars.Get resolves the primary calendar")

	// ===== CalendarList.List returns the seeded primary entry =====
	calList, err := svc.CalendarList.List().Do()
	if err != nil {
		t.Fatalf("calendarList.list: %v", err)
	}
	var entry *calendar.CalendarListEntry
	for _, e := range calList.Items {
		if e.Primary {
			entry = e
		}
	}
	if entry == nil {
		t.Fatalf("calendarList.list: no primary entry in %d items", len(calList.Items))
	}
	if entry.Kind != "calendar#calendarListEntry" || entry.AccessRole != "owner" || entry.Id != prim.Id {
		t.Errorf("calendarList.list: kind=%q accessRole=%q id=%q", entry.Kind, entry.AccessRole, entry.Id)
	}
	Record(t, "google-api-go-client", "gcalendar-style", "CalendarList.List returns the seeded primary entry")

	// ===== Events.Insert + Events.Get round-trip the full event body =====
	created, err := svc.Events.Insert("primary", &calendar.Event{
		Summary:     "SDK conformance standup",
		Description: "Inserted through google-api-go-client",
		Location:    "https://meet.google.com/conformance",
		Start:       &calendar.EventDateTime{DateTime: "2026-09-01T15:00:00Z", TimeZone: "America/New_York"},
		End:         &calendar.EventDateTime{DateTime: "2026-09-01T15:30:00Z", TimeZone: "America/New_York"},
		Attendees:   []*calendar.EventAttendee{{Email: "alice@example.com"}},
	}).Do()
	if err != nil {
		t.Fatalf("events.insert: %v", err)
	}
	if created.Id == "" || created.Status != "confirmed" || created.Sequence != 0 {
		t.Fatalf("events.insert: id=%q status=%q sequence=%d", created.Id, created.Status, created.Sequence)
	}
	if !strings.HasSuffix(created.ICalUID, "@google.com") || !strings.Contains(created.HtmlLink, created.Id) {
		t.Errorf("events.insert: iCalUID=%q htmlLink=%q", created.ICalUID, created.HtmlLink)
	}
	got, err := svc.Events.Get("primary", created.Id).Do()
	if err != nil {
		t.Fatalf("events.get: %v", err)
	}
	if got.Kind != "calendar#event" || got.Summary != "SDK conformance standup" ||
		got.Start.DateTime != "2026-09-01T15:00:00Z" || got.Start.TimeZone != "America/New_York" ||
		got.End.DateTime != "2026-09-01T15:30:00Z" {
		t.Errorf("events.get: kind=%q summary=%q start=%+v end=%+v", got.Kind, got.Summary, got.Start, got.End)
	}
	if len(got.Attendees) != 1 || got.Attendees[0].Email != "alice@example.com" {
		t.Errorf("events.get: attendees=%+v", got.Attendees)
	}
	if got.Creator == nil || got.Creator.Email != "mock-user@gmail.com" {
		t.Errorf("events.get: creator=%+v", got.Creator)
	}
	Record(t, "google-api-go-client", "gcalendar-style", "Events.Insert + Events.Get round-trip the event body")

	// ===== Events.List Q filters by summary/description text =====
	review, err := svc.Events.Insert("primary", &calendar.Event{
		Summary:     "Design review",
		Description: "Quarterly widget roadmap",
		Start:       &calendar.EventDateTime{DateTime: "2026-09-02T17:00:00Z"},
		End:         &calendar.EventDateTime{DateTime: "2026-09-02T18:00:00Z"},
	}).Do()
	if err != nil {
		t.Fatalf("events.insert review: %v", err)
	}
	lunch, err := svc.Events.Insert("primary", &calendar.Event{
		Summary:     "Team lunch",
		Description: "Sandwiches downtown",
		Start:       &calendar.EventDateTime{DateTime: "2026-09-03T12:00:00Z"},
		End:         &calendar.EventDateTime{DateTime: "2026-09-03T13:00:00Z"},
	}).Do()
	if err != nil {
		t.Fatalf("events.insert lunch: %v", err)
	}
	hits, err := svc.Events.List("primary").Q("widget").Do()
	if err != nil {
		t.Fatalf("events.list q=widget: %v", err)
	}
	if len(hits.Items) != 1 || hits.Items[0].Id != review.Id {
		ids := []string{}
		for _, e := range hits.Items {
			ids = append(ids, e.Id)
		}
		t.Errorf("events.list q=widget: want [%s], got %v", review.Id, ids)
	}
	Record(t, "google-api-go-client", "gcalendar-style", "Events.List Q matches summary/description text")

	// ===== Events.List MaxResults=1 walks pages via PageToken =====
	page1, err := svc.Events.List("primary").MaxResults(1).Do()
	if err != nil {
		t.Fatalf("events.list page1: %v", err)
	}
	if len(page1.Items) != 1 || page1.NextPageToken == "" {
		t.Fatalf("events.list page1: len=%d nextPageToken=%q", len(page1.Items), page1.NextPageToken)
	}
	page2, err := svc.Events.List("primary").MaxResults(1).PageToken(page1.NextPageToken).Do()
	if err != nil {
		t.Fatalf("events.list page2: %v", err)
	}
	if len(page2.Items) != 1 || page2.Items[0].Id == "" || page2.Items[0].Id == page1.Items[0].Id {
		t.Errorf("events.list page2: same page repeated (%v vs %v)", page1.Items, page2.Items)
	}
	Record(t, "google-api-go-client", "gcalendar-style", "Events.List MaxResults + PageToken walk pages")

	// ===== Events.Patch updates fields and bumps sequence + etag =====
	patched, err := svc.Events.Patch("primary", created.Id, &calendar.Event{
		Summary:  "SDK conformance standup (moved)",
		Location: "https://meet.google.com/conformance-2",
	}).Do()
	if err != nil {
		t.Fatalf("events.patch: %v", err)
	}
	if patched.Summary != "SDK conformance standup (moved)" || patched.Location != "https://meet.google.com/conformance-2" {
		t.Errorf("events.patch: summary=%q location=%q", patched.Summary, patched.Location)
	}
	if patched.Sequence != 1 {
		t.Errorf("events.patch: sequence=%d, want 1", patched.Sequence)
	}
	if patched.Etag == created.Etag {
		t.Errorf("events.patch: etag unchanged after content change (%q)", patched.Etag)
	}
	if after, err := svc.Events.Get("primary", created.Id).Do(); err != nil || after == nil || after.Summary != patched.Summary {
		t.Errorf("events.get after patch: event=%v err=%v", after, err)
	}
	Record(t, "google-api-go-client", "gcalendar-style", "Events.Patch updates fields and bumps sequence + etag")

	// ===== Events.Instances expands an RRULE into per-occurrence events =====
	recurring, err := svc.Events.Insert("primary", &calendar.Event{
		Summary:    "Daily conformance drill",
		Start:      &calendar.EventDateTime{DateTime: "2026-09-07T09:00:00Z"},
		End:        &calendar.EventDateTime{DateTime: "2026-09-07T09:30:00Z"},
		Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=3"},
	}).Do()
	if err != nil {
		t.Fatalf("events.insert recurring: %v", err)
	}
	inst, err := svc.Events.Instances("primary", recurring.Id).Do()
	if err != nil {
		t.Fatalf("events.instances: %v", err)
	}
	if inst.Kind != "calendar#events" || len(inst.Items) != 3 {
		t.Fatalf("events.instances: kind=%q len=%d, want 3 instances", inst.Kind, len(inst.Items))
	}
	for i, e := range inst.Items {
		if e.RecurringEventId != recurring.Id || e.Id != recurring.Id+"_"+strconv.Itoa(i) {
			t.Errorf("events.instances[%d]: id=%q recurringEventId=%q", i, e.Id, e.RecurringEventId)
		}
		if want := "2026-09-0" + strconv.Itoa(7+i) + "T09:00:00Z"; e.Start.DateTime != want {
			t.Errorf("events.instances[%d]: start=%q, want %q", i, e.Start.DateTime, want)
		}
	}
	Record(t, "google-api-go-client", "gcalendar-style", "Events.Instances expands an RRULE into per-occurrence events")

	// ===== Deleted events surface as decoded googleapi 404s =====
	if err := svc.Events.Delete("primary", lunch.Id).Do(); err != nil {
		t.Fatalf("events.delete: %v", err)
	}
	_, err = svc.Events.Get("primary", lunch.Id).Do()
	if gErr, ok := err.(*googleapi.Error); !ok || gErr.Code != 404 {
		t.Errorf("events.get after delete: want googleapi 404, got %v", err)
	} else if !strings.Contains(gErr.Message, "Event not found") {
		t.Errorf("events.get after delete: message=%q", gErr.Message)
	}
	Record(t, "google-api-go-client", "gcalendar-style", "Events.Delete then Get -> googleapi 404")
}
