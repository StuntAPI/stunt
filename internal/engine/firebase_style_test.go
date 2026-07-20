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

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestFirebaseStyleAdapter exercises the firebase-style adapter end-to-end:
//
//   - signUp (v1) → {localId, idToken, refreshToken, expiresIn, email}
//   - signInWithPassword (v1) → same user signs in
//   - getAccountInfo (v1) → user info by idToken
//   - v3 verifyPassword → same flow via legacy Identity Toolkit
//   - Firestore create doc (typed values: stringValue, integerValue, booleanValue, arrayValue)
//   - Firestore get doc → returns typed values
//   - Firestore list docs → shows created doc
//   - FCM send → {name:"projects/.../messages/N"}
//   - 401 without auth
func TestFirebaseStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "firebase-style")
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
			"firebase": {Adapter: absAdapterDir},
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

	base := addrs["firebase"]
	const token = "Bearer mock-firebase-token"

	// ===== signUp (v1) → create user =====

	const signUpEmail = "testuser@stunt-test.com"
	const signUpPassword = "securePassword123"
	body, status := fbPost(t, base+"/v1/accounts:signUp", token, map[string]any{
		"email":             signUpEmail,
		"password":          signUpPassword,
		"returnSecureToken": true,
	})
	if status != 200 {
		t.Fatalf("signUp -> status %d, want 200; body %s", status, body)
	}
	var signUpResp map[string]any
	if err := json.Unmarshal([]byte(body), &signUpResp); err != nil {
		t.Fatalf("unmarshal signUp: %v (body %s)", err, body)
	}
	localID, ok := signUpResp["localId"].(string)
	if !ok || localID == "" {
		t.Fatalf("localId = %v, want non-empty string", signUpResp["localId"])
	}
	idToken, ok := signUpResp["idToken"].(string)
	if !ok || idToken == "" {
		t.Fatalf("idToken = %v, want non-empty string", signUpResp["idToken"])
	}
	refreshToken, ok := signUpResp["refreshToken"].(string)
	if !ok || refreshToken == "" {
		t.Fatalf("refreshToken = %v, want non-empty string", signUpResp["refreshToken"])
	}
	if signUpResp["expiresIn"] != "3600" {
		t.Fatalf("expiresIn = %v, want 3600", signUpResp["expiresIn"])
	}
	if signUpResp["email"] != signUpEmail {
		t.Fatalf("email = %v, want %v", signUpResp["email"], signUpEmail)
	}

	// ===== signInWithPassword (v1) → sign in with same user =====

	body, status = fbPost(t, base+"/v1/accounts:signInWithPassword", token, map[string]any{
		"email":             signUpEmail,
		"password":          signUpPassword,
		"returnSecureToken": true,
	})
	if status != 200 {
		t.Fatalf("signInWithPassword -> status %d, want 200; body %s", status, body)
	}
	var signInResp map[string]any
	if err := json.Unmarshal([]byte(body), &signInResp); err != nil {
		t.Fatalf("unmarshal signIn: %v (body %s)", err, body)
	}
	if signInResp["localId"] != localID {
		t.Fatalf("signIn localId = %v, want %v", signInResp["localId"], localID)
	}
	if signInResp["email"] != signUpEmail {
		t.Fatalf("signIn email = %v, want %v", signInResp["email"], signUpEmail)
	}

	// ===== signInWithPassword (wrong password) → error =====

	body, status = fbPost(t, base+"/v1/accounts:signInWithPassword", token, map[string]any{
		"email":    signUpEmail,
		"password": "wrong-password",
	})
	if status != 400 {
		t.Fatalf("signIn wrong password -> status %d, want 400", status)
	}

	// ===== getAccountInfo (v1) → user info =====

	body, status = fbPost(t, base+"/v1/accounts:getAccountInfo", token, map[string]any{
		"idToken": idToken,
	})
	if status != 200 {
		t.Fatalf("getAccountInfo -> status %d, want 200; body %s", status, body)
	}
	var acctInfo map[string]any
	if err := json.Unmarshal([]byte(body), &acctInfo); err != nil {
		t.Fatalf("unmarshal getAccountInfo: %v", err)
	}
	users, ok := acctInfo["users"].([]any)
	if !ok || len(users) < 1 {
		t.Fatalf("users = %v, want array with >= 1 item", acctInfo["users"])
	}
	firstUser := users[0].(map[string]any)
	if firstUser["email"] != signUpEmail {
		t.Fatalf("getAccountInfo email = %v, want %v", firstUser["email"], signUpEmail)
	}

	// ===== v3 verifyPassword (legacy) =====

	body, status = fbPost(t, base+"/identitytoolkit/v3/relyingparty/verifyPassword", token, map[string]any{
		"email":             signUpEmail,
		"password":          signUpPassword,
		"returnSecureToken": true,
	})
	if status != 200 {
		t.Fatalf("v3 verifyPassword -> status %d, want 200; body %s", status, body)
	}
	var v3Resp map[string]any
	if err := json.Unmarshal([]byte(body), &v3Resp); err != nil {
		t.Fatalf("unmarshal v3 verifyPassword: %v", err)
	}
	if v3Resp["localId"] != localID {
		t.Fatalf("v3 localId = %v, want %v", v3Resp["localId"], localID)
	}

	// ===== Firestore create doc (TYPED VALUES) =====

	const projectID = "stunt-test-project"
	const collection = "users"
	docBody := map[string]any{
		"fields": map[string]any{
			"name":    map[string]any{"stringValue": "Alice Stunt"},
			"age":     map[string]any{"integerValue": "30"},
			"active":  map[string]any{"booleanValue": true},
			"tags":    map[string]any{"arrayValue": map[string]any{"values": []any{map[string]any{"stringValue": "premium"}, map[string]any{"stringValue": "early-adopter"}}}},
			"address": map[string]any{"mapValue": map[string]any{"fields": map[string]any{"city": map[string]any{"stringValue": "San Francisco"}, "zip": map[string]any{"stringValue": "94102"}}}},
		},
	}
	docPath := base + "/v1/projects/" + projectID + "/databases/(default)/documents/" + collection
	body, status = fbPost(t, docPath, token, docBody)
	if status != 200 {
		t.Fatalf("create doc -> status %d, want 200; body %s", status, body)
	}
	var createdDoc map[string]any
	if err := json.Unmarshal([]byte(body), &createdDoc); err != nil {
		t.Fatalf("unmarshal created doc: %v (body %s)", err, body)
	}
	docName, ok := createdDoc["name"].(string)
	if !ok || docName == "" {
		t.Fatalf("doc name = %v, want non-empty string", createdDoc["name"])
	}
	if !strings.Contains(docName, "projects/"+projectID+"/databases/(default)/documents/"+collection+"/") {
		t.Fatalf("doc name = %q, want it to contain the full resource path", docName)
	}
	// Verify typed values are present.
	fields, ok := createdDoc["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields = %v, want map", createdDoc["fields"])
	}
	nameField, ok := fields["name"].(map[string]any)
	if !ok || nameField["stringValue"] != "Alice Stunt" {
		t.Fatalf("name field = %v, want stringValue:Alice Stunt", fields["name"])
	}
	ageField, ok := fields["age"].(map[string]any)
	if !ok || ageField["integerValue"] != "30" {
		t.Fatalf("age field = %v, want integerValue:30", fields["age"])
	}
	activeField, ok := fields["active"].(map[string]any)
	if !ok || activeField["booleanValue"] != true {
		t.Fatalf("active field = %v, want booleanValue:true", fields["active"])
	}
	tagsField, ok := fields["tags"].(map[string]any)
	if !ok {
		t.Fatalf("tags field = %v, want arrayValue", fields["tags"])
	}
	arrVal, ok := tagsField["arrayValue"].(map[string]any)
	if !ok {
		t.Fatalf("arrayValue = %v, want map", tagsField["arrayValue"])
	}
	arrValues, ok := arrVal["values"].([]any)
	if !ok || len(arrValues) != 2 {
		t.Fatalf("array values = %v, want 2 items", arrVal["values"])
	}

	// ===== Firestore get doc → returns typed values =====

	// Extract doc ID from the name.
	docID := docName[strings.LastIndex(docName, "/")+1:]
	getPath := base + "/v1/projects/" + projectID + "/databases/(default)/documents/" + collection + "/" + docID
	body, status = fbGet(t, getPath, token)
	if status != 200 {
		t.Fatalf("get doc -> status %d, want 200; body %s", status, body)
	}
	var gotDoc map[string]any
	if err := json.Unmarshal([]byte(body), &gotDoc); err != nil {
		t.Fatalf("unmarshal got doc: %v", err)
	}
	if gotDoc["name"] != docName {
		t.Fatalf("got doc name = %v, want %v", gotDoc["name"], docName)
	}
	gotFields, ok := gotDoc["fields"].(map[string]any)
	if !ok {
		t.Fatalf("got fields = %v", gotDoc["fields"])
	}
	gotName, ok := gotFields["name"].(map[string]any)
	if !ok || gotName["stringValue"] != "Alice Stunt" {
		t.Fatalf("got name = %v", gotFields["name"])
	}

	// ===== Firestore list docs → shows created doc =====

	body, status = fbGet(t, docPath, token)
	if status != 200 {
		t.Fatalf("list docs -> status %d, want 200; body %s", status, body)
	}
	var docList map[string]any
	if err := json.Unmarshal([]byte(body), &docList); err != nil {
		t.Fatalf("unmarshal doc list: %v", err)
	}
	docs, ok := docList["documents"].([]any)
	if !ok {
		t.Fatalf("documents = %v, want array", docList["documents"])
	}
	foundDoc := false
	for _, d := range docs {
		dm := d.(map[string]any)
		if dm["name"] == docName {
			foundDoc = true
		}
	}
	if !foundDoc {
		t.Fatalf("created doc not found in list (STATEFUL)")
	}

	// ===== FCM send → {name} =====

	fcmBody := map[string]any{
		"message": map[string]any{
			"token": "device-registration-token-123",
			"notification": map[string]any{
				"title": "Test Notification",
				"body":  "This is a test push from stunt",
			},
			"data": map[string]any{
				"orderId": "order-12345",
			},
		},
	}
	fcmPath := base + "/v1/projects/" + projectID + "/messages:send"
	body, status = fbPost(t, fcmPath, token, fcmBody)
	if status != 200 {
		t.Fatalf("FCM send -> status %d, want 200; body %s", status, body)
	}
	var fcmResp map[string]any
	if err := json.Unmarshal([]byte(body), &fcmResp); err != nil {
		t.Fatalf("unmarshal FCM: %v", err)
	}
	fcmName, ok := fcmResp["name"].(string)
	if !ok || fcmName == "" {
		t.Fatalf("FCM name = %v, want non-empty string", fcmResp["name"])
	}
	if !strings.Contains(fcmName, "projects/"+projectID+"/messages/") {
		t.Fatalf("FCM name = %q, want it to contain projects/%s/messages/", fcmName, projectID)
	}

	// ===== 401 without auth =====

	body, status = fbPostNoAuth(t, base+"/v1/accounts:signUp", map[string]any{
		"email":    "noauth@test.com",
		"password": "password",
	})
	if status != 401 {
		t.Fatalf("signUp without auth -> status %d, want 401; body %s", status, body)
	}
	var err401 map[string]any
	if err := json.Unmarshal([]byte(body), &err401); err != nil {
		t.Fatalf("unmarshal 401 body: %v", err)
	}
	errObj, ok := err401["error"].(map[string]any)
	if !ok {
		t.Fatalf("401 error = %v, want object", err401["error"])
	}
	if _, ok := errObj["status"].(string); !ok {
		t.Fatalf("error.status = %v, want string", errObj["status"])
	}

	// ===== Firestore without auth → 401 =====

	body, status = fbGetNoAuth(t, docPath)
	if status != 401 {
		t.Fatalf("Firestore without auth -> status %d, want 401", status)
	}

	// ===== Refresh token (v3) =====

	body, status = fbPost(t, base+"/identitytoolkit/v3/relyingparty/refreshToken", token, map[string]any{
		"refreshToken": refreshToken,
	})
	if status != 200 {
		t.Fatalf("refresh -> status %d, want 200; body %s", status, body)
	}
	var refreshResp map[string]any
	if err := json.Unmarshal([]byte(body), &refreshResp); err != nil {
		t.Fatalf("unmarshal refresh: %v", err)
	}
	newIDToken, ok := refreshResp["id_token"].(string)
	if !ok || newIDToken == "" {
		t.Fatalf("refresh id_token = %v, want non-empty string", refreshResp["id_token"])
	}
}

// === Firebase test helpers ===

func fbGet(t *testing.T, rawurl, authHeader string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", authHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func fbGetNoAuth(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func fbPost(t *testing.T, rawurl, authHeader string, body map[string]any) (string, int) {
	t.Helper()
	resp := fbPostRaw(t, rawurl, authHeader, body)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func fbPostNoAuth(t *testing.T, rawurl string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func fbPostRaw(t *testing.T, rawurl, authHeader string, body map[string]any) *http.Response {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
