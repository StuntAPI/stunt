package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestGDocsStyleAdapter exercises the gdocs-style adapter:
//
//   - 401 without auth
//   - Create document → documentId, title
//   - GET document → structural content model
//   - batchUpdate insertText → replies + text visible on GET (STATEFUL)
//   - multi-paragraph inserts, updateParagraphStyle, updateTextStyle
//   - createParagraphBullets / deleteParagraphBullets
//   - insertPageBreak
//   - insertInlineImage (JSON base64 and multipart), bytes in the blob store
//   - deleteContentRange
//   - UTF-16 index arithmetic (surrogate pairs)
//   - failure paths: unknown request type, bad index, bad range, bad
//     namedStyle, unknown document
//   - Revisions
func TestGDocsStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "gdocs-style")
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
			"docs": {Adapter: absAdapterDir},
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

	base := addrs["docs"]
	token := "mock-oauth2-token"

	// ===== 401 without auth =====

	_, status := gdocsGet(t, base+"/v1/documents/test-doc-id", "")
	if status != 401 {
		t.Fatalf("get without auth -> status %d, want 401", status)
	}

	// ===== Create document =====

	body, status := gdocsPost(t, base+"/v1/documents", token, map[string]any{
		"title": "My Test Document",
	})
	if status != 200 {
		t.Fatalf("create -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	docID, ok := resp["documentId"].(string)
	if !ok || docID == "" {
		t.Fatalf("documentId = %v, want non-empty string", resp["documentId"])
	}
	if resp["title"] != "My Test Document" {
		t.Fatalf("title = %v, want 'My Test Document'", resp["title"])
	}

	// Verify body.content exists with the structural shape.
	content := gdocsContent(t, resp)
	if len(content) < 1 {
		t.Fatalf("content = %v, want non-empty array", content)
	}
	first := content[0].(map[string]any)
	if first["startIndex"].(float64) != 1 {
		t.Fatalf("first startIndex = %v, want 1", first["startIndex"])
	}
	if first["endIndex"].(float64) != 2 {
		t.Fatalf("empty doc first endIndex = %v, want 2", first["endIndex"])
	}

	// ===== batchUpdate insertText =====

	body, status = gdocsPost(t, base+"/v1/documents/"+docID+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"insertText": map[string]any{
					"location": map[string]any{"index": 1},
					"text":     "Hello, World!",
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("batchUpdate -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal batchUpdate: %v (body %s)", err, body)
	}
	if resp["documentId"] != docID {
		t.Fatalf("batchUpdate documentId = %v, want %v", resp["documentId"], docID)
	}
	replies, ok := resp["replies"].([]any)
	if !ok || len(replies) != 1 {
		t.Fatalf("replies = %v, want array of 1", resp["replies"])
	}

	// ===== GET revisions → clock-derived modifiedTime =====

	body, status = gdocsGet(t, base+"/v1/documents/"+docID+"/revisions", token)
	if status != 200 {
		t.Fatalf("revisions -> status %d, want 200; body %s", status, body)
	}
	var revResp map[string]any
	if err := json.Unmarshal([]byte(body), &revResp); err != nil {
		t.Fatalf("unmarshal revisions: %v (body %s)", err, body)
	}
	revs, ok := revResp["revisions"].([]any)
	if !ok || len(revs) != 2 {
		t.Fatalf("revisions = %v, want 2 (create + batchUpdate)", revResp["revisions"])
	}
	for i, r := range revs {
		rm := r.(map[string]any)
		assertRecentDocsStamp(t, rm["modifiedTime"], fmt.Sprintf("revisions[%d].modifiedTime", i))
	}

	// ===== GET document → inserted text visible, ranged structure =====

	body, status = gdocsGet(t, base+"/v1/documents/"+docID, token)
	if status != 200 {
		t.Fatalf("get after update -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal get: %v (body %s)", err, body)
	}
	content = gdocsContent(t, resp)
	if len(content) != 1 {
		t.Fatalf("content after insert has %d items, want 1", len(content))
	}
	para0 := content[0].(map[string]any)
	if para0["endIndex"].(float64) != 15 { // 13 chars + trailing newline, end exclusive
		t.Fatalf("endIndex = %v, want 15", para0["endIndex"])
	}
	if got := gdocsParaText(t, para0); got != "Hello, World!\n" {
		t.Fatalf("paragraph text = %q, want %q", got, "Hello, World!\n")
	}
	pstyle := gdocsParaStyle(t, para0)
	if pstyle["namedStyle"] != "NORMAL_TEXT" {
		t.Fatalf("namedStyle = %v, want NORMAL_TEXT", pstyle["namedStyle"])
	}

	// ===== endOfSegmentLocation insert → second paragraph =====

	body, status = gdocsPost(t, base+"/v1/documents/"+docID+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"insertText": map[string]any{
					"endOfSegmentLocation": map[string]any{"segmentId": ""},
					// The leading newline starts a new paragraph, as in the
					// real API.
					"text": "\nSecond paragraph",
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("batchUpdate endOfSegment -> status %d, want 200; body %s", status, body)
	}
	body, status = gdocsGet(t, base+"/v1/documents/"+docID, token)
	if status != 200 {
		t.Fatalf("get -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal get: %v (body %s)", err, body)
	}
	content = gdocsContent(t, resp)
	if len(content) != 2 {
		t.Fatalf("content has %d items, want 2", len(content))
	}
	para1 := content[1].(map[string]any)
	if para1["startIndex"].(float64) != 15 {
		t.Fatalf("second paragraph startIndex = %v, want 15", para1["startIndex"])
	}
	if para1["endIndex"].(float64) != 32 { // 16 chars + newline, end exclusive
		t.Fatalf("second paragraph endIndex = %v, want 32", para1["endIndex"])
	}

	// ===== updateParagraphStyle (namedStyle HEADING_1) =====

	body, status = gdocsPost(t, base+"/v1/documents/"+docID+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"updateParagraphStyle": map[string]any{
					"range":          map[string]any{"startIndex": 1, "endIndex": 15},
					"paragraphStyle": map[string]any{"namedStyle": "HEADING_1"},
					"fields":         "namedStyle",
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("updateParagraphStyle -> status %d, want 200; body %s", status, body)
	}
	body, _ = gdocsGet(t, base+"/v1/documents/"+docID, token)
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal get: %v (body %s)", err, body)
	}
	if got := gdocsParaStyle(t, gdocsContent(t, resp)[0].(map[string]any)); got["namedStyle"] != "HEADING_1" {
		t.Fatalf("namedStyle after update = %v, want HEADING_1", got["namedStyle"])
	}

	// ===== updateTextStyle (bold) =====

	body, status = gdocsPost(t, base+"/v1/documents/"+docID+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"updateTextStyle": map[string]any{
					"range":     map[string]any{"startIndex": 1, "endIndex": 6},
					"textStyle": map[string]any{"bold": true},
					"fields":    "bold",
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("updateTextStyle -> status %d, want 200; body %s", status, body)
	}
	body, _ = gdocsGet(t, base+"/v1/documents/"+docID, token)
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal get: %v (body %s)", err, body)
	}
	els := gdocsParaElements(t, gdocsContent(t, resp)[0].(map[string]any))
	if len(els) < 2 {
		t.Fatalf("expected styled split run, got %d elements", len(els))
	}
	run0 := els[0].(map[string]any)["textRun"].(map[string]any)
	style0 := run0["textStyle"].(map[string]any)
	if style0["bold"] != true {
		t.Fatalf("bold = %v, want true (run %v)", style0["bold"], run0)
	}
	if run0["content"] != "Hello" {
		t.Fatalf("bold run content = %v, want 'Hello'", run0["content"])
	}

	// ===== createParagraphBullets + deleteParagraphBullets =====

	body, status = gdocsPost(t, base+"/v1/documents/"+docID+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"createParagraphBullets": map[string]any{
					"range":        map[string]any{"startIndex": 15, "endIndex": 32},
					"bulletPreset": "NUMBERED_DECIMAL_ALPHA_ROMAN",
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("createParagraphBullets -> status %d, want 200; body %s", status, body)
	}
	body, _ = gdocsGet(t, base+"/v1/documents/"+docID, token)
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal get: %v (body %s)", err, body)
	}
	content = gdocsContent(t, resp)
	bulletPara := content[len(content)-1].(map[string]any)["paragraph"].(map[string]any)
	bullet, ok := bulletPara["bullet"].(map[string]any)
	if !ok {
		t.Fatalf("bullet missing after createParagraphBullets: %v", bulletPara)
	}
	if bullet["listId"] == nil || bullet["nestingLevel"].(float64) != 0 {
		t.Fatalf("bullet = %v, want listId + nestingLevel 0", bullet)
	}
	lists, ok := resp["lists"].(map[string]any)
	if !ok || len(lists) != 1 {
		t.Fatalf("lists = %v, want one list", resp["lists"])
	}
	list := lists[bullet["listId"].(string)].(map[string]any)
	glyph := list["listProperties"].(map[string]any)["nestingLevels"].([]any)[0].(map[string]any)["glyph"]
	if glyph != "1." {
		t.Fatalf("glyph = %v, want '1.'", glyph)
	}

	body, status = gdocsPost(t, base+"/v1/documents/"+docID+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"deleteParagraphBullets": map[string]any{
					"range": map[string]any{"startIndex": 15, "endIndex": 32},
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("deleteParagraphBullets -> status %d, want 200; body %s", status, body)
	}
	body, _ = gdocsGet(t, base+"/v1/documents/"+docID, token)
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal get: %v (body %s)", err, body)
	}
	content = gdocsContent(t, resp)
	if _, still := content[len(content)-1].(map[string]any)["paragraph"].(map[string]any)["bullet"]; still {
		t.Fatalf("bullet still present after deleteParagraphBullets")
	}

	// ===== insertPageBreak at the start of the document =====

	body, status = gdocsPost(t, base+"/v1/documents/"+docID+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"insertPageBreak": map[string]any{
					"location": map[string]any{"index": 1},
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("insertPageBreak -> status %d, want 200; body %s", status, body)
	}
	body, _ = gdocsGet(t, base+"/v1/documents/"+docID, token)
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal get: %v (body %s)", err, body)
	}
	els = gdocsParaElements(t, gdocsContent(t, resp)[0].(map[string]any))
	if _, ok := els[0].(map[string]any)["pageBreak"]; !ok {
		t.Fatalf("first element = %v, want a pageBreak", els[0])
	}

	// ===== insertInlineImage via JSON base64 =====

	pngBytes := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3, 4}
	body, status = gdocsPost(t, base+"/v1/documents/"+docID+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"insertInlineImage": map[string]any{
					"location":  map[string]any{"index": 1},
					"imageData": map[string]any{"data": base64.StdEncoding.EncodeToString(pngBytes), "mimeType": "image/png"},
					"objectSize": map[string]any{
						"height": map[string]any{"magnitude": 60, "unit": "PT"},
						"width":  map[string]any{"magnitude": 80, "unit": "PT"},
					},
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("insertInlineImage -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal insertInlineImage: %v (body %s)", err, body)
	}
	replies = resp["replies"].([]any)
	imageReply := replies[0].(map[string]any)
	objectID, ok := imageReply["objectId"].(string)
	if !ok || objectID == "" {
		t.Fatalf("insertInlineImage reply = %v, want objectId", imageReply)
	}
	body, _ = gdocsGet(t, base+"/v1/documents/"+docID, token)
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal get: %v (body %s)", err, body)
	}
	inlineObjects, ok := resp["inlineObjects"].(map[string]any)
	if !ok {
		t.Fatalf("inlineObjects missing: %v", resp["inlineObjects"])
	}
	obj := inlineObjects[objectID].(map[string]any)
	embedded := obj["inlineObjectProperties"].(map[string]any)["embeddedObject"].(map[string]any)
	if embedded["mimeType"] != "image/png" {
		t.Fatalf("mimeType = %v, want image/png", embedded["mimeType"])
	}
	els = gdocsParaElements(t, gdocsContent(t, resp)[0].(map[string]any))
	objEl, ok := els[0].(map[string]any)["inlineObjectElement"].(map[string]any)
	if !ok || objEl["inlineObjectId"] != objectID {
		t.Fatalf("first element = %v, want inlineObjectElement %s", els[0], objectID)
	}

	// The image bytes live in the blob store under the objectId.
	_, _, blobs, ok := e.StateStores("docs")
	if !ok {
		t.Fatal("StateStores(docs) not ok")
	}
	rc, err := blobs.Get("gdocs", objectID)
	if err != nil {
		t.Fatalf("blob get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, pngBytes) {
		t.Fatalf("blob content = %v, want %v", got, pngBytes)
	}

	// ===== deleteContentRange: delete "ello" from "Hello" =====

	// Current text starts with the inline image (index 1) then the page
	// break (index 2), so "Hello" occupies indices 3..8.
	body, status = gdocsPost(t, base+"/v1/documents/"+docID+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"deleteContentRange": map[string]any{
					"range": map[string]any{"startIndex": 4, "endIndex": 8},
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("deleteContentRange -> status %d, want 200; body %s", status, body)
	}
	body, _ = gdocsGet(t, base+"/v1/documents/"+docID, token)
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal get: %v (body %s)", err, body)
	}
	fullText := gdocsDocText(t, resp)
	if bytes.Contains([]byte(fullText), []byte("ello")) {
		t.Fatalf("deleted text still present: %q", fullText)
	}
	if !bytes.Contains([]byte(fullText), []byte("H, World!")) {
		t.Fatalf("text after delete = %q, want 'H, World!'", fullText)
	}

	// ===== UTF-16 surrogate-pair index arithmetic =====

	body, status = gdocsPost(t, base+"/v1/documents", token, map[string]any{"title": "Emoji doc"})
	if status != 200 {
		t.Fatalf("create emoji doc -> status %d; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	emojiDoc := resp["documentId"].(string)

	body, status = gdocsPost(t, base+"/v1/documents/"+emojiDoc+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"insertText": map[string]any{
					"location": map[string]any{"index": 1},
					"text":     "\U0001F600", // 😀: 4 UTF-8 bytes, 2 UTF-16 units
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("insert emoji -> status %d; body %s", status, body)
	}
	body, _ = gdocsGet(t, base+"/v1/documents/"+emojiDoc, token)
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal get: %v (body %s)", err, body)
	}
	// The emoji counts as TWO units: paragraph spans 1..4 (2 + newline).
	if got := gdocsContent(t, resp)[0].(map[string]any)["endIndex"].(float64); got != 4 {
		t.Fatalf("emoji paragraph endIndex = %v, want 4 (surrogate pair = 2 units)", got)
	}

	// An index inside the surrogate pair is a 400 INVALID_ARGUMENT.
	body, status = gdocsPost(t, base+"/v1/documents/"+emojiDoc+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"insertText": map[string]any{
					"location": map[string]any{"index": 2},
					"text":     "X",
				},
			},
		},
	})
	if status != 400 {
		t.Fatalf("insert inside surrogate pair -> status %d, want 400; body %s", status, body)
	}
	if !gdocsIsInvalidArgument(t, body, "surrogate") {
		t.Fatalf("expected surrogate-pair INVALID_ARGUMENT error, got %s", body)
	}

	// Index 3 is after the emoji and before the final newline.
	body, status = gdocsPost(t, base+"/v1/documents/"+emojiDoc+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"insertText": map[string]any{
					"location": map[string]any{"index": 3},
					"text":     "X",
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("insert after emoji -> status %d; body %s", status, body)
	}
	body, _ = gdocsGet(t, base+"/v1/documents/"+emojiDoc, token)
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal get: %v (body %s)", err, body)
	}
	if got := gdocsDocText(t, resp); got != "\U0001F600X\n" {
		t.Fatalf("emoji doc text = %q, want '😀X\\n'", got)
	}

	// ===== multipart batchUpdate: metadata part + image file part =====

	mpBody := &bytes.Buffer{}
	mw := multipart.NewWriter(mpBody)
	metaJSON, _ := json.Marshal(map[string]any{
		"requests": []map[string]any{
			{
				"insertInlineImage": map[string]any{
					"location": map[string]any{"index": 1},
				},
			},
		},
	})
	if err := mw.WriteField("metadata", string(metaJSON)); err != nil {
		t.Fatal(err)
	}
	jpgBytes := []byte{0xff, 0xd8, 0xff, 0xe0, 9, 9, 9}
	fw, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="image"; filename="photo.jpg"`},
		"Content-Type":        {"image/jpeg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(jpgBytes); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("POST", base+"/v1/documents/"+emojiDoc+"/batchUpdate", bytes.NewReader(mpBody.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	mpRaw, _ := io.ReadAll(httpResp.Body)
	httpResp.Body.Close()
	if httpResp.StatusCode != 200 {
		t.Fatalf("multipart batchUpdate -> status %d, want 200; body %s", httpResp.StatusCode, mpRaw)
	}
	if err := json.Unmarshal(mpRaw, &resp); err != nil {
		t.Fatalf("unmarshal multipart reply: %v (body %s)", err, mpRaw)
	}
	mpOID := resp["replies"].([]any)[0].(map[string]any)["objectId"].(string)
	if mpOID == "" {
		t.Fatalf("multipart objectId empty: %s", mpRaw)
	}
	rc, err = blobs.Get("gdocs", mpOID)
	if err != nil {
		t.Fatalf("blob get multipart image: %v", err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, jpgBytes) {
		t.Fatalf("multipart blob content = %v, want %v", got, jpgBytes)
	}
	body, _ = gdocsGet(t, base+"/v1/documents/"+emojiDoc, token)
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal get: %v (body %s)", err, body)
	}
	inlineObjects = resp["inlineObjects"].(map[string]any)
	mpObj := inlineObjects[mpOID].(map[string]any)
	mpEmbedded := mpObj["inlineObjectProperties"].(map[string]any)["embeddedObject"].(map[string]any)
	if mpEmbedded["mimeType"] != "image/jpeg" {
		t.Fatalf("multipart image mimeType = %v, want image/jpeg (from part content-type)", mpEmbedded["mimeType"])
	}

	// ===== failure paths =====

	// Unknown request type → 400 INVALID_ARGUMENT (not a silent no-op).
	body, status = gdocsPost(t, base+"/v1/documents/"+emojiDoc+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{"frobnicate": map[string]any{"location": map[string]any{"index": 1}}},
		},
	})
	if status != 400 {
		t.Fatalf("unknown request type -> status %d, want 400; body %s", status, body)
	}
	if !gdocsIsInvalidArgument(t, body, "unknown request type") {
		t.Fatalf("expected unknown-request INVALID_ARGUMENT, got %s", body)
	}

	// Out-of-range index → 400.
	body, status = gdocsPost(t, base+"/v1/documents/"+emojiDoc+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"insertText": map[string]any{
					"location": map[string]any{"index": 99999},
					"text":     "nope",
				},
			},
		},
	})
	if status != 400 {
		t.Fatalf("out-of-range index -> status %d, want 400; body %s", status, body)
	}

	// Deleting the document's final newline → 400. The multipart image now
	// occupies index 1, the emoji 2–3, X index 4, so the final newline is
	// index 5 and endIndex 6 exceeds the document.
	body, status = gdocsPost(t, base+"/v1/documents/"+emojiDoc+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"deleteContentRange": map[string]any{
					"range": map[string]any{"startIndex": 5, "endIndex": 6},
				},
			},
		},
	})
	if status != 400 {
		t.Fatalf("delete final newline -> status %d, want 400; body %s", status, body)
	}

	// Invalid namedStyle → 400.
	body, status = gdocsPost(t, base+"/v1/documents/"+emojiDoc+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"updateParagraphStyle": map[string]any{
					"range":          map[string]any{"startIndex": 1, "endIndex": 4},
					"paragraphStyle": map[string]any{"namedStyle": "MEGA_TEXT"},
					"fields":         "namedStyle",
				},
			},
		},
	})
	if status != 400 {
		t.Fatalf("invalid namedStyle -> status %d, want 400; body %s", status, body)
	}
	if !gdocsIsInvalidArgument(t, body, "namedStyle") {
		t.Fatalf("expected namedStyle INVALID_ARGUMENT, got %s", body)
	}

	// Two request types in one request object → 400.
	body, status = gdocsPost(t, base+"/v1/documents/"+emojiDoc+"/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"insertText":      map[string]any{"location": map[string]any{"index": 1}, "text": "a"},
				"insertPageBreak": map[string]any{"location": map[string]any{"index": 1}},
			},
		},
	})
	if status != 400 {
		t.Fatalf("two request types -> status %d, want 400; body %s", status, body)
	}

	// batchUpdate on an unknown document → 404.
	body, status = gdocsPost(t, base+"/v1/documents/does-not-exist/batchUpdate", token, map[string]any{
		"requests": []map[string]any{
			{
				"insertText": map[string]any{
					"location": map[string]any{"index": 1},
					"text":     "x",
				},
			},
		},
	})
	if status != 404 {
		t.Fatalf("batchUpdate unknown doc -> status %d, want 404; body %s", status, body)
	}

	// ===== Revisions =====

	body, status = gdocsGet(t, base+"/v1/documents/"+docID+"/revisions", token)
	if status != 200 {
		t.Fatalf("revisions -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal revisions: %v (body %s)", err, body)
	}
	revisions, ok := resp["revisions"].([]any)
	if !ok || len(revisions) < 1 {
		t.Fatalf("revisions = %v, want non-empty array", resp["revisions"])
	}
}

// === GDocs test helpers ===

// assertRecentDocsStamp checks that v is a Docs-format timestamp (RFC3339
// with milliseconds) minted within the last 15 minutes — the adapter derives
// revision modifiedTime values from the engine clock, never a hardcoded date.
func assertRecentDocsStamp(t *testing.T, v any, what string) {
	t.Helper()
	s, ok := v.(string)
	if !ok || s == "" {
		t.Fatalf("%s = %v, want non-empty timestamp string", what, v)
	}
	ts, err := time.Parse("2006-01-02T15:04:05.000Z", s)
	if err != nil {
		t.Fatalf("%s = %q, unparsable as Docs stamp: %v", what, s, err)
	}
	if d := time.Since(ts); d < -time.Minute || d > 15*time.Minute {
		t.Fatalf("%s = %q, want within 15min of now (age %s)", what, s, d)
	}
}

func gdocsGet(t *testing.T, rawurl, token string) (string, int) {
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

func gdocsPost(t *testing.T, rawurl, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
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

// gdocsContent returns resp.body.content as []any, failing the test when the
// shape is wrong.
func gdocsContent(t *testing.T, resp map[string]any) []any {
	t.Helper()
	docBody, ok := resp["body"].(map[string]any)
	if !ok {
		t.Fatalf("body = %v, want object", resp["body"])
	}
	content, ok := docBody["content"].([]any)
	if !ok {
		t.Fatalf("body.content = %v, want array", docBody["content"])
	}
	return content
}

// gdocsParaElements returns the paragraph's elements array.
func gdocsParaElements(t *testing.T, item map[string]any) []any {
	t.Helper()
	para, ok := item["paragraph"].(map[string]any)
	if !ok {
		t.Fatalf("item %v has no paragraph", item)
	}
	els, ok := para["elements"].([]any)
	if !ok {
		t.Fatalf("paragraph %v has no elements", para)
	}
	return els
}

// gdocsParaStyle returns the paragraph's paragraphStyle.
func gdocsParaStyle(t *testing.T, item map[string]any) map[string]any {
	t.Helper()
	para, ok := item["paragraph"].(map[string]any)
	if !ok {
		t.Fatalf("item %v has no paragraph", item)
	}
	style, ok := para["paragraphStyle"].(map[string]any)
	if !ok {
		t.Fatalf("paragraph %v has no paragraphStyle", para)
	}
	return style
}

// gdocsParaText concatenates the paragraph's textRun contents.
func gdocsParaText(t *testing.T, item map[string]any) string {
	t.Helper()
	text := ""
	for _, el := range gdocsParaElements(t, item) {
		elMap, ok := el.(map[string]any)
		if !ok {
			continue
		}
		if run, ok := elMap["textRun"].(map[string]any); ok {
			text += run["content"].(string)
		}
	}
	return text
}

// gdocsDocText concatenates every paragraph's text in the document.
func gdocsDocText(t *testing.T, resp map[string]any) string {
	t.Helper()
	text := ""
	for _, item := range gdocsContent(t, resp) {
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := itemMap["paragraph"]; ok {
			text += gdocsParaText(t, itemMap)
		}
	}
	return text
}

// gdocsIsInvalidArgument asserts the error body is a Google-style 400
// INVALID_ARGUMENT whose message mentions want (case-insensitive).
func gdocsIsInvalidArgument(t *testing.T, body, want string) bool {
	t.Helper()
	var errResp struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal error body: %v (body %s)", err, body)
	}
	if errResp.Error.Status != "INVALID_ARGUMENT" {
		return false
	}
	wantLower := ""
	for _, r := range want {
		if r >= 'A' && r <= 'Z' {
			r = r + ('a' - 'A')
		}
		wantLower += string(r)
	}
	msgLower := ""
	for _, r := range errResp.Error.Message {
		if r >= 'A' && r <= 'Z' {
			r = r + ('a' - 'A')
		}
		msgLower += string(r)
	}
	return bytes.Contains([]byte(msgLower), []byte(wantLower))
}
