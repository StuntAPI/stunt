package conformance

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"
)

// TestYouTubeConformance drives Google's own generated client
// (google.golang.org/api/youtube/v3) against the youtube-style adapter:
// option.WithEndpoint points the SDK at the booted engine while its
// serialization, part/id query plumbing and googleapi error decoding all
// stay stock. Like the real Data API the adapter validates bearer tokens,
// so the suite first mints one through the adapter's OAuth2
// authorization-code flow with x/oauth2 and pins the SDK to it. The
// resumable /upload/ sessions are exercised by the engine suite; this
// walks the JSON surface (the SDK's metadata-only insert path).
func TestYouTubeConformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "youtube-style")

	conf := &oauth2.Config{
		ClientID:     "conformance-client",
		ClientSecret: "conformance-secret",
		Endpoint: oauth2.Endpoint{
			AuthURL:  base + "/o/oauth2/auth",
			TokenURL: base + "/o/oauth2/token",
		},
		RedirectURL: "http://localhost:9090/callback",
		Scopes:      []string{"https://www.googleapis.com/auth/youtube.upload"},
	}

	// ===== OAuth2 authorize + exchange mint the access token the SDK presents =====

	// Do NOT follow the 302 — the redirect_uri is the client's own
	// callback (unroutable here); the code lives in the Location header.
	noRedirect := HTTPClient()
	noRedirect.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := noRedirect.Get(conf.AuthCodeURL("conformance-state"))
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 302 {
		t.Fatalf("authorize -> %d", resp.StatusCode)
	}
	code := locQuery(t, resp.Header.Get("Location"), "code")
	if code == "" {
		t.Fatal("authorize redirect carries no code")
	}
	tok, err := conf.Exchange(ctx, code)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatal("exchange returned no access token")
	}
	Record(t, "google-api-go-client", "youtube-style", "OAuth2 authorize + exchange mint the access token the SDK presents")

	svc, err := youtube.NewService(ctx,
		option.WithEndpoint(base),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok.AccessToken})),
	)
	if err != nil {
		t.Fatalf("youtube.NewService: %v", err)
	}

	insert := func(title, privacy string) *youtube.Video {
		t.Helper()
		v, err := svc.Videos.Insert([]string{"snippet", "status"}, &youtube.Video{
			Snippet: &youtube.VideoSnippet{Title: title, Description: "uploaded by google-api-go-client"},
			Status:  &youtube.VideoStatus{PrivacyStatus: privacy},
		}).Do()
		if err != nil {
			t.Fatalf("videos.insert(%s): %v", title, err)
		}
		if !strings.HasPrefix(v.Id, "mock-video-") {
			t.Errorf("videos.insert(%s): id=%q", title, v.Id)
		}
		return v
	}

	// ===== Videos.Insert metadata-only returns the resource; Videos.List id= reads it back =====
	vid1 := insert("SDK Insert One", "unlisted")
	if vid1.Snippet == nil || vid1.Snippet.Title != "SDK Insert One" {
		t.Errorf("videos.insert: snippet=%+v", vid1.Snippet)
	}
	if vid1.Status == nil || vid1.Status.UploadStatus != "processed" || vid1.Status.PrivacyStatus != "unlisted" {
		t.Errorf("videos.insert: status=%+v", vid1.Status)
	}
	got, err := svc.Videos.List([]string{"snippet", "status"}).Id(vid1.Id).Do()
	if err != nil {
		t.Fatalf("videos.list id=%s: %v", vid1.Id, err)
	}
	if len(got.Items) != 1 || got.Items[0].Id != vid1.Id || got.Items[0].Snippet == nil || got.Items[0].Snippet.Title != "SDK Insert One" {
		t.Fatalf("videos.list id=%s: %+v", vid1.Id, got.Items)
	}
	// part=snippet,status — a part that was not requested must not come back.
	if got.Items[0].Statistics != nil {
		t.Errorf("videos.list: statistics returned though not requested in part")
	}
	Record(t, "google-api-go-client", "youtube-style", "Videos.Insert metadata-only + Videos.List id= round-trip with part projection")

	// Two more videos for the paging walk below.
	vid2 := insert("SDK Insert Two", "public")
	vid3 := insert("SDK Insert Three", "private")

	// ===== Videos.List without id lists every user video (documented deviation) =====
	all, err := svc.Videos.List([]string{"snippet"}).Do()
	if err != nil {
		t.Fatalf("videos.list: %v", err)
	}
	seen := map[string]bool{}
	for _, v := range all.Items {
		seen[v.Id] = true
	}
	for _, id := range []string{vid1.Id, vid2.Id, vid3.Id} {
		if !seen[id] {
			t.Errorf("videos.list: %q missing from %v", id, seen)
		}
	}
	Record(t, "google-api-go-client", "youtube-style", "Videos.List without id lists every user video")

	// ===== Videos.List MaxResults + PageToken walk the pages =====
	page1, err := svc.Videos.List([]string{"snippet"}).MaxResults(2).Do()
	if err != nil {
		t.Fatalf("videos.list page1: %v", err)
	}
	if len(page1.Items) != 2 || page1.NextPageToken == "" {
		t.Fatalf("videos.list page1: len=%d nextPageToken=%q", len(page1.Items), page1.NextPageToken)
	}
	page2, err := svc.Videos.List([]string{"snippet"}).MaxResults(2).PageToken(page1.NextPageToken).Do()
	if err != nil {
		t.Fatalf("videos.list page2: %v", err)
	}
	if len(page2.Items) != 1 || page2.NextPageToken != "" ||
		page2.Items[0].Id == page1.Items[0].Id || page2.Items[0].Id == page1.Items[1].Id {
		t.Errorf("videos.list page2: %+v (nextPageToken=%q)", page2.Items, page2.NextPageToken)
	}
	Record(t, "google-api-go-client", "youtube-style", "Videos.List MaxResults + PageToken walk pages")

	// ===== Channels.List mine=true returns the authenticated user's channel =====
	ch, err := svc.Channels.List([]string{"snippet"}).Mine(true).Do()
	if err != nil {
		t.Fatalf("channels.list mine: %v", err)
	}
	if len(ch.Items) != 1 || !strings.HasPrefix(ch.Items[0].Id, "mock-channel-") {
		t.Fatalf("channels.list mine: %+v", ch.Items)
	}
	if ch.Items[0].Snippet == nil || !strings.HasSuffix(ch.Items[0].Snippet.Title, "Channel") {
		t.Errorf("channels.list mine: snippet=%+v", ch.Items[0].Snippet)
	}
	Record(t, "google-api-go-client", "youtube-style", "Channels.List mine=true returns the authenticated user's channel")

	// ===== Playlists.Insert + Playlists.List mine=true round-trip =====
	pl, err := svc.Playlists.Insert([]string{"snippet", "status"}, &youtube.Playlist{
		Snippet: &youtube.PlaylistSnippet{Title: "SDK Conformance Playlist", Description: "built via google-api-go-client"},
		Status:  &youtube.PlaylistStatus{PrivacyStatus: "public"},
	}).Do()
	if err != nil {
		t.Fatalf("playlists.insert: %v", err)
	}
	if !strings.HasPrefix(pl.Id, "mock-playlist-") || pl.Snippet == nil || pl.Snippet.Title != "SDK Conformance Playlist" {
		t.Errorf("playlists.insert: id=%q snippet=%+v", pl.Id, pl.Snippet)
	}
	if pl.Status == nil || pl.Status.PrivacyStatus != "public" {
		t.Errorf("playlists.insert: status=%+v", pl.Status)
	}
	if pl.ContentDetails == nil || pl.ContentDetails.ItemCount != 0 {
		t.Errorf("playlists.insert: contentDetails=%+v", pl.ContentDetails)
	}
	mine, err := svc.Playlists.List([]string{"snippet", "contentDetails"}).Mine(true).Do()
	if err != nil {
		t.Fatalf("playlists.list mine: %v", err)
	}
	listed := false
	for _, p := range mine.Items {
		if p.Id == pl.Id {
			listed = p.Snippet != nil && p.Snippet.Title == "SDK Conformance Playlist"
		}
	}
	if !listed {
		t.Errorf("playlists.list mine: created playlist %q not listed", pl.Id)
	}
	Record(t, "google-api-go-client", "youtube-style", "Playlists.Insert + Playlists.List mine=true round-trip")

	// ===== PlaylistItems.Insert adds the video, Delete removes it =====
	item, err := svc.PlaylistItems.Insert([]string{"snippet"}, &youtube.PlaylistItem{
		Snippet: &youtube.PlaylistItemSnippet{
			PlaylistId: pl.Id,
			ResourceId: &youtube.ResourceId{Kind: "youtube#video", VideoId: vid2.Id},
		},
	}).Do()
	if err != nil {
		t.Fatalf("playlistItems.insert: %v", err)
	}
	if !strings.HasPrefix(item.Id, "mock-playlist-item-") {
		t.Errorf("playlistItems.insert: id=%q", item.Id)
	}
	// Real API returns the full resource: the video's title rides snippet.
	if item.Snippet == nil || item.Snippet.Title != vid2.Snippet.Title || item.Snippet.ResourceId == nil || item.Snippet.ResourceId.VideoId != vid2.Id {
		t.Errorf("playlistItems.insert: snippet=%+v, want title %q + resourceId %q", item.Snippet, vid2.Snippet.Title, vid2.Id)
	}
	if err := svc.PlaylistItems.Delete(item.Id).Do(); err != nil {
		t.Fatalf("playlistItems.delete: %v", err)
	}
	Record(t, "google-api-go-client", "youtube-style", "PlaylistItems.Insert adds the video, Delete removes it")

	// ===== Videos.Delete -> 204; re-delete surfaces a googleapi 404 =====
	if err := svc.Videos.Delete(vid3.Id).Do(); err != nil {
		t.Fatalf("videos.delete: %v", err)
	}
	// Like the real API, list by an unknown id omits it (200 + no items).
	afterDel, err := svc.Videos.List([]string{"snippet"}).Id(vid3.Id).Do()
	if err != nil {
		t.Fatalf("videos.list after delete: %v", err)
	}
	if len(afterDel.Items) != 0 {
		t.Errorf("videos.list after delete: %d items, want 0", len(afterDel.Items))
	}
	err = svc.Videos.Delete(vid3.Id).Do()
	if gErr, ok := err.(*googleapi.Error); !ok || gErr.Code != 404 {
		t.Errorf("videos.delete twice: want googleapi 404, got %v", err)
	}
	Record(t, "google-api-go-client", "youtube-style", "Videos.Delete -> 204; re-delete surfaces a googleapi 404")
}
