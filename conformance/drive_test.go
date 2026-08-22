package conformance

import (
	"context"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// TestDriveConformance drives Google's own generated client
// (google.golang.org/api/drive/v3) against the drive-style adapter. The
// drive client keeps /drive/v3 in its base path (gmail's carries it in
// each relative URL), so the endpoint is base+"/drive/v3/"; everything
// else — serialization, query-param plumbing, googleapi error decoding —
// stays stock. The adapter validates bearers against its token store, so
// the suite uses its documented static test token. The /upload/ endpoints
// are a documented deviation (JSON bodies, not media) and are skipped:
// files are created at the metadata level through POST /drive/v3/files.
func TestDriveConformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "drive-style")

	svc, err := drive.NewService(ctx,
		option.WithEndpoint(base+"/drive/v3/"),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "ya29.mock_test_token_drive"})),
	)
	if err != nil {
		t.Fatalf("drive.NewService: %v", err)
	}

	// Change-feed cursor captured before any mutation; the changes
	// behavior at the end replays everything recorded since here.
	cur, err := svc.Changes.GetStartPageToken().Do()
	if err != nil {
		t.Fatalf("changes.getStartPageToken: %v", err)
	}

	// ===== Files.List returns the seeded folder =====
	lst, err := svc.Files.List().Do()
	if err != nil {
		t.Fatalf("files.list: %v", err)
	}
	if len(lst.Files) == 0 {
		t.Fatal("files.list: no files")
	}
	var seed *drive.File
	for _, f := range lst.Files {
		if f.Id == "seed-folder-docs" {
			seed = f
		}
	}
	if seed == nil || seed.MimeType != "application/vnd.google-apps.folder" {
		t.Errorf("files.list: seeded folder missing or wrong mimeType (got %+v)", seed)
	}
	Record(t, "google-api-go-client", "drive-style", "Files.List returns the seeded folder")

	// ===== Files.Create mints a folder that List then shows =====
	created, err := svc.Files.Create(&drive.File{
		Name:     "sdk-conformance-folder",
		MimeType: "application/vnd.google-apps.folder",
		Parents:  []string{seed.Id},
	}).Do()
	if err != nil {
		t.Fatalf("files.create: %v", err)
	}
	if created.Id == "" || created.Name != "sdk-conformance-folder" {
		t.Errorf("files.create: got id=%q name=%q", created.Id, created.Name)
	}
	after, _ := svc.Files.List().Do()
	seen := false
	for _, f := range after.Files {
		if f.Id == created.Id {
			seen = true
		}
	}
	if !seen {
		t.Errorf("files.list: created folder %q not listed", created.Id)
	}
	Record(t, "google-api-go-client", "drive-style", "Files.Create + Files.List round-trip")

	// ===== Files.Get round-trips the created metadata =====
	got, err := svc.Files.Get(created.Id).Do()
	if err != nil {
		t.Fatalf("files.get: %v", err)
	}
	if got.MimeType != "application/vnd.google-apps.folder" ||
		len(got.Parents) != 1 || got.Parents[0] != seed.Id ||
		got.Size != 0 || got.CreatedTime == "" {
		t.Errorf("files.get: got %+v", got)
	}
	Record(t, "google-api-go-client", "drive-style", "Files.Get returns the created metadata (int64 size decodes)")

	// ===== Files.Update renames the file, Get reflects it =====
	renamed, err := svc.Files.Update(created.Id, &drive.File{Name: "renamed-by-sdk"}).Do()
	if err != nil {
		t.Fatalf("files.update: %v", err)
	}
	if renamed.Name != "renamed-by-sdk" {
		t.Errorf("files.update: name=%q", renamed.Name)
	}
	refetched, _ := svc.Files.Get(created.Id).Do()
	if refetched == nil || refetched.Name != "renamed-by-sdk" {
		t.Errorf("files.get after update: name not persisted (%+v)", refetched)
	}
	Record(t, "google-api-go-client", "drive-style", "Files.Update renames the file")

	// ===== Files.List q= filters scope the result set =====
	folders, err := svc.Files.List().Q("mimeType = 'application/vnd.google-apps.folder'").Do()
	if err != nil {
		t.Fatalf("files.list q mimeType: %v", err)
	}
	sawCreated := false
	for _, f := range folders.Files {
		if f.MimeType != "application/vnd.google-apps.folder" {
			t.Errorf("files.list q: non-folder %q leaked (mimeType=%q)", f.Id, f.MimeType)
		}
		if f.Id == created.Id {
			sawCreated = true
		}
	}
	if !sawCreated {
		t.Errorf("files.list q: created folder %q missing from folder query", created.Id)
	}
	inSeed, err := svc.Files.List().Q("parents in '" + seed.Id + "'").Do()
	if err != nil {
		t.Fatalf("files.list q parents: %v", err)
	}
	if len(inSeed.Files) != 1 || inSeed.Files[0].Id != created.Id {
		t.Errorf("files.list q parents: want only %q, got %v", created.Id, inSeed.Files)
	}
	Record(t, "google-api-go-client", "drive-style", "Files.List q= filters by mimeType and parents")

	// ===== PageSize + PageToken walk the file pages =====
	page1, err := svc.Files.List().PageSize(1).Do()
	if err != nil {
		t.Fatalf("files.list page1: %v", err)
	}
	if len(page1.Files) != 1 || page1.NextPageToken == "" {
		t.Fatalf("files.list page1: len=%d nextPageToken=%q", len(page1.Files), page1.NextPageToken)
	}
	page2, err := svc.Files.List().PageSize(1).PageToken(page1.NextPageToken).Do()
	if err != nil {
		t.Fatalf("files.list page2: %v", err)
	}
	if len(page2.Files) != 1 || page2.Files[0].Id == page1.Files[0].Id {
		t.Errorf("files.list page2: same page repeated (%v)", page2.Files)
	}
	Record(t, "google-api-go-client", "drive-style", "Files.List PageSize + PageToken walk pages")

	// ===== Deleted files surface as decoded googleapi 404s =====
	if err := svc.Files.Delete(created.Id).Do(); err != nil {
		t.Fatalf("files.delete: %v", err)
	}
	_, err = svc.Files.Get(created.Id).Do()
	if gErr, ok := err.(*googleapi.Error); !ok || gErr.Code != 404 {
		t.Errorf("files.get after delete: want googleapi 404, got %v", err)
	}
	Record(t, "google-api-go-client", "drive-style", "Files.Delete then Get -> googleapi 404")

	// ===== Changes.List replays the mutations since startPageToken =====
	changes, err := svc.Changes.List(cur.StartPageToken).Do()
	if err != nil {
		t.Fatalf("changes.list: %v", err)
	}
	sawRename, sawRemoval := false, false
	for _, ch := range changes.Changes {
		if ch.FileId != created.Id {
			continue
		}
		if ch.File != nil && ch.File.Name == "renamed-by-sdk" {
			sawRename = true
		}
		if ch.Removed {
			sawRemoval = true
		}
	}
	if !sawRename || !sawRemoval {
		t.Errorf("changes.list: rename=%v removal=%v (changes=%d)", sawRename, sawRemoval, len(changes.Changes))
	}
	if changes.NewStartPageToken == "" {
		t.Errorf("changes.list: final page carries no newStartPageToken")
	}
	Record(t, "google-api-go-client", "drive-style", "Changes.List replays mutations from startPageToken")
}
