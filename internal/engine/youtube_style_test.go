package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestYouTubeStyleAdapter exercises the YouTube Data API v3 reference adapter
// end-to-end through the full OAuth2 + upload + list + playlists flow:
//
//   - OAuth2 authorize → 302; token exchange → bearer pair
//   - POST /upload/youtube/v3/videos → video resource {id, snippet, status}
//   - GET /youtube/v3/videos?id=... shows the uploaded video (STATEFUL)
//   - channels: GET /youtube/v3/channels?mine=true → channel
//   - playlists: create → list shows it
//   - playlistItems: add video to playlist
func TestYouTubeStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "youtube-style")
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
			"youtube": {Adapter: absAdapterDir},
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

	base := addrs["youtube"]

	// ===== OAuth2 → get access token =====

	const redirectURI = "http://localhost:8080/callback"
	const clientID = "yt-test-client-id"
	const clientSecret = "yt-test-client-secret"

	code := youtubeAuthorize(t, base, redirectURI, "state-yt", clientID)
	accessToken := youtubeExchange(t, base, code, clientID, clientSecret, redirectURI)

	// ===== Upload a video =====

	uploadBody := map[string]any{
		"snippet": map[string]any{
			"title":       "My Awesome Video",
			"description": "This is a test video uploaded via stunt",
		},
		"status": map[string]any{
			"privacyStatus": "unlisted",
		},
	}
	body, status := youtubePostJSONAuth(t, base+"/upload/youtube/v3/videos?uploadType=resumable&part=snippet,status", accessToken, uploadBody)
	if status != 200 {
		t.Fatalf("upload video -> status %d, want 200; body %s", status, body)
	}
	var video map[string]any
	if err := json.Unmarshal([]byte(body), &video); err != nil {
		t.Fatalf("unmarshal upload response: %v (body %s)", err, body)
	}
	videoID, ok := video["id"].(string)
	if !ok || !strings.HasPrefix(videoID, "mock-video-") {
		t.Fatalf("video id = %v, want mock-video-* prefix", video["id"])
	}
	snippet, ok := video["snippet"].(map[string]any)
	if !ok {
		t.Fatalf("snippet = %v, want dict", video["snippet"])
	}
	if snippet["title"] != "My Awesome Video" {
		t.Fatalf("title = %v, want My Awesome Video", snippet["title"])
	}
	videoStatus, ok := video["status"].(map[string]any)
	if !ok {
		t.Fatalf("status = %v, want dict", video["status"])
	}
	if videoStatus["privacyStatus"] != "unlisted" {
		t.Fatalf("privacyStatus = %v, want unlisted", videoStatus["privacyStatus"])
	}

	// ===== STATEFUL: GET /youtube/v3/videos?id=... shows the uploaded video =====

	body, status = youtubeGetAuth(t, base+"/youtube/v3/videos?id="+videoID+"&part=snippet", accessToken)
	if status != 200 {
		t.Fatalf("GET videos -> status %d, want 200; body %s", status, body)
	}
	var videoListResp map[string]any
	if err := json.Unmarshal([]byte(body), &videoListResp); err != nil {
		t.Fatalf("unmarshal video list: %v (body %s)", err, body)
	}
	items, ok := videoListResp["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("videos items = %v, want list of 1", videoListResp["items"])
	}
	foundVideo := items[0].(map[string]any)
	if foundVideo["id"] != videoID {
		t.Fatalf("found video id = %v, want %v", foundVideo["id"], videoID)
	}

	// GET /youtube/v3/videos?id=nonexistent → empty list
	body, status = youtubeGetAuth(t, base+"/youtube/v3/videos?id=no-such-video&part=snippet", accessToken)
	if status != 200 {
		t.Fatalf("GET videos nonexistent -> status %d, want 200", status)
	}
	if err := json.Unmarshal([]byte(body), &videoListResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	items = videoListResp["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("nonexistent video items count = %d, want 0", len(items))
	}

	// ===== Channels: GET /youtube/v3/channels?mine=true =====

	body, status = youtubeGetAuth(t, base+"/youtube/v3/channels?part=snippet&mine=true", accessToken)
	if status != 200 {
		t.Fatalf("GET channels -> status %d, want 200; body %s", status, body)
	}
	var channelResp map[string]any
	if err := json.Unmarshal([]byte(body), &channelResp); err != nil {
		t.Fatalf("unmarshal channels: %v (body %s)", err, body)
	}
	channels, ok := channelResp["items"].([]any)
	if !ok || len(channels) != 1 {
		t.Fatalf("channels items = %v, want list of 1", channelResp["items"])
	}
	channel := channels[0].(map[string]any)
	channelID, ok := channel["id"].(string)
	if !ok || !strings.HasPrefix(channelID, "mock-channel-") {
		t.Fatalf("channel id = %v, want mock-channel-* prefix", channel["id"])
	}

	// ===== Playlists: create → list =====

	playlistBody := map[string]any{
		"snippet": map[string]any{
			"title":       "My Playlist",
			"description": "A test playlist",
		},
		"status": map[string]any{
			"privacyStatus": "private",
		},
	}
	body, status = youtubePostJSONAuth(t, base+"/youtube/v3/playlists?part=snippet,status,contentDetails", accessToken, playlistBody)
	if status != 200 {
		t.Fatalf("create playlist -> status %d, want 200; body %s", status, body)
	}
	var playlist map[string]any
	if err := json.Unmarshal([]byte(body), &playlist); err != nil {
		t.Fatalf("unmarshal create playlist: %v (body %s)", err, body)
	}
	playlistID, ok := playlist["id"].(string)
	if !ok || !strings.HasPrefix(playlistID, "mock-playlist-") {
		t.Fatalf("playlist id = %v, want mock-playlist-* prefix", playlist["id"])
	}
	if playlist["snippet"].(map[string]any)["title"] != "My Playlist" {
		t.Fatalf("playlist title mismatch")
	}

	// GET /youtube/v3/playlists?mine=true → shows it
	body, status = youtubeGetAuth(t, base+"/youtube/v3/playlists?part=snippet,contentDetails&mine=true", accessToken)
	if status != 200 {
		t.Fatalf("list playlists -> status %d, want 200; body %s", status, body)
	}
	var playlistsResp map[string]any
	if err := json.Unmarshal([]byte(body), &playlistsResp); err != nil {
		t.Fatalf("unmarshal list playlists: %v (body %s)", err, body)
	}
	foundPlaylists, ok := playlistsResp["items"].([]any)
	if !ok || len(foundPlaylists) != 1 {
		t.Fatalf("playlists items count = %d, want 1", len(foundPlaylists))
	}

	// ===== Playlist items: add video to playlist =====

	itemBody := map[string]any{
		"snippet": map[string]any{
			"playlistId": playlistID,
			"resourceId": map[string]any{
				"kind":    "youtube#video",
				"videoId": videoID,
			},
		},
	}
	body, status = youtubePostJSONAuth(t, base+"/youtube/v3/playlistItems?part=snippet", accessToken, itemBody)
	if status != 200 {
		t.Fatalf("add playlist item -> status %d, want 200; body %s", status, body)
	}
	var itemResp map[string]any
	if err := json.Unmarshal([]byte(body), &itemResp); err != nil {
		t.Fatalf("unmarshal add playlist item: %v (body %s)", err, body)
	}
	itemID, ok := itemResp["id"].(string)
	if !ok || !strings.HasPrefix(itemID, "mock-playlist-item-") {
		t.Fatalf("playlist item id = %v, want mock-playlist-item-* prefix", itemResp["id"])
	}

	// Add to nonexistent playlist → 404
	badItemBody := map[string]any{
		"snippet": map[string]any{
			"playlistId": "no-such-playlist",
			"resourceId": map[string]any{
				"kind":    "youtube#video",
				"videoId": videoID,
			},
		},
	}
	_, status = youtubePostJSONAuth(t, base+"/youtube/v3/playlistItems?part=snippet", accessToken, badItemBody)
	if status != 404 {
		t.Fatalf("add to nonexistent playlist -> status %d, want 404", status)
	}

	// ===== No auth → 401 =====

	_, status = youtubeGetAuth(t, base+"/youtube/v3/videos?id="+videoID, "")
	if status != 401 {
		t.Fatalf("GET videos no auth -> status %d, want 401", status)
	}

	// ===== Catch-all 404 =====

	_, status = youtubeGetAuth(t, base+"/youtube/v3/no-such-resource", accessToken)
	if status != 404 {
		t.Fatalf("GET unmatched route -> status %d, want 404", status)
	}
}

// === Helpers ===

func youtubeAuthorize(t *testing.T, base, redirectURI, state, clientID string) string {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(base + "/o/oauth2/auth?client_id=" + clientID +
		"&redirect_uri=" + url.QueryEscape(redirectURI) +
		"&state=" + state + "&response_type=code")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 302 {
		t.Fatalf("authorize -> status %d, want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	u, _ := url.Parse(loc)
	code := u.Query().Get("code")
	if code == "" {
		t.Fatal("authorize: no code in redirect")
	}
	return code
}

func youtubeExchange(t *testing.T, base, code, clientID, clientSecret, redirectURI string) string {
	t.Helper()
	resp, err := http.PostForm(base+"/o/oauth2/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("exchange -> status %d, body %s", resp.StatusCode, b)
	}
	var tokens map[string]any
	if err := json.Unmarshal(b, &tokens); err != nil {
		t.Fatalf("unmarshal tokens: %v (body %s)", err, b)
	}
	access, ok := tokens["access_token"].(string)
	if !ok {
		t.Fatalf("access_token = %v, want string", tokens["access_token"])
	}
	return access
}

func youtubePostJSONAuth(t *testing.T, urlStr, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", urlStr, bytes.NewReader(data))
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

func youtubeGetAuth(t *testing.T, urlStr, token string) (string, int) {
	t.Helper()
	return getAuth(t, urlStr, token)
}
