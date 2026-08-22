package conformance

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// TestGmailConformance drives Google's own generated client
// (google.golang.org/api/gmail/v1) against the gmail-style adapter:
// option.WithEndpoint points the SDK at the booted engine while its
// serialization, query-param plumbing and googleapi error decoding all
// stay stock.
func TestGmailConformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "gmail-style")

	svc, err := gmail.NewService(ctx,
		option.WithEndpoint(base),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "conformance-token"})),
	)
	if err != nil {
		t.Fatalf("gmail.NewService: %v", err)
	}

	// ===== Labels.List returns the seeded system labels =====
	labels, err := svc.Users.Labels.List("me").Do()
	if err != nil {
		t.Fatalf("labels.list: %v", err)
	}
	found := map[string]bool{}
	for _, l := range labels.Labels {
		found[l.Id] = true
	}
	if !found["INBOX"] || !found["SENT"] {
		t.Errorf("labels.list: want seeded INBOX+SENT, got %v", found)
	}
	Record(t, "google-api-go-client", "gmail-style", "Labels.List returns seeded system labels")

	// ===== Labels.Create mints a user label that List then shows =====
	created, err := svc.Users.Labels.Create("me", &gmail.Label{Name: "sdk-conformance"}).Do()
	if err != nil {
		t.Fatalf("labels.create: %v", err)
	}
	if created.Id == "" || created.Type != "user" {
		t.Errorf("labels.create: got id=%q type=%q", created.Id, created.Type)
	}
	after, _ := svc.Users.Labels.List("me").Do()
	seen := false
	for _, l := range after.Labels {
		if l.Id == created.Id {
			seen = true
		}
	}
	if !seen {
		t.Errorf("labels.list: created label %q not listed", created.Id)
	}
	Record(t, "google-api-go-client", "gmail-style", "Labels.Create + Labels.List round-trip")

	// ===== Messages.Send takes a raw rfc822 and returns id + threadId =====
	raw := base64.RawURLEncoding.EncodeToString([]byte(
		"From: conformance@example.com\r\nTo: mock-user@gmail.com\r\nSubject: Sent by the SDK\r\n\r\nbody from google-api-go-client\r\n"))
	sent, err := svc.Users.Messages.Send("me", &gmail.Message{Raw: raw}).Do()
	if err != nil {
		t.Fatalf("messages.send: %v", err)
	}
	if sent.Id == "" || sent.ThreadId == "" {
		t.Fatalf("messages.send: got id=%q threadId=%q", sent.Id, sent.ThreadId)
	}
	Record(t, "google-api-go-client", "gmail-style", "Messages.Send raw rfc822 -> id + threadId")

	// ===== Messages.Get round-trips headers from the raw message =====
	got, err := svc.Users.Messages.Get("me", sent.Id).Do()
	if err != nil {
		t.Fatalf("messages.get: %v", err)
	}
	hdrs := map[string]string{}
	for _, h := range got.Payload.Headers {
		hdrs[h.Name] = h.Value
	}
	if !strings.Contains(hdrs["Subject"], "Sent by the SDK") {
		t.Errorf("messages.get: subject header = %q", hdrs["Subject"])
	}
	Record(t, "google-api-go-client", "gmail-style", "Messages.Get parses the sent raw message into payload headers")

	// ===== Messages.List includes the sent message =====
	lst, err := svc.Users.Messages.List("me").Do()
	if err != nil {
		t.Fatalf("messages.list: %v", err)
	}
	listed := false
	for _, m := range lst.Messages {
		if m.Id == sent.Id {
			listed = true
		}
	}
	if !listed {
		t.Errorf("messages.list: sent message %q missing", sent.Id)
	}
	Record(t, "google-api-go-client", "gmail-style", "Messages.List includes sent messages")

	// ===== MaxResults=1 paginates through pageToken =====
	page1, err := svc.Users.Messages.List("me").MaxResults(1).Do()
	if err != nil {
		t.Fatalf("messages.list page1: %v", err)
	}
	if len(page1.Messages) != 1 || page1.NextPageToken == "" {
		t.Fatalf("messages.list page1: len=%d nextPageToken=%q", len(page1.Messages), page1.NextPageToken)
	}
	page2, err := svc.Users.Messages.List("me").MaxResults(1).PageToken(page1.NextPageToken).Do()
	if err != nil {
		t.Fatalf("messages.list page2: %v", err)
	}
	if len(page2.Messages) != 1 || page2.Messages[0].Id == page1.Messages[0].Id {
		t.Errorf("messages.list page2: same page repeated (%v)", page2.Messages)
	}
	Record(t, "google-api-go-client", "gmail-style", "Messages.List MaxResults + PageToken walk pages")

	// ===== Messages.Modify adds a label, Get reflects it =====
	modified, err := svc.Users.Messages.Modify("me", sent.Id, &gmail.ModifyMessageRequest{
		AddLabelIds: []string{created.Id},
	}).Do()
	if err != nil {
		t.Fatalf("messages.modify: %v", err)
	}
	has := false
	for _, id := range modified.LabelIds {
		if id == created.Id {
			has = true
		}
	}
	if !has {
		t.Errorf("messages.modify: label %q not in %v", created.Id, modified.LabelIds)
	}
	Record(t, "google-api-go-client", "gmail-style", "Messages.Modify AddLabelIds applies")

	// ===== Messages.Get format=metadata keeps headers, drops the body =====
	meta, err := svc.Users.Messages.Get("me", sent.Id).Format("metadata").Do()
	if err != nil {
		t.Fatalf("messages.get metadata: %v", err)
	}
	if len(meta.Payload.Headers) == 0 || meta.Payload.Body != nil && meta.Payload.Body.Size != 0 {
		t.Errorf("messages.get metadata: headers=%d body=%+v", len(meta.Payload.Headers), meta.Payload.Body)
	}
	Record(t, "google-api-go-client", "gmail-style", "Messages.Get Format=metadata returns headers without body")

	// ===== Deleted messages surface as decoded googleapi 404s =====
	if err := svc.Users.Messages.Delete("me", sent.Id).Do(); err != nil {
		t.Fatalf("messages.delete: %v", err)
	}
	_, err = svc.Users.Messages.Get("me", sent.Id).Do()
	if gErr, ok := err.(*googleapi.Error); !ok || gErr.Code != 404 {
		t.Errorf("messages.get after delete: want googleapi 404, got %v", err)
	}
	Record(t, "google-api-go-client", "gmail-style", "Messages.Delete then Get -> googleapi 404")
}
