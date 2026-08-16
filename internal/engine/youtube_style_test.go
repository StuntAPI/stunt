package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
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

	// ===== Upload a video (resumable: initiate -> single final chunk) =====

	uploadBody := map[string]any{
		"snippet": map[string]any{
			"title":       "My Awesome Video",
			"description": "This is a test video uploaded via stunt",
		},
		"status": map[string]any{
			"privacyStatus": "unlisted",
		},
	}
	video := youtubeResumableUpload(t, base, accessToken, uploadBody, []byte("fake video media bytes"))
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

	var body string
	var status int
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

// TestYouTubeStyleResumableUpload drives the real videos.insert resumable
// protocol chunk by chunk:
//
//   - initiate → 200 with a Location session URL and an empty body
//   - status probe → 308 Resume Incomplete (no Range before the first byte)
//   - chunked upload of a multi-KB binary: 308s with Range headers that
//     track the accepted bytes; the video does not exist until completion
//   - sending the final range while a middle chunk is still missing → 400
//   - the final chunk creates the video; part=fileDetails reports the exact
//     byte count (the real owner-only projection)
//   - cancel (DELETE session URL) → 499: session gone and no video created
//   - unknown session id → 404
func TestYouTubeStyleResumableUpload(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "youtube-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"youtube": {Adapter: adapterDir},
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

	const redirectURI = "http://localhost:8080/callback"
	const clientID = "yt-resume-client"
	const clientSecret = "yt-resume-secret"
	code := youtubeAuthorize(t, base, redirectURI, "state-yt-r", clientID)
	accessToken := youtubeExchange(t, base, code, clientID, clientSecret, redirectURI)

	// ===== Initiate =====
	metadata := map[string]any{
		"snippet": map[string]any{"title": "Chunked Binary", "description": "multi-KB"},
		"status":  map[string]any{"privacyStatus": "unlisted"},
	}
	loc := youtubeInitiateUpload(t, base, accessToken, metadata)

	// Probe before any bytes: 308, no Range header.
	probe := youtubePutChunk(t, loc, "", "")
	if probe.StatusCode != 308 || probe.Header.Get("Range") != "" {
		t.Fatalf("probe -> %d Range=%q, want 308 without Range", probe.StatusCode, probe.Header.Get("Range"))
	}
	io.ReadAll(probe.Body)
	probe.Body.Close()

	// Three chunks: 5 KiB + 4 KiB + 3 KiB (12288 total).
	chunk1 := youtubeTestBytes(5*1024, 1)
	chunk2 := youtubeTestBytes(4*1024, 2)
	chunk3 := youtubeTestBytes(3*1024, 3)

	r := youtubePutChunk(t, loc, string(chunk1), "bytes 0-5119/12288")
	if r.StatusCode != 308 || r.Header.Get("Range") != "bytes=0-5119" {
		t.Fatalf("chunk 1 -> %d Range=%q, want 308 bytes=0-5119", r.StatusCode, r.Header.Get("Range"))
	}
	io.ReadAll(r.Body)
	r.Body.Close()

	// The video must not exist before the final chunk lands.
	body, status := youtubeGetAuth(t, base+"/youtube/v3/videos?part=snippet&mine=true", accessToken)
	if status != 200 || strings.Contains(body, "Chunked Binary") {
		t.Fatalf("video visible before completion: %d %s", status, body)
	}

	// Sending the FINAL range while chunk 2 is still missing → 400
	// (chunk start does not match the next expected offset).
	r = youtubePutChunk(t, loc, string(chunk3), "bytes 9216-12287/12288")
	if r.StatusCode != 400 {
		t.Fatalf("final range with missing middle chunk -> %d, want 400", r.StatusCode)
	}
	io.ReadAll(r.Body)
	r.Body.Close()

	// Differing total on the next chunk → 400.
	r = youtubePutChunk(t, loc, string(chunk2), "bytes 5120-9215/99999")
	if r.StatusCode != 400 {
		t.Fatalf("differing total -> %d, want 400", r.StatusCode)
	}
	io.ReadAll(r.Body)
	r.Body.Close()

	// Chunk 2 → 308; final chunk 3 → 200 with the video resource.
	r = youtubePutChunk(t, loc, string(chunk2), "bytes 5120-9215/12288")
	if r.StatusCode != 308 || r.Header.Get("Range") != "bytes=0-9215" {
		t.Fatalf("chunk 2 -> %d Range=%q, want 308 bytes=0-9215", r.StatusCode, r.Header.Get("Range"))
	}
	io.ReadAll(r.Body)
	r.Body.Close()

	r = youtubePutChunk(t, loc, string(chunk3), "bytes 9216-12287/12288")
	finalBody, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("final chunk -> %d; body %s", r.StatusCode, finalBody)
	}
	var video map[string]any
	if err := json.Unmarshal(finalBody, &video); err != nil {
		t.Fatalf("unmarshal final chunk response: %v (body %s)", err, finalBody)
	}
	videoID, _ := video["id"].(string)
	if videoID == "" {
		t.Fatalf("final chunk response has no id: %s", finalBody)
	}

	// part=fileDetails reports the exact accepted byte count.
	body, status = youtubeGetAuth(t, base+"/youtube/v3/videos?part=snippet,fileDetails&id="+videoID, accessToken)
	if status != 200 {
		t.Fatalf("videos.list after upload -> %d; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatal(err)
	}
	items := listResp["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("videos.list items = %v, want 1", items)
	}
	fd := items[0].(map[string]any)["fileDetails"].(map[string]any)
	if fd["fileSize"] != float64(12288) {
		t.Fatalf("fileDetails.fileSize = %v, want %d", fd["fileSize"], 12288)
	}

	// The completed session is gone.
	probe = youtubePutChunk(t, loc, "", "")
	io.ReadAll(probe.Body)
	probe.Body.Close()
	if probe.StatusCode != 404 {
		t.Fatalf("probe after completion -> %d, want 404", probe.StatusCode)
	}

	// ===== Cancel: 499, no video created =====
	cancelLoc := youtubeInitiateUpload(t, base, accessToken, map[string]any{
		"snippet": map[string]any{"title": "Canceled Video"},
	})
	r = youtubePutChunk(t, cancelLoc, string(chunk1), "bytes 0-5119/15360")
	if r.StatusCode != 308 {
		t.Fatalf("cancel-prep chunk -> %d, want 308", r.StatusCode)
	}
	io.ReadAll(r.Body)
	r.Body.Close()

	cancelReq, err := http.NewRequest("DELETE", cancelLoc, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancelResp, err := http.DefaultClient.Do(cancelReq)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(cancelResp.Body)
	cancelResp.Body.Close()
	if cancelResp.StatusCode != 499 {
		t.Fatalf("cancel -> %d, want 499", cancelResp.StatusCode)
	}

	// Session gone and the canceled video never appeared.
	probe = youtubePutChunk(t, cancelLoc, "", "")
	io.ReadAll(probe.Body)
	probe.Body.Close()
	if probe.StatusCode != 404 {
		t.Fatalf("probe after cancel -> %d, want 404", probe.StatusCode)
	}
	body, status = youtubeGetAuth(t, base+"/youtube/v3/videos?part=snippet&mine=true", accessToken)
	if status != 200 || strings.Contains(body, "Canceled Video") {
		t.Fatalf("canceled video is visible: %d %s", status, body)
	}

	// Unknown session id → 404.
	probe = youtubePutChunk(t, base+"/upload/youtube/v3/videos?uploadType=resumable&upload_id=ytup_nope", "", "")
	io.ReadAll(probe.Body)
	probe.Body.Close()
	if probe.StatusCode != 404 {
		t.Fatalf("unknown session -> %d, want 404", probe.StatusCode)
	}
}

// youtubeInitiateUpload starts a resumable session and returns its URL.
func youtubeInitiateUpload(t *testing.T, base, token string, metadata map[string]any) string {
	t.Helper()
	data, _ := json.Marshal(metadata)
	req, err := http.NewRequest("POST", base+"/upload/youtube/v3/videos?uploadType=resumable&part=snippet,status", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("initiate -> %d; body %s", resp.StatusCode, body)
	}
	if len(body) != 0 {
		t.Fatalf("initiate body = %q, want empty", body)
	}
	loc := resp.Header.Get("Location")
	if loc == "" || !strings.Contains(loc, "upload_id=") {
		t.Fatalf("initiate Location = %q, want session URL with upload_id", loc)
	}
	return loc
}

// youtubePutChunk PUTs one chunk (body + optional Content-Range header) and
// returns the raw response.
func youtubePutChunk(t *testing.T, urlStr, body, contentRange string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("PUT", urlStr, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentRange != "" {
		req.Header.Set("Content-Range", contentRange)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// youtubeResumableUpload runs the two-phase flow with the metadata and the
// full media as a single final chunk, returning the created video resource.
func youtubeResumableUpload(t *testing.T, base, token string, metadata map[string]any, media []byte) map[string]any {
	t.Helper()
	loc := youtubeInitiateUpload(t, base, token, metadata)
	resp := youtubePutChunk(t, loc, string(media), fmt.Sprintf("bytes 0-%d/%d", len(media)-1, len(media)))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("resumable final chunk -> %d; body %s", resp.StatusCode, body)
	}
	var video map[string]any
	if err := json.Unmarshal(body, &video); err != nil {
		t.Fatalf("unmarshal video: %v (body %s)", err, body)
	}
	return video
}

// youtubeTestBytes returns n deterministic pseudo-random bytes.
func youtubeTestBytes(n int, seed int) []byte {
	out := make([]byte, n)
	x := uint32(seed)*2654435761 + 77777
	for i := range out {
		x = x*1664525 + 1013904223
		out[i] = byte(x >> 11)
	}
	return out
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
