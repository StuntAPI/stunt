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

	"stuntapi.com/stunt/internal/manifest"
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

	// ===== second user: getAccountInfo binds the token to ITS user =====

	const secondEmail = "seconduser@stunt-test.com"
	body, status = fbPost(t, base+"/v1/accounts:signUp", token, map[string]any{
		"email":             secondEmail,
		"password":          "secondPassword456",
		"returnSecureToken": true,
	})
	if status != 200 {
		t.Fatalf("signUp second -> status %d, want 200; body %s", status, body)
	}
	var secondResp map[string]any
	if err := json.Unmarshal([]byte(body), &secondResp); err != nil {
		t.Fatalf("unmarshal signUp second: %v (body %s)", err, body)
	}
	secondIDToken, ok := secondResp["idToken"].(string)
	if !ok || secondIDToken == "" {
		t.Fatalf("second idToken = %v, want non-empty string", secondResp["idToken"])
	}

	body, status = fbPost(t, base+"/v1/accounts:getAccountInfo", token, map[string]any{
		"idToken": secondIDToken,
	})
	if status != 200 {
		t.Fatalf("getAccountInfo (second user) -> status %d, want 200; body %s", status, body)
	}
	var secondAcct map[string]any
	if err := json.Unmarshal([]byte(body), &secondAcct); err != nil {
		t.Fatalf("unmarshal getAccountInfo second: %v (body %s)", err, body)
	}
	secondUsers, ok := secondAcct["users"].([]any)
	if !ok || len(secondUsers) != 1 {
		t.Fatalf("second users = %v, want 1", secondAcct["users"])
	}
	if secondUsers[0].(map[string]any)["email"] != secondEmail {
		t.Fatalf("second getAccountInfo email = %v, want %v (token must bind to ITS user)",
			secondUsers[0].(map[string]any)["email"], secondEmail)
	}

	// ===== getAccountInfo with an unknown idToken -> 401 =====

	body, status = fbPost(t, base+"/v1/accounts:getAccountInfo", token, map[string]any{
		"idToken": "totally-unknown-id-token",
	})
	if status != 401 {
		t.Fatalf("getAccountInfo unknown token -> status %d, want 401; body %s", status, body)
	}

	// ===== securetoken: POST /v1/token grant_type=refresh_token exchange =====

	body, status = fbPostForm(t, base+"/v1/token", token, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
	if status != 200 {
		t.Fatalf("securetoken exchange -> status %d, want 200; body %s", status, body)
	}
	var stResp map[string]any
	if err := json.Unmarshal([]byte(body), &stResp); err != nil {
		t.Fatalf("unmarshal securetoken: %v (body %s)", err, body)
	}
	stIDToken, ok := stResp["id_token"].(string)
	if !ok || stIDToken == "" {
		t.Fatalf("securetoken id_token = %v, want non-empty string", stResp["id_token"])
	}
	if stResp["access_token"] != stIDToken {
		t.Fatalf("securetoken access_token = %v, want to match id_token", stResp["access_token"])
	}
	if stResp["expires_in"] != "3600" {
		t.Fatalf("securetoken expires_in = %v, want 3600", stResp["expires_in"])
	}
	if stResp["token_type"] != "Bearer" {
		t.Fatalf("securetoken token_type = %v, want Bearer", stResp["token_type"])
	}
	if stResp["user_id"] != localID {
		t.Fatalf("securetoken user_id = %v, want %v", stResp["user_id"], localID)
	}
	if _, ok := stResp["refresh_token"].(string); !ok {
		t.Fatalf("securetoken refresh_token = %v, want string", stResp["refresh_token"])
	}

	// The freshly minted id_token resolves to the SAME user via the binding.
	body, status = fbPost(t, base+"/v1/accounts:getAccountInfo", token, map[string]any{
		"idToken": stIDToken,
	})
	if status != 200 {
		t.Fatalf("getAccountInfo (securetoken id) -> status %d, want 200; body %s", status, body)
	}
	var stAcct map[string]any
	if err := json.Unmarshal([]byte(body), &stAcct); err != nil {
		t.Fatalf("unmarshal getAccountInfo securetoken: %v (body %s)", err, body)
	}
	stUsers := stAcct["users"].([]any)
	if stUsers[0].(map[string]any)["email"] != signUpEmail {
		t.Fatalf("securetoken user email = %v, want %v", stUsers[0].(map[string]any)["email"], signUpEmail)
	}

	// ===== securetoken failure paths =====

	body, status = fbPostForm(t, base+"/v1/token", token, url.Values{
		"grant_type":    {"password"},
		"refresh_token": {refreshToken},
	})
	if status != 400 {
		t.Fatalf("securetoken bad grant_type -> status %d, want 400; body %s", status, body)
	}

	body, status = fbPostForm(t, base+"/v1/token", token, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"not-a-real-refresh-token"},
	})
	if status != 400 {
		t.Fatalf("securetoken bad refresh token -> status %d, want 400; body %s", status, body)
	}

	// ===== Firestore: POST with ?documentId= =====

	peoplePath := base + "/v1/projects/" + projectID + "/databases/(default)/documents/people"
	body, status = fbPost(t, peoplePath+"?documentId=carol", token, map[string]any{
		"fields": map[string]any{
			"name": map[string]any{"stringValue": "Carol Creator"},
			"age":  map[string]any{"integerValue": "50"},
		},
	})
	if status != 200 {
		t.Fatalf("create doc with documentId -> status %d, want 200; body %s", status, body)
	}
	var carolDoc map[string]any
	if err := json.Unmarshal([]byte(body), &carolDoc); err != nil {
		t.Fatalf("unmarshal carol doc: %v (body %s)", err, body)
	}
	if carolDoc["name"] != "projects/"+projectID+"/databases/(default)/documents/people/carol" {
		t.Fatalf("carol name = %v, want explicit documentId path", carolDoc["name"])
	}

	// Reusing the same documentId -> 409 ALREADY_EXISTS.
	body, status = fbPost(t, peoplePath+"?documentId=carol", token, map[string]any{
		"fields": map[string]any{
			"name": map[string]any{"stringValue": "Duplicate"},
		},
	})
	if status != 409 {
		t.Fatalf("duplicate documentId -> status %d, want 409; body %s", status, body)
	}

	// ===== Firestore: create docs for runQuery =====

	for _, p := range []struct {
		name string
		age  string
	}{{"Alice Query", "30"}, {"Bob Query", "40"}} {
		body, status = fbPost(t, peoplePath, token, map[string]any{
			"fields": map[string]any{
				"name": map[string]any{"stringValue": p.name},
				"age":  map[string]any{"integerValue": p.age},
			},
		})
		if status != 200 {
			t.Fatalf("create runQuery doc %s -> status %d, want 200; body %s", p.name, status, body)
		}
	}

	// ===== Firestore: documents:runQuery (from + where + orderBy + limit) =====

	runQueryPath := base + "/v1/projects/" + projectID + "/databases/(default)/documents:runQuery"
	body, status = fbPost(t, runQueryPath, token, map[string]any{
		"structuredQuery": map[string]any{
			"from": []any{map[string]any{"collectionId": "people"}},
			"where": map[string]any{
				"fieldFilter": map[string]any{
					"field": map[string]any{"fieldPath": "age"},
					"op":    "GREATER_THAN",
					"value": map[string]any{"integerValue": "25"},
				},
			},
			"orderBy": []any{map[string]any{
				"field":     map[string]any{"fieldPath": "age"},
				"direction": "DESCENDING",
			}},
			"limit": 2,
		},
	})
	if status != 200 {
		t.Fatalf("runQuery -> status %d, want 200; body %s", status, body)
	}
	var runResp []map[string]any
	if err := json.Unmarshal([]byte(body), &runResp); err != nil {
		t.Fatalf("unmarshal runQuery: %v (body %s)", err, body)
	}
	if len(runResp) != 2 {
		t.Fatalf("runQuery returned %d docs, want 2 (limit); body %s", len(runResp), body)
	}
	firstDoc, ok := runResp[0]["document"].(map[string]any)
	if !ok {
		t.Fatalf("runQuery[0].document = %v, want object", runResp[0])
	}
	firstAge, ok := firstDoc["fields"].(map[string]any)["age"].(map[string]any)["integerValue"]
	if !ok || firstAge != "50" {
		t.Fatalf("runQuery[0] age = %v, want 50 (DESCENDING order)", firstDoc["fields"])
	}
	if !strings.HasSuffix(firstDoc["name"].(string), "/people/carol") {
		t.Fatalf("runQuery[0].name = %v, want carol (age 50 first)", firstDoc["name"])
	}

	// ===== Firestore: runQuery failure path (missing structuredQuery) =====

	body, status = fbPost(t, runQueryPath, token, map[string]any{})
	if status != 400 {
		t.Fatalf("runQuery missing structuredQuery -> status %d, want 400; body %s", status, body)
	}

	// ===== Firestore: runQuery failure path (unsupported op) =====

	body, status = fbPost(t, runQueryPath, token, map[string]any{
		"structuredQuery": map[string]any{
			"from": []any{map[string]any{"collectionId": "people"}},
			"where": map[string]any{
				"fieldFilter": map[string]any{
					"field": map[string]any{"fieldPath": "age"},
					"op":    "SOMETHING_WEIRD",
					"value": map[string]any{"integerValue": "1"},
				},
			},
		},
	})
	if status != 400 {
		t.Fatalf("runQuery unsupported op -> status %d, want 400; body %s", status, body)
	}

	// ===== Firestore: nested collection path (subcollection under a doc) =====

	nestedPath := peoplePath + "/carol/addresses"
	body, status = fbPost(t, nestedPath, token, map[string]any{
		"fields": map[string]any{
			"city": map[string]any{"stringValue": "San Francisco"},
		},
	})
	if status != 200 {
		t.Fatalf("create nested doc -> status %d, want 200; body %s", status, body)
	}
	var nestedDoc map[string]any
	if err := json.Unmarshal([]byte(body), &nestedDoc); err != nil {
		t.Fatalf("unmarshal nested doc: %v (body %s)", err, body)
	}
	nestedName, ok := nestedDoc["name"].(string)
	if !ok || !strings.HasPrefix(nestedName, "projects/"+projectID+"/databases/(default)/documents/people/carol/addresses/") {
		t.Fatalf("nested doc name = %v, want full nested resource path", nestedDoc["name"])
	}

	// List the subcollection and find the doc back.
	body, status = fbGet(t, nestedPath, token)
	if status != 200 {
		t.Fatalf("list nested docs -> status %d, want 200; body %s", status, body)
	}
	var nestedList map[string]any
	if err := json.Unmarshal([]byte(body), &nestedList); err != nil {
		t.Fatalf("unmarshal nested list: %v (body %s)", err, body)
	}
	nestedDocs, ok := nestedList["documents"].([]any)
	if !ok || len(nestedDocs) != 1 {
		t.Fatalf("nested documents = %v, want 1", nestedList["documents"])
	}
	if nestedDocs[0].(map[string]any)["name"] != nestedName {
		t.Fatalf("nested list name = %v, want %v", nestedDocs[0], nestedName)
	}

	// ===== FCM: subscribe tokens to topics (simulator extension) =====

	subscribePath := base + "/v1/projects/" + projectID + "/topics/"
	body, status = fbPost(t, subscribePath+"news:subscribe", token, map[string]any{
		"tokens": []string{"device-token-a", "device-token-b"},
	})
	if status != 200 {
		t.Fatalf("subscribe news -> status %d, want 200; body %s", status, body)
	}
	body, status = fbPost(t, subscribePath+"sports:subscribe", token, map[string]any{
		"token": "device-token-c",
	})
	if status != 200 {
		t.Fatalf("subscribe sports -> status %d, want 200; body %s", status, body)
	}

	// ===== FCM: send by topic -> fanout to subscribed tokens =====

	body, status = fbPost(t, fcmPath, token, map[string]any{
		"message": map[string]any{
			"topic": "news",
			"notification": map[string]any{
				"title": "News flash",
			},
		},
	})
	if status != 200 {
		t.Fatalf("FCM topic send -> status %d, want 200; body %s", status, body)
	}

	// ===== FCM: send by condition ('news' in topics || 'sports' in topics) =====

	body, status = fbPost(t, fcmPath, token, map[string]any{
		"message": map[string]any{
			"condition": "'news' in topics || 'sports' in topics",
			"notification": map[string]any{
				"title": "Condition blast",
			},
		},
	})
	if status != 200 {
		t.Fatalf("FCM condition send -> status %d, want 200; body %s", status, body)
	}

	// Verify fanout via the message list (simulator extension).
	body, status = fbGet(t, base+"/v1/projects/"+projectID+"/messages", token)
	if status != 200 {
		t.Fatalf("list messages -> status %d, want 200; body %s", status, body)
	}
	var msgList map[string]any
	if err := json.Unmarshal([]byte(body), &msgList); err != nil {
		t.Fatalf("unmarshal message list: %v (body %s)", err, body)
	}
	messages, ok := msgList["messages"].([]any)
	if !ok {
		t.Fatalf("messages = %v, want array", msgList["messages"])
	}
	topicFanout, condFanout := -1, -1
	for _, m := range messages {
		mm := m.(map[string]any)
		if mm["topic"] == "news" {
			recips, _ := mm["recipients"].([]any)
			topicFanout = len(recips)
		}
		if mm["condition"] != "" && mm["condition"] != nil {
			recips, _ := mm["recipients"].([]any)
			condFanout = len(recips)
		}
	}
	if topicFanout != 2 {
		t.Fatalf("topic fanout = %d recipients, want 2 (device-token-a, device-token-b)", topicFanout)
	}
	if condFanout != 3 {
		t.Fatalf("condition fanout = %d recipients, want 3 (a, b, c)", condFanout)
	}

	// ===== FCM: send with no target -> 400 =====

	body, status = fbPost(t, fcmPath, token, map[string]any{
		"message": map[string]any{
			"notification": map[string]any{"title": "Nowhere"},
		},
	})
	if status != 400 {
		t.Fatalf("FCM no target -> status %d, want 400; body %s", status, body)
	}

	// ===== FCM: send with multiple targets -> 400 =====

	body, status = fbPost(t, fcmPath, token, map[string]any{
		"message": map[string]any{
			"token": "device-token-a",
			"topic": "news",
		},
	})
	if status != 400 {
		t.Fatalf("FCM multiple targets -> status %d, want 400; body %s", status, body)
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

// fbPostForm sends a form-encoded POST (application/x-www-form-urlencoded),
// like the real securetoken /v1/token grant.
func fbPostForm(t *testing.T, rawurl, authHeader string, form url.Values) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", rawurl, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", authHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
