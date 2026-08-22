package conformance

import (
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/tasks/v1"
)

// TestTasksConformance drives Google's own generated client
// (google.golang.org/api/tasks/v1) against the gtasks-style adapter:
// option.WithEndpoint points the SDK at the booted engine while its
// serialization, query-param plumbing and googleapi error decoding all
// stay stock.
func TestTasksConformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "gtasks-style")

	svc, err := tasks.NewService(ctx,
		option.WithEndpoint(base),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "conformance-token"})),
	)
	if err != nil {
		t.Fatalf("tasks.NewService: %v", err)
	}

	// ===== Tasklists.List returns the seeded default list =====
	lists, err := svc.Tasklists.List().Do()
	if err != nil {
		t.Fatalf("tasklists.list: %v", err)
	}
	if len(lists.Items) == 0 {
		t.Fatalf("tasklists.list: no items")
	}
	seeded := false
	for _, tl := range lists.Items {
		if tl.Title == "My Tasks" && tl.Id != "" {
			seeded = true
		}
	}
	if !seeded {
		t.Errorf("tasklists.list: seeded \"My Tasks\" list missing, got %+v", lists.Items)
	}
	Record(t, "google-api-go-client", "gtasks-style", "Tasklists.List returns the seeded default list")

	// ===== Tasklists.Insert mints a list that List then shows =====
	made, err := svc.Tasklists.Insert(&tasks.TaskList{Title: "SDK Conformance"}).Do()
	if err != nil {
		t.Fatalf("tasklists.insert: %v", err)
	}
	if made.Id == "" || made.Title != "SDK Conformance" || made.Updated == "" {
		t.Fatalf("tasklists.insert: got id=%q title=%q updated=%q", made.Id, made.Title, made.Updated)
	}
	after, _ := svc.Tasklists.List().Do()
	seen := false
	for _, tl := range after.Items {
		if tl.Id == made.Id {
			seen = true
		}
	}
	if !seen {
		t.Errorf("tasklists.list: created list %q not listed", made.Id)
	}
	Record(t, "google-api-go-client", "gtasks-style", "Tasklists.Insert + Tasklists.List round-trip")

	listID := made.Id

	// ===== Tasks.Insert creates a task that Get round-trips =====
	due := "2026-09-01T00:00:00.000Z"
	buy, err := svc.Tasks.Insert(listID, &tasks.Task{
		Title: "Buy oat milk",
		Notes: "the barista blend",
		Due:   due,
	}).Do()
	if err != nil {
		t.Fatalf("tasks.insert: %v", err)
	}
	if buy.Id == "" || buy.Status != "needsAction" || buy.Position == "" {
		t.Fatalf("tasks.insert: got id=%q status=%q position=%q", buy.Id, buy.Status, buy.Position)
	}
	if !strings.Contains(buy.SelfLink, buy.Id) {
		t.Errorf("tasks.insert: selfLink %q does not reference id %q", buy.SelfLink, buy.Id)
	}
	got, err := svc.Tasks.Get(listID, buy.Id).Do()
	if err != nil {
		t.Fatalf("tasks.get: %v", err)
	}
	if got.Title != "Buy oat milk" || got.Notes != "the barista blend" || got.Due != due || got.Status != "needsAction" {
		t.Errorf("tasks.get: got title=%q notes=%q due=%q status=%q", got.Title, got.Notes, got.Due, got.Status)
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", got.Updated); err != nil {
		t.Errorf("tasks.get: updated %q not a Google Tasks stamp: %v", got.Updated, err)
	}
	Record(t, "google-api-go-client", "gtasks-style", "Tasks.Insert + Tasks.Get round-trip fields")

	// Two more tasks so filtering and paging have something to bite on.
	ship, err := svc.Tasks.Insert(listID, &tasks.Task{Title: "Ship release notes"}).Do()
	if err != nil {
		t.Fatalf("tasks.insert(ship): %v", err)
	}
	reply, err := svc.Tasks.Insert(listID, &tasks.Task{Title: "Reply to Dana"}).Do()
	if err != nil {
		t.Fatalf("tasks.insert(reply): %v", err)
	}

	// ===== Tasks.Patch marks a task completed and stamps completed =====
	patched, err := svc.Tasks.Patch(listID, reply.Id, &tasks.Task{Status: "completed"}).Do()
	if err != nil {
		t.Fatalf("tasks.patch: %v", err)
	}
	if patched.Status != "completed" || patched.Completed == nil {
		t.Fatalf("tasks.patch: status=%q completed=%v", patched.Status, patched.Completed)
	}
	stamp, err := time.Parse("2006-01-02T15:04:05.000Z", *patched.Completed)
	if err != nil {
		t.Fatalf("tasks.patch: completed %q not a Google Tasks stamp: %v", *patched.Completed, err)
	}
	if d := time.Since(stamp); d < -time.Minute || d > 15*time.Minute {
		t.Errorf("tasks.patch: completed stamp %q is %s old, want minted now", *patched.Completed, d)
	}
	Record(t, "google-api-go-client", "gtasks-style", "Tasks.Patch status=completed derives the completed stamp")

	// ===== Tasks.List ShowCompleted=false drops completed tasks =====
	open, err := svc.Tasks.List(listID).ShowCompleted(false).Do()
	if err != nil {
		t.Fatalf("tasks.list showCompleted=false: %v", err)
	}
	for _, tk := range open.Items {
		if tk.Id == reply.Id || tk.Status != "needsAction" {
			t.Errorf("tasks.list showCompleted=false: got %q status=%q, want only needsAction tasks", tk.Id, tk.Status)
		}
	}
	if len(open.Items) != 2 {
		t.Errorf("tasks.list showCompleted=false: got %d items, want 2", len(open.Items))
	}
	Record(t, "google-api-go-client", "gtasks-style", "Tasks.List ShowCompleted=false hides completed tasks")

	// ===== Tasks.Move re-parents and repositions via query params =====
	moved, err := svc.Tasks.Move(listID, buy.Id).Parent(ship.Id).Previous(ship.Id).Do()
	if err != nil {
		t.Fatalf("tasks.move: %v", err)
	}
	if moved.Parent != ship.Id {
		t.Errorf("tasks.move: parent=%q, want %q", moved.Parent, ship.Id)
	}
	if moved.Position == got.Position || moved.Position == "" {
		t.Errorf("tasks.move: position=%q, want a new non-empty position (was %q)", moved.Position, got.Position)
	}
	reread, err := svc.Tasks.Get(listID, buy.Id).Do()
	if err != nil {
		t.Fatalf("tasks.get after move: %v", err)
	}
	if reread.Parent != ship.Id || reread.Position != moved.Position {
		t.Errorf("tasks.get after move: parent=%q position=%q, want %q/%q",
			reread.Parent, reread.Position, ship.Id, moved.Position)
	}
	Record(t, "google-api-go-client", "gtasks-style", "Tasks.Move Parent/Previous re-parents and repositions")

	// ===== Tasks.List MaxResults + PageToken walk pages =====
	page1, err := svc.Tasks.List(listID).MaxResults(1).Do()
	if err != nil {
		t.Fatalf("tasks.list page1: %v", err)
	}
	if len(page1.Items) != 1 || page1.NextPageToken == "" {
		t.Fatalf("tasks.list page1: len=%d nextPageToken=%q", len(page1.Items), page1.NextPageToken)
	}
	page2, err := svc.Tasks.List(listID).MaxResults(1).PageToken(page1.NextPageToken).Do()
	if err != nil {
		t.Fatalf("tasks.list page2: %v", err)
	}
	if len(page2.Items) != 1 || page2.Items[0].Id == page1.Items[0].Id {
		t.Errorf("tasks.list page2: same page repeated (%v / %v)", page1.Items, page2.Items)
	}
	Record(t, "google-api-go-client", "gtasks-style", "Tasks.List MaxResults + PageToken walk pages")

	// ===== Deleted tasks surface as decoded googleapi 404s =====
	if err := svc.Tasks.Delete(listID, buy.Id).Do(); err != nil {
		t.Fatalf("tasks.delete: %v", err)
	}
	_, err = svc.Tasks.Get(listID, buy.Id).Do()
	if gErr, ok := err.(*googleapi.Error); !ok || gErr.Code != 404 {
		t.Errorf("tasks.get after delete: want googleapi 404, got %v", err)
	}
	Record(t, "google-api-go-client", "gtasks-style", "Tasks.Delete then Tasks.Get -> googleapi 404")
}
