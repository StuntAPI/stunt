package conformance

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/docs/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// TestDocsConformance drives Google's generated Docs client
// (google.golang.org/api/docs/v1) against the gdocs-style adapter: the
// structural document model, ordered batchUpdate application, and googleapi
// error decoding all run through the stock SDK.
func TestDocsConformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "gdocs-style")

	svc, err := docs.NewService(ctx,
		option.WithEndpoint(base),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "conformance-token"})),
	)
	if err != nil {
		t.Fatalf("docs.NewService: %v", err)
	}

	// ===== Documents.Create mints a titled document =====
	created, err := svc.Documents.Create(&docs.Document{Title: "SDK Conformance Doc"}).Do()
	if err != nil {
		t.Fatalf("documents.create: %v", err)
	}
	if created.DocumentId == "" || created.Title != "SDK Conformance Doc" {
		t.Fatalf("documents.create: id=%q title=%q", created.DocumentId, created.Title)
	}
	Record(t, "google-api-go-client", "gdocs-style", "Documents.Create returns documentId + title")

	// ===== Documents.Get round-trips the structural model =====
	got, err := svc.Documents.Get(created.DocumentId).Do()
	if err != nil {
		t.Fatalf("documents.get: %v", err)
	}
	if got.Body == nil || len(got.Body.Content) == 0 {
		t.Fatalf("documents.get: no body content")
	}
	Record(t, "google-api-go-client", "gdocs-style", "Documents.Get returns the structural body model")

	// ===== batchUpdate insertText lands the text and echoes a reply =====
	upd, err := svc.Documents.BatchUpdate(created.DocumentId, &docs.BatchUpdateDocumentRequest{
		Requests: []*docs.Request{{
			InsertText: &docs.InsertTextRequest{
				Text:     "inserted by the SDK",
				Location: &docs.Location{Index: 1},
			},
		}},
	}).Do()
	if err != nil {
		t.Fatalf("documents.batchUpdate insertText: %v", err)
	}
	if len(upd.Replies) != 1 {
		t.Errorf("documents.batchUpdate: %d replies, want 1", len(upd.Replies))
	}
	after, err := svc.Documents.Get(created.DocumentId).Do()
	if err != nil {
		t.Fatalf("documents.get after insert: %v", err)
	}
	if !strings.Contains(docText(after), "inserted by the SDK") {
		t.Errorf("documents.get after insert: text missing (content=%s)", docText(after))
	}
	Record(t, "google-api-go-client", "gdocs-style", "batchUpdate insertText applies and replies 1:1")

	// ===== Ordered requests: two inserts apply in sequence =====
	if _, err := svc.Documents.BatchUpdate(created.DocumentId, &docs.BatchUpdateDocumentRequest{
		Requests: []*docs.Request{
			{InsertText: &docs.InsertTextRequest{Text: "second.", Location: &docs.Location{Index: 1}}},
			{InsertText: &docs.InsertTextRequest{Text: "First. ", Location: &docs.Location{Index: 1}}},
		},
	}).Do(); err != nil {
		t.Fatalf("documents.batchUpdate ordered: %v", err)
	}
	final := docText(mustGetDoc(t, svc, created.DocumentId))
	if !strings.HasPrefix(final, "First. second.") {
		t.Errorf("documents.get final: prefix=%q", firstN(final, 40))
	}
	Record(t, "google-api-go-client", "gdocs-style", "batchUpdate applies ordered requests at indexes")

	// ===== Unknown document decodes as googleapi 404 =====
	_, err = svc.Documents.Get("sdk-missing-document").Do()
	if gErr, ok := err.(*googleapi.Error); !ok || gErr.Code != 404 {
		t.Errorf("documents.get unknown: want googleapi 404, got %v", err)
	}
	Record(t, "google-api-go-client", "gdocs-style", "Unknown document Get -> googleapi 404")
}

func mustGetDoc(t *testing.T, svc *docs.Service, id string) *docs.Document {
	t.Helper()
	d, err := svc.Documents.Get(id).Do()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// docText flattens the structural body into its run text.
func docText(d *docs.Document) string {
	var b strings.Builder
	if d.Body == nil {
		return ""
	}
	for _, el := range d.Body.Content {
		if el.Paragraph == nil {
			continue
		}
		for _, pe := range el.Paragraph.Elements {
			if pe.TextRun != nil {
				b.WriteString(pe.TextRun.Content)
			}
		}
	}
	return b.String()
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
