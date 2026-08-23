package adapters

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// These tests drive the firebase-style adapter scripts directly (lib.star
// preloaded) over a shared store and a VIRTUAL clock, covering what the
// HTTP-level engine test cannot reach without waiting: Identity Toolkit
// signUp/signInWithPassword/getAccountInfo token flows (token -> user
// bindings, the real error codes EMAIL_EXISTS / EMAIL_NOT_FOUND /
// INVALID_PASSWORD, the 1h idToken expiry), the v3 relyingparty and
// securetoken refresh grants, Firestore's typed-value document CRUD
// (create/get/patch-merge/delete, per-collection documentIds, cursor
// paging, subcollections, runQuery), and FCM target validation with
// topic/condition fanout.

const (
	firebaseHost = "identitytoolkit.googleapis.test"
	firebaseAuth = "Bearer mock-firebase-credential"
	fbProject    = "stunt-test-project"
	fbDocBase    = "/v1/projects/" + fbProject + "/databases/(default)/documents"
	fbResBase    = "projects/" + fbProject + "/databases/(default)/documents"
)

// firebaseFixture is one shared store + virtual clock with a loaded VM per
// handler script (auth.star, firestore.star, and fcm.star each need their
// own VM, but they observe the same collections/kv state, like the engine).
type firebaseFixture struct {
	t   *testing.T
	vc  *clock.Clock
	vms map[string]*starlark.VM
}

func newFirebaseFixture(t *testing.T, start time.Time) *firebaseFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "firebase-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}
	tmp := t.TempDir()
	store, _ := primitives.Open(filepath.Join(tmp, "f.db"))
	t.Cleanup(func() { store.Close() })
	kvStore, _ := kv.Open(filepath.Join(tmp, "f.kv.db"))
	t.Cleanup(func() { kvStore.Close() })
	blobStore, _ := blob.Open(filepath.Join(tmp, "blobs"))
	t.Cleanup(func() { blobStore.Close() })

	vc := clock.NewVirtualClock(start)
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: vc, ServiceName: "test",
	})
	load := func(script string) *starlark.VM {
		t.Helper()
		src, err := os.ReadFile(filepath.Join(root, "scripts", script))
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
		if err != nil {
			t.Fatalf("LoadWithLib %s: %v", script, err)
		}
		return vm
	}
	return &firebaseFixture{t: t, vc: vc, vms: map[string]*starlark.VM{
		"auth": load("auth.star"), "firestore": load("firestore.star"), "fcm": load("fcm.star"),
	}}
}

// call invokes a firebase handler. auth is the raw Authorization header
// value ("" exercises the 401 path); params carries the {route} captures
// the engine normally extracts.
func (f *firebaseFixture) call(group, handler, method, path string, params, query map[string]string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: firebaseHost, Headers: headers,
		Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// fbErr digs out the Firebase error envelope {error:{code,message,status}}.
func fbErr(t *testing.T, r starlark.Response) map[string]any {
	t.Helper()
	e, ok := r.Body["error"].(map[string]any)
	if !ok {
		t.Fatalf("status %d has no error envelope: %v", r.Status, r.Body)
	}
	return e
}

// Firestore typed-value wrappers: every field travels in a type-keyed object.
func fbStr(s string) map[string]any { return map[string]any{"stringValue": s} }
func fbInt(s string) map[string]any { return map[string]any{"integerValue": s} }
func fbBool(b bool) map[string]any  { return map[string]any{"booleanValue": b} }

func fbArr(vs ...map[string]any) map[string]any {
	wrapped := make([]any, 0, len(vs))
	for _, v := range vs {
		wrapped = append(wrapped, v)
	}
	return map[string]any{"arrayValue": map[string]any{"values": wrapped}}
}

// fbCreateDoc POSTs a typed-fields document into a top-level collection,
// optionally with an explicit documentId (query param when idViaQuery,
// body field otherwise).
func (f *firebaseFixture) fbCreateDoc(collection, documentID string, idViaQuery bool, fields map[string]any) starlark.Response {
	f.t.Helper()
	query := map[string]string{}
	body := map[string]any{"fields": fields}
	if documentID != "" {
		if idViaQuery {
			query["documentId"] = documentID
		} else {
			body["documentId"] = documentID
		}
	}
	return f.call("firestore", "on_create_document", "POST", fbDocBase+"/"+collection,
		map[string]string{"project": fbProject, "collection": collection}, query, body, firebaseAuth)
}

// fbGetDoc GETs one document by collection + id.
func (f *firebaseFixture) fbGetDoc(collection, id string) starlark.Response {
	f.t.Helper()
	return f.call("firestore", "on_get_document", "GET", fbDocBase+"/"+collection+"/"+id,
		map[string]string{"project": fbProject, "collection": collection, "id": id}, nil, nil, firebaseAuth)
}

// fbTypedField digs the typed wrapper of one field out of a document body.
func fbTypedField(t *testing.T, doc map[string]any, key string) map[string]any {
	t.Helper()
	fields, ok := doc["fields"].(map[string]any)
	if !ok {
		t.Fatalf("document has no fields: %v", doc)
	}
	v, ok := fields[key].(map[string]any)
	if !ok {
		t.Fatalf("field %q missing or not a typed value: %v", key, fields[key])
	}
	return v
}

// fbRecipients extracts the resolved recipient tokens of a stored message.
func fbRecipients(t *testing.T, msg map[string]any) []string {
	t.Helper()
	raw, ok := msg["recipients"].([]any)
	if !ok {
		t.Fatalf("message has no recipients: %v", msg)
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		s, _ := r.(string)
		out = append(out, s)
	}
	return out
}

// fbFindMessage locates a stored message by an exact field value.
func fbFindMessage(t *testing.T, r starlark.Response, field, value string) map[string]any {
	t.Helper()
	msgs, ok := r.Body["messages"].([]any)
	if !ok {
		t.Fatalf("message list has no messages array: %v", r.Body)
	}
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm[field] == value {
			return mm
		}
	}
	t.Fatalf("no stored message with %s=%q in %d messages", field, value, len(msgs))
	return nil
}

// TestFirebaseAuthSignUpSignInFlows: the Identity Toolkit v1 surface —
// signUp mints the SignupNewUserResponse token set, duplicate and email-less
// signUps answer the real error codes, signInWithPassword round-trips the
// user, signInWithIdp provisions a verified federated user, the v3
// relyingparty dispatcher mirrors verifyPassword, and the Bearer-or-key
// credential gate.
func TestFirebaseAuthSignUpSignInFlows(t *testing.T) {
	f := newFirebaseFixture(t, time.Unix(1_750_000_000, 0).UTC())

	// ===== signUp mints the Identity Toolkit token set with a padded localId =====
	// signUp answers the SignupNewUserResponse kind; ids are padded sequences.
	up := f.call("auth", "on_sign_up", "POST", "/v1/accounts:signUp", nil, nil, map[string]any{
		"email": "ada@example.test", "password": "Secret123!", "displayName": "Ada",
	}, firebaseAuth)
	if up.Status != 200 {
		t.Fatalf("signUp -> %d: %v", up.Status, up.Body)
	}
	if up.Body["kind"] != "identitytoolkit#SignupNewUserResponse" {
		t.Fatalf("signUp kind = %v, want identitytoolkit#SignupNewUserResponse", up.Body["kind"])
	}
	if up.Body["localId"] != "firebase-user-000001" {
		t.Fatalf("signUp localId = %v, want padded firebase-user-000001", up.Body["localId"])
	}
	if up.Body["idToken"] == "" || up.Body["refreshToken"] == "" {
		t.Fatalf("signUp returned no token pair: %v", up.Body)
	}
	if up.Body["expiresIn"] != "3600" || up.Body["email"] != "ada@example.test" {
		t.Fatalf("signUp envelope = %v", up.Body)
	}

	// ===== duplicate and email-less signUps are Firebase-coded 400s =====
	dup := f.call("auth", "on_sign_up", "POST", "/v1/accounts:signUp", nil, nil, map[string]any{
		"email": "ada@example.test", "password": "Other123!",
	}, firebaseAuth)
	if dup.Status != 400 {
		t.Fatalf("duplicate signUp -> %d, want 400", dup.Status)
	}
	if e := fbErr(t, dup); e["message"] != "EMAIL_EXISTS" || e["status"] != "ALREADY_EXISTS" || e["code"] != int64(400) {
		t.Fatalf("duplicate signUp envelope = %v, want EMAIL_EXISTS/ALREADY_EXISTS", e)
	}
	noEmail := f.call("auth", "on_sign_up", "POST", "/v1/accounts:signUp", nil, nil,
		map[string]any{"password": "NoEmail123!"}, firebaseAuth)
	if noEmail.Status != 400 || fbErr(t, noEmail)["message"] != "MISSING_EMAIL" {
		t.Fatalf("email-less signUp -> %d %v, want 400 MISSING_EMAIL", noEmail.Status, noEmail.Body)
	}

	// ===== password sign-in round-trips the user; bad credentials answer the real codes =====
	in := f.call("auth", "on_sign_in_with_password", "POST", "/v1/accounts:signInWithPassword", nil, nil, map[string]any{
		"email": "ada@example.test", "password": "Secret123!", "returnSecureToken": true,
	}, firebaseAuth)
	if in.Status != 200 {
		t.Fatalf("signInWithPassword -> %d: %v", in.Status, in.Body)
	}
	if in.Body["localId"] != up.Body["localId"] || in.Body["kind"] != "identitytoolkit#VerifyPasswordResponse" {
		t.Fatalf("signIn response = %v, want the signUp user and VerifyPasswordResponse kind", in.Body)
	}
	if in.Body["registered"] != true || in.Body["idToken"] == "" {
		t.Fatalf("signIn envelope = %v, want registered:true and a fresh idToken", in.Body)
	}
	if in.Body["idToken"] == up.Body["idToken"] {
		t.Fatal("signIn reused the signUp idToken; each issuance must mint a new one")
	}

	badPw := f.call("auth", "on_sign_in_with_password", "POST", "/v1/accounts:signInWithPassword", nil, nil, map[string]any{
		"email": "ada@example.test", "password": "wrong-password",
	}, firebaseAuth)
	if badPw.Status != 400 || fbErr(t, badPw)["message"] != "INVALID_PASSWORD" {
		t.Fatalf("wrong password -> %d %v, want 400 INVALID_PASSWORD", badPw.Status, badPw.Body)
	}
	noUser := f.call("auth", "on_sign_in_with_password", "POST", "/v1/accounts:signInWithPassword", nil, nil, map[string]any{
		"email": "nobody@example.test", "password": "Secret123!",
	}, firebaseAuth)
	if noUser.Status != 400 || fbErr(t, noUser)["message"] != "EMAIL_NOT_FOUND" {
		t.Fatalf("unknown email -> %d %v, want 400 EMAIL_NOT_FOUND", noUser.Status, noUser.Body)
	}
	bare := f.call("auth", "on_sign_in_with_password", "POST", "/v1/accounts:signInWithPassword",
		nil, nil, nil, firebaseAuth)
	if bare.Status != 400 {
		t.Fatalf("empty sign-in -> %d, want 400 (MISSING_EMAIL_OR_PASSWORD)", bare.Status)
	}

	// ===== signInWithIdp provisions a verified Google user =====
	idp := f.call("auth", "on_sign_in_with_idp", "POST", "/v1/accounts:signInWithIdp", nil, nil, map[string]any{
		"postBody": "id_token=google-id-token", "requestUri": "https://firebase.google.test/__/auth",
	}, firebaseAuth)
	if idp.Status != 200 {
		t.Fatalf("signInWithIdp -> %d: %v", idp.Status, idp.Body)
	}
	if idp.Body["kind"] != "identitytoolkit#VerifyAssertionResponse" {
		t.Fatalf("signInWithIdp kind = %v, want VerifyAssertionResponse", idp.Body["kind"])
	}
	prov, ok := idp.Body["providerUserInfo"].([]any)
	if !ok || len(prov) != 1 || prov[0].(map[string]any)["providerId"] != "google.com" {
		t.Fatalf("signInWithIdp providerUserInfo = %v, want google.com assertion", idp.Body["providerUserInfo"])
	}
	idpInfo := f.call("auth", "on_get_account_info", "POST", "/v1/accounts:getAccountInfo", nil, nil,
		map[string]any{"idToken": idp.Body["idToken"]}, firebaseAuth)
	idpUsers := idpInfo.Body["users"].([]any)
	if idpInfo.Status != 200 || idpUsers[0].(map[string]any)["emailVerified"] != true {
		t.Fatalf("idp user lookup = %d %v, want emailVerified:true", idpInfo.Status, idpInfo.Body)
	}

	// ===== the v3 relyingparty dispatcher mirrors verifyPassword and 404s unknown actions =====
	v3 := f.call("auth", "on_relyingparty", "POST", "/identitytoolkit/v3/relyingparty/verifyPassword",
		map[string]string{"action": "verifyPassword"}, nil, map[string]any{
			"email": "ada@example.test", "password": "Secret123!",
		}, firebaseAuth)
	if v3.Status != 200 || v3.Body["localId"] != up.Body["localId"] ||
		v3.Body["kind"] != "identitytoolkit#VerifyPasswordResponse" {
		t.Fatalf("v3 verifyPassword = %d %v, want the same user and kind", v3.Status, v3.Body)
	}
	unknown := f.call("auth", "on_relyingparty", "POST", "/identitytoolkit/v3/relyingparty/frobnicate",
		map[string]string{"action": "frobnicate"}, nil, map[string]any{}, firebaseAuth)
	if unknown.Status != 404 || !strings.Contains(fbErr(t, unknown)["message"].(string), "Unknown relyingparty action") {
		t.Fatalf("unknown v3 action -> %d %v, want 404", unknown.Status, unknown.Body)
	}

	// ===== endpoints accept Bearer or key and reject anonymous requests 401 =====
	anon := f.call("auth", "on_sign_up", "POST", "/v1/accounts:signUp", nil, nil, map[string]any{
		"email": "anon@example.test", "password": "AnonPass1!",
	}, "")
	if anon.Status != 401 {
		t.Fatalf("anonymous signUp -> %d, want 401", anon.Status)
	}
	if e := fbErr(t, anon); e["status"] != "UNAUTHENTICATED" || e["code"] != int64(401) {
		t.Fatalf("401 envelope = %v, want UNAUTHENTICATED", e)
	}
	keyed := f.call("auth", "on_sign_up", "POST", "/v1/accounts:signUp", nil,
		map[string]string{"key": "AIzaSyMockWebApiKey"}, map[string]any{
			"email": "grace@example.test", "password": "Grace123!",
		}, "")
	if keyed.Status != 200 {
		t.Fatalf("key-auth signUp -> %d: %v (key must stand in for Bearer)", keyed.Status, keyed.Body)
	}
}

// TestFirebaseIdTokenBindingAndExpiry: every issued idToken resolves
// getAccountInfo to the user it was minted for (never a fallback user),
// unknown tokens are 401 INVALID_ID_TOKEN, and the token dies at its 1h
// expiry while the refresh token survives it — all on the virtual clock.
func TestFirebaseIdTokenBindingAndExpiry(t *testing.T) {
	f := newFirebaseFixture(t, time.Unix(1_750_000_000, 0).UTC())

	signUp := func(email, name string) map[string]any {
		r := f.call("auth", "on_sign_up", "POST", "/v1/accounts:signUp", nil, nil, map[string]any{
			"email": email, "password": "Secret123!", "displayName": name,
		}, firebaseAuth)
		if r.Status != 200 {
			t.Fatalf("signUp %s -> %d: %v", email, r.Status, r.Body)
		}
		return r.Body
	}
	lookup := func(idToken string) starlark.Response {
		return f.call("auth", "on_get_account_info", "POST", "/v1/accounts:lookup", nil, nil,
			map[string]any{"idToken": idToken}, firebaseAuth)
	}

	ada := signUp("ada@example.test", "Ada")
	grace := signUp("grace@example.test", "Grace")

	// ===== getAccountInfo resolves each idToken to the user it was minted for =====
	adaInfo := lookup(ada["idToken"].(string))
	if adaInfo.Status != 200 {
		t.Fatalf("lookup ada -> %d: %v", adaInfo.Status, adaInfo.Body)
	}
	users := adaInfo.Body["users"].([]any)
	if len(users) != 1 {
		t.Fatalf("lookup returned %d users, want exactly 1", len(users))
	}
	au := users[0].(map[string]any)
	if au["localId"] != ada["localId"] || au["email"] != "ada@example.test" || au["displayName"] != "Ada" {
		t.Fatalf("ada lookup = %v, want HER localId/email/displayName", au)
	}
	if au["emailVerified"] != false {
		t.Fatalf("password signUp emailVerified = %v, want false", au["emailVerified"])
	}
	graceInfo := lookup(grace["idToken"].(string))
	gu := graceInfo.Body["users"].([]any)[0].(map[string]any)
	if gu["localId"] != grace["localId"] || gu["email"] != "grace@example.test" {
		t.Fatalf("grace lookup = %v, want HER user (no first-user fallback)", gu)
	}
	v3 := f.call("auth", "on_relyingparty", "POST", "/identitytoolkit/v3/relyingparty/getAccountInfo",
		map[string]string{"action": "getAccountInfo"}, nil,
		map[string]any{"idToken": grace["idToken"]}, firebaseAuth)
	if v3.Status != 200 || v3.Body["users"].([]any)[0].(map[string]any)["email"] != "grace@example.test" {
		t.Fatalf("v3 getAccountInfo = %d %v, want grace via the same binding", v3.Status, v3.Body)
	}

	// ===== unknown and empty idTokens are 401 INVALID_ID_TOKEN =====
	for _, tok := range []string{"totally-unknown-token", ""} {
		r := lookup(tok)
		if r.Status != 401 {
			t.Fatalf("lookup token %q -> %d, want 401", tok, r.Status)
		}
		if e := fbErr(t, r); e["status"] != "UNAUTHENTICATED" ||
			!strings.Contains(e["message"].(string), "INVALID_ID_TOKEN") {
			t.Fatalf("bad-token envelope = %v, want INVALID_ID_TOKEN/UNAUTHENTICATED", e)
		}
	}

	// ===== the idToken dies at 1h while its refresh token survives =====
	f.vc.Advance(3601 * time.Second)
	if r := lookup(ada["idToken"].(string)); r.Status != 401 {
		t.Fatalf("lookup after 1h -> %d, want 401 (expired)", r.Status)
	}
	rt := f.call("auth", "on_relyingparty", "POST", "/identitytoolkit/v3/relyingparty/refreshToken",
		map[string]string{"action": "refreshToken"}, nil,
		map[string]any{"refreshToken": ada["refreshToken"]}, firebaseAuth)
	if rt.Status != 200 {
		t.Fatalf("refresh after idToken expiry -> %d: %v (refresh tokens are long-lived)", rt.Status, rt.Body)
	}
	fresh := lookup(rt.Body["id_token"].(string))
	if fresh.Status != 200 || fresh.Body["users"].([]any)[0].(map[string]any)["email"] != "ada@example.test" {
		t.Fatalf("refreshed idToken lookup = %d %v, want ada", fresh.Status, fresh.Body)
	}
}

// TestFirebaseRefreshTokenGrants: the v3 refreshToken exchange and the
// securetoken POST /v1/token grant both rotate id/refresh tokens for the
// user the refresh token is bound to, and reject unknown or malformed
// requests with 400s.
func TestFirebaseRefreshTokenGrants(t *testing.T) {
	f := newFirebaseFixture(t, time.Unix(1_750_000_000, 0).UTC())

	up := f.call("auth", "on_sign_up", "POST", "/v1/accounts:signUp", nil, nil, map[string]any{
		"email": "ada@example.test", "password": "Secret123!",
	}, firebaseAuth)
	if up.Status != 200 {
		t.Fatalf("signUp -> %d: %v", up.Status, up.Body)
	}
	localID := up.Body["localId"].(string)
	refresh := up.Body["refreshToken"].(string)

	v3Refresh := func(token string) starlark.Response {
		return f.call("auth", "on_relyingparty", "POST", "/identitytoolkit/v3/relyingparty/refreshToken",
			map[string]string{"action": "refreshToken"}, nil,
			map[string]any{"refreshToken": token}, firebaseAuth)
	}

	// ===== v3 refreshToken rotates the token pair for the bound user =====
	rt := v3Refresh(refresh)
	if rt.Status != 200 {
		t.Fatalf("v3 refreshToken -> %d: %v", rt.Status, rt.Body)
	}
	if rt.Body["user_id"] != localID || rt.Body["token_type"] != "Bearer" || rt.Body["expires_in"] != "3600" {
		t.Fatalf("v3 refresh envelope = %v", rt.Body)
	}
	if rt.Body["id_token"] == up.Body["idToken"] || rt.Body["refresh_token"] == refresh {
		t.Fatalf("v3 refresh did not rotate tokens: %v", rt.Body)
	}
	info := f.call("auth", "on_get_account_info", "POST", "/v1/accounts:getAccountInfo", nil, nil,
		map[string]any{"idToken": rt.Body["id_token"]}, firebaseAuth)
	if info.Status != 200 || info.Body["users"].([]any)[0].(map[string]any)["email"] != "ada@example.test" {
		t.Fatalf("rotated idToken lookup = %d %v, want ada", info.Status, info.Body)
	}
	// The rotated refresh token is itself exchangeable (chaining).
	if chained := v3Refresh(rt.Body["refresh_token"].(string)); chained.Status != 200 {
		t.Fatalf("chained refresh -> %d: %v", chained.Status, chained.Body)
	}

	// ===== unknown and missing refresh tokens are 400 INVALID_REFRESH_TOKEN =====
	bad := v3Refresh("not-a-real-refresh-token")
	if bad.Status != 400 || !strings.Contains(fbErr(t, bad)["message"].(string), "INVALID_REFRESH_TOKEN") {
		t.Fatalf("unknown refresh token -> %d %v, want 400 INVALID_REFRESH_TOKEN", bad.Status, bad.Body)
	}
	empty := f.call("auth", "on_relyingparty", "POST", "/identitytoolkit/v3/relyingparty/refreshToken",
		map[string]string{"action": "refreshToken"}, nil, map[string]any{}, firebaseAuth)
	if empty.Status != 400 {
		t.Fatalf("empty refresh -> %d, want 400 (MISSING_REFRESH_TOKEN)", empty.Status)
	}

	// ===== POST /v1/token exchanges a refresh token in the securetoken shape =====
	st := f.call("auth", "on_securetoken", "POST", "/v1/token", nil, nil, map[string]any{
		"grant_type": "refresh_token", "refresh_token": refresh,
	}, firebaseAuth)
	if st.Status != 200 {
		t.Fatalf("securetoken -> %d: %v", st.Status, st.Body)
	}
	if st.Body["access_token"] != st.Body["id_token"] {
		t.Fatalf("securetoken access_token = %v, id_token = %v (access_token mirrors id_token in this adapter)",
			st.Body["access_token"], st.Body["id_token"])
	}
	if st.Body["expires_in"] != "3600" || st.Body["token_type"] != "Bearer" ||
		st.Body["user_id"] != localID || st.Body["project_id"] != "stunt-firebase-project" {
		t.Fatalf("securetoken envelope = %v", st.Body)
	}
	if st.Body["refresh_token"] == "" || st.Body["refresh_token"] == refresh {
		t.Fatalf("securetoken refresh_token = %v, want a rotated token", st.Body["refresh_token"])
	}
	stInfo := f.call("auth", "on_get_account_info", "POST", "/v1/accounts:getAccountInfo", nil, nil,
		map[string]any{"idToken": st.Body["id_token"]}, firebaseAuth)
	if stInfo.Status != 200 || stInfo.Body["users"].([]any)[0].(map[string]any)["email"] != "ada@example.test" {
		t.Fatalf("securetoken id_token lookup = %d %v, want ada", stInfo.Status, stInfo.Body)
	}

	// ===== the grant rejects wrong grant types and unknown tokens =====
	badGrant := f.call("auth", "on_securetoken", "POST", "/v1/token", nil, nil, map[string]any{
		"grant_type": "password", "refresh_token": refresh,
	}, firebaseAuth)
	if badGrant.Status != 400 {
		t.Fatalf("wrong grant_type -> %d, want 400", badGrant.Status)
	}
	badToken := f.call("auth", "on_securetoken", "POST", "/v1/token", nil, nil, map[string]any{
		"grant_type": "refresh_token", "refresh_token": "nope",
	}, firebaseAuth)
	if badToken.Status != 400 {
		t.Fatalf("unknown refresh token grant -> %d, want 400", badToken.Status)
	}
}

// TestFirebaseFirestoreDocumentCRUD: typed-value documents — create wraps
// fields under a full resource name, get reads them back, PATCH merges and
// upserts, explicit documentIds are scoped per project+collection, delete
// empties the body, list paginates, and subcollections nest under their
// parent document.
func TestFirebaseFirestoreDocumentCRUD(t *testing.T) {
	f := newFirebaseFixture(t, time.Unix(1_750_000_000, 0).UTC())

	// ===== create wraps fields in Firestore typed values under a full resource name =====
	created := f.fbCreateDoc("people", "", true, map[string]any{
		"name":    fbStr("Alice Stunt"),
		"age":     fbInt("30"),
		"active":  fbBool(true),
		"score":   map[string]any{"doubleValue": 4.5},
		"tags":    fbArr(fbStr("premium"), fbStr("beta")),
		"address": map[string]any{"mapValue": map[string]any{"fields": map[string]any{"city": fbStr("San Francisco")}}},
	})
	if created.Status != 200 {
		t.Fatalf("create doc -> %d: %v", created.Status, created.Body)
	}
	if created.Body["name"] != fbResBase+"/people/doc-000001" {
		t.Fatalf("created name = %v, want %s/people/doc-000001", created.Body["name"], fbDocBase)
	}
	if got := fbTypedField(t, created.Body, "name")["stringValue"]; got != "Alice Stunt" {
		t.Fatalf("name field = %v", got)
	}
	if got := fbTypedField(t, created.Body, "age")["integerValue"]; got != "30" {
		t.Fatalf("age field = %v, want string-encoded \"30\"", got)
	}
	if got := fbTypedField(t, created.Body, "active")["booleanValue"]; got != true {
		t.Fatalf("active field = %v", got)
	}
	if got := fbTypedField(t, created.Body, "score")["doubleValue"]; got != 4.5 {
		t.Fatalf("score field = %v", got)
	}
	tags := fbTypedField(t, created.Body, "tags")["arrayValue"].(map[string]any)["values"].([]any)
	if len(tags) != 2 || tags[0].(map[string]any)["stringValue"] != "premium" {
		t.Fatalf("tags array = %v", tags)
	}
	addr := fbTypedField(t, created.Body, "address")["mapValue"].(map[string]any)["fields"].(map[string]any)
	if addr["city"].(map[string]any)["stringValue"] != "San Francisco" {
		t.Fatalf("address map = %v", addr)
	}
	if created.Body["createTime"] == "" || created.Body["updateTime"] == "" {
		t.Fatalf("create returned no timestamps: %v", created.Body)
	}
	createTime := created.Body["createTime"].(string)

	// ===== get reads the document back; unknown ids are 404 NOT_FOUND =====
	got := f.fbGetDoc("people", "doc-000001")
	if got.Status != 200 || got.Body["name"] != created.Body["name"] {
		t.Fatalf("get doc -> %d %v", got.Status, got.Body)
	}
	if got.Body["createTime"] != createTime {
		t.Fatalf("get createTime = %v, want the create timestamp", got.Body["createTime"])
	}
	missing := f.fbGetDoc("people", "no-such-doc")
	if missing.Status != 404 || fbErr(t, missing)["status"] != "NOT_FOUND" {
		t.Fatalf("get unknown doc -> %d %v, want 404 NOT_FOUND", missing.Status, missing.Body)
	}

	// ===== PATCH merges fields, bumps updateTime, and upserts unknown ids =====
	f.vc.Advance(1 * time.Hour)
	patched := f.call("firestore", "on_upsert_document", "PATCH", fbDocBase+"/people/doc-000001",
		map[string]string{"project": fbProject, "collection": "people", "id": "doc-000001"}, nil,
		map[string]any{"fields": map[string]any{"age": fbInt("31"), "city": fbStr("Lisbon")}}, firebaseAuth)
	if patched.Status != 200 {
		t.Fatalf("patch doc -> %d: %v", patched.Status, patched.Body)
	}
	if got := fbTypedField(t, patched.Body, "age")["integerValue"]; got != "31" {
		t.Fatalf("patched age = %v, want the patched 31", got)
	}
	if got := fbTypedField(t, patched.Body, "city")["stringValue"]; got != "Lisbon" {
		t.Fatalf("patched city = %v, want the added field", got)
	}
	if got := fbTypedField(t, patched.Body, "active")["booleanValue"]; got != true {
		t.Fatalf("patch dropped untouched field active: %v", patched.Body["fields"])
	}
	if patched.Body["updateTime"].(string) <= createTime {
		t.Fatalf("patched updateTime = %v, want it after createTime %v", patched.Body["updateTime"], createTime)
	}
	if patched.Body["createTime"] != createTime {
		t.Fatalf("patch must not rewrite createTime: %v", patched.Body["createTime"])
	}
	// PATCH on a nonexistent id creates it (Firestore upsert semantics).
	upserted := f.call("firestore", "on_upsert_document", "PATCH", fbDocBase+"/people/brand-new",
		map[string]string{"project": fbProject, "collection": "people", "id": "brand-new"}, nil,
		map[string]any{"fields": map[string]any{"name": fbStr("Brand New")}}, firebaseAuth)
	if upserted.Status != 200 || upserted.Body["name"] != fbResBase+"/people/brand-new" {
		t.Fatalf("patch-upsert -> %d %v", upserted.Status, upserted.Body)
	}
	if r := f.fbGetDoc("people", "brand-new"); r.Status != 200 {
		t.Fatalf("get patch-upserted doc -> %d, want 200", r.Status)
	}

	// ===== explicit documentIds are scoped per project+collection and 409 on reuse =====
	carol := f.fbCreateDoc("people", "carol", true, map[string]any{"name": fbStr("Carol")})
	if carol.Status != 200 || carol.Body["name"] != fbResBase+"/people/carol" {
		t.Fatalf("create with documentId -> %d %v", carol.Status, carol.Body)
	}
	dup := f.fbCreateDoc("people", "carol", true, map[string]any{"name": fbStr("Dup")})
	if dup.Status != 409 || fbErr(t, dup)["status"] != "ALREADY_EXISTS" {
		t.Fatalf("duplicate documentId -> %d %v, want 409 ALREADY_EXISTS", dup.Status, dup.Body)
	}
	// The SAME id under a different collection (or project) coexists — ids
	// are only unique within one collection path, like real Firestore.
	sameOther := f.fbCreateDoc("users", "carol", true, map[string]any{"name": fbStr("Carol U")})
	if sameOther.Status != 200 || sameOther.Body["name"] != fbResBase+"/users/carol" {
		t.Fatalf("same id other collection -> %d %v, want 200", sameOther.Status, sameOther.Body)
	}
	otherProject := f.call("firestore", "on_create_document", "POST",
		"/v1/projects/other-project/databases/(default)/documents/people",
		map[string]string{"project": "other-project", "collection": "people"},
		map[string]string{"documentId": "carol"},
		map[string]any{"fields": map[string]any{"name": fbStr("Carol O")}}, firebaseAuth)
	if otherProject.Status != 200 {
		t.Fatalf("same id other project -> %d: %v", otherProject.Status, otherProject.Body)
	}
	if r := f.fbGetDoc("users", "carol"); r.Status != 200 {
		t.Fatalf("get users/carol -> %d, want 200", r.Status)
	}

	// ===== delete returns an empty 200 and leaves 404s behind (its own project only) =====
	del := f.call("firestore", "on_delete_document", "DELETE", fbDocBase+"/people/carol",
		map[string]string{"project": fbProject, "collection": "people", "id": "carol"},
		nil, nil, firebaseAuth)
	if del.Status != 200 || len(del.Body) != 0 {
		t.Fatalf("delete doc -> %d %v, want 200 with empty body", del.Status, del.Body)
	}
	if r := f.fbGetDoc("people", "carol"); r.Status != 404 {
		t.Fatalf("get deleted doc -> %d, want 404", r.Status)
	}
	if r := f.call("firestore", "on_delete_document", "DELETE", fbDocBase+"/people/carol",
		map[string]string{"project": fbProject, "collection": "people", "id": "carol"},
		nil, nil, firebaseAuth); r.Status != 404 {
		t.Fatalf("re-delete -> %d, want 404", r.Status)
	}
	// The other-project namesake survived the delete.
	otherGet := f.call("firestore", "on_get_document", "GET",
		"/v1/projects/other-project/databases/(default)/documents/people/carol",
		map[string]string{"project": "other-project", "collection": "people", "id": "carol"},
		nil, nil, firebaseAuth)
	if otherGet.Status != 200 {
		t.Fatalf("other-project carol after delete -> %d, want 200", otherGet.Status)
	}

	// ===== list pages with pageSize/pageToken and rejects a malformed token =====
	for _, city := range []string{"berlin", "oslo", "rome"} {
		r := f.fbCreateDoc("cities", city, city != "oslo", map[string]any{"name": fbStr(city)})
		if r.Status != 200 {
			t.Fatalf("seed city %s -> %d: %v", city, r.Status, r.Body)
		}
	}
	list := func(query map[string]string) starlark.Response {
		return f.call("firestore", "on_list_documents", "GET", fbDocBase+"/cities",
			map[string]string{"project": fbProject, "collection": "cities"}, query, nil, firebaseAuth)
	}
	page1 := list(map[string]string{"pageSize": "2"})
	if page1.Status != 200 {
		t.Fatalf("list page 1 -> %d: %v", page1.Status, page1.Body)
	}
	docs1 := page1.Body["documents"].([]any)
	cursor, _ := page1.Body["nextPageToken"].(string)
	if len(docs1) != 2 || cursor == "" {
		t.Fatalf("page 1 = %d docs cursor %q, want 2 docs + nextPageToken", len(docs1), cursor)
	}
	page2 := list(map[string]string{"pageSize": "2", "pageToken": cursor})
	if page2.Status != 200 {
		t.Fatalf("list page 2 -> %d: %v", page2.Status, page2.Body)
	}
	docs2 := page2.Body["documents"].([]any)
	if len(docs2) != 1 {
		t.Fatalf("page 2 has %d docs, want the last city", len(docs2))
	}
	if _, more := page2.Body["nextPageToken"]; more {
		t.Fatalf("page 2 still has nextPageToken %v", page2.Body["nextPageToken"])
	}
	seen := map[string]bool{}
	for _, d := range append(append([]any{}, docs1...), docs2...) {
		seen[strings.TrimPrefix(d.(map[string]any)["name"].(string), fbResBase+"/cities/")] = true
	}
	if len(seen) != 3 || !seen["berlin"] || !seen["oslo"] || !seen["rome"] {
		t.Fatalf("paged walk covered %v, want all three cities exactly once", seen)
	}
	badToken := list(map[string]string{"pageSize": "2", "pageToken": "not-a-token"})
	if badToken.Status != 400 || fbErr(t, badToken)["status"] != "INVALID_ARGUMENT" {
		t.Fatalf("malformed pageToken -> %d %v, want 400 INVALID_ARGUMENT", badToken.Status, badToken.Body)
	}

	// ===== subcollections nest under their parent document path =====
	sub := f.call("firestore", "on_create_subdocument", "POST", fbDocBase+"/users/carol/addresses",
		map[string]string{"project": fbProject, "collection": "users", "document": "carol", "sub": "addresses"},
		map[string]string{"documentId": "home"},
		map[string]any{"fields": map[string]any{"city": fbStr("San Francisco")}}, firebaseAuth)
	if sub.Status != 200 || sub.Body["name"] != fbResBase+"/users/carol/addresses/home" {
		t.Fatalf("create subdoc -> %d %v", sub.Status, sub.Body)
	}
	subList := f.call("firestore", "on_list_subdocuments", "GET", fbDocBase+"/users/carol/addresses",
		map[string]string{"project": fbProject, "collection": "users", "document": "carol", "sub": "addresses"},
		nil, nil, firebaseAuth)
	if subList.Status != 200 || len(subList.Body["documents"].([]any)) != 1 {
		t.Fatalf("list subdocs -> %d %v", subList.Status, subList.Body)
	}
	subGet := f.call("firestore", "on_get_subdocument", "GET", fbDocBase+"/users/carol/addresses/home",
		map[string]string{"project": fbProject, "collection": "users", "document": "carol", "sub": "addresses", "id": "home"},
		nil, nil, firebaseAuth)
	if subGet.Status != 200 || subGet.Body["name"] != sub.Body["name"] {
		t.Fatalf("get subdoc -> %d %v", subGet.Status, subGet.Body)
	}
	// The subdocument id is NOT reachable through the flat users collection.
	if r := f.fbGetDoc("users", "home"); r.Status != 404 {
		t.Fatalf("flat get of subdocument id -> %d, want 404 (path-scoped)", r.Status)
	}
}

// TestFirebaseFirestoreRunQuery: the structuredQuery subset — filter/sort/
// limit over unwrapped typed fields, IN and ARRAY_CONTAINS over arrays,
// project scoping, and the 400 INVALID_ARGUMENT envelopes for malformed
// queries.
func TestFirebaseFirestoreRunQuery(t *testing.T) {
	f := newFirebaseFixture(t, time.Unix(1_750_000_000, 0).UTC())

	seed := map[string]struct {
		age  string
		tags []string
	}{
		"carol": {"50", []string{"premium"}},
		"alice": {"30", []string{"premium", "beta"}},
		"bob":   {"40", []string{"beta"}},
	}
	for id, p := range seed {
		tagVals := make([]any, 0, len(p.tags))
		for _, tag := range p.tags {
			tagVals = append(tagVals, fbStr(tag))
		}
		r := f.fbCreateDoc("people", id, true, map[string]any{
			"name": fbStr(id), "age": fbInt(p.age),
			"tags": map[string]any{"arrayValue": map[string]any{"values": tagVals}},
		})
		if r.Status != 200 {
			t.Fatalf("seed %s -> %d: %v", id, r.Status, r.Body)
		}
	}
	runQuery := func(project string, sq map[string]any) starlark.Response {
		return f.call("firestore", "on_run_query", "POST",
			"/v1/projects/"+project+"/databases/(default)/documents:runQuery",
			map[string]string{"project": project}, nil,
			map[string]any{"structuredQuery": sq}, firebaseAuth)
	}

	// ===== runQuery filters, sorts, and limits over typed fields =====
	q := runQuery(fbProject, map[string]any{
		"from": []any{map[string]any{"collectionId": "people"}},
		"where": map[string]any{"fieldFilter": map[string]any{
			"field": map[string]any{"fieldPath": "age"},
			"op":    "GREATER_THAN",
			"value": fbInt("25"),
		}},
		"orderBy": []any{map[string]any{
			"field": map[string]any{"fieldPath": "age"}, "direction": "DESCENDING",
		}},
		"limit": 2,
	})
	if q.Status != 200 {
		t.Fatalf("runQuery -> %d: %v", q.Status, q.Body)
	}
	if len(q.BodyList) != 2 {
		t.Fatalf("runQuery returned %d rows, want the limit of 2", len(q.BodyList))
	}
	first := q.BodyList[0].(map[string]any)
	firstDoc := first["document"].(map[string]any)
	if !strings.HasSuffix(firstDoc["name"].(string), "/people/carol") {
		t.Fatalf("runQuery[0].name = %v, want carol (age DESC)", firstDoc["name"])
	}
	if got := fbTypedField(t, firstDoc, "age")["integerValue"]; got != "50" {
		t.Fatalf("runQuery[0].age = %v, want 50", got)
	}
	if first["readTime"] == "" {
		t.Fatalf("runQuery row has no readTime: %v", first)
	}
	eq := runQuery(fbProject, map[string]any{
		"from": []any{map[string]any{"collectionId": "people"}},
		"where": map[string]any{"fieldFilter": map[string]any{
			"field": map[string]any{"fieldPath": "name"},
			"op":    "EQUAL",
			"value": fbStr("alice"),
		}},
	})
	if eq.Status != 200 || len(eq.BodyList) != 1 ||
		!strings.HasSuffix(eq.BodyList[0].(map[string]any)["document"].(map[string]any)["name"].(string), "/people/alice") {
		t.Fatalf("EQUAL query = %d %v, want alice only", eq.Status, eq.BodyList)
	}

	// ===== IN and ARRAY_CONTAINS resolve over array values =====
	in := runQuery(fbProject, map[string]any{
		"from": []any{map[string]any{"collectionId": "people"}},
		"where": map[string]any{"fieldFilter": map[string]any{
			"field": map[string]any{"fieldPath": "age"},
			"op":    "IN",
			"value": fbArr(fbInt("30"), fbInt("40")),
		}},
	})
	if in.Status != 200 || len(in.BodyList) != 2 {
		t.Fatalf("IN query = %d %v, want alice+bob (ages 30,40)", in.Status, in.BodyList)
	}
	contains := runQuery(fbProject, map[string]any{
		"from": []any{map[string]any{"collectionId": "people"}},
		"where": map[string]any{"fieldFilter": map[string]any{
			"field": map[string]any{"fieldPath": "tags"},
			"op":    "ARRAY_CONTAINS",
			"value": fbStr("beta"),
		}},
	})
	if contains.Status != 200 || len(contains.BodyList) != 2 {
		t.Fatalf("ARRAY_CONTAINS query = %d %v, want alice+bob (tagged beta)", contains.Status, contains.BodyList)
	}

	// ===== queries are project-scoped and malformed bodies are 400 INVALID_ARGUMENT =====
	if other := runQuery("other-project", map[string]any{
		"from": []any{map[string]any{"collectionId": "people"}},
	}); other.Status != 200 || len(other.BodyList) != 0 {
		t.Fatalf("other-project query = %d %v, want 200 with no rows", other.Status, other.BodyList)
	}
	for name, body := range map[string]any{
		"no structuredQuery": nil,
		"no from":            map[string]any{},
		"unary filter": map[string]any{
			"from":  []any{map[string]any{"collectionId": "people"}},
			"where": map[string]any{"unaryFilter": map[string]any{}},
		},
		"unsupported op": map[string]any{
			"from": []any{map[string]any{"collectionId": "people"}},
			"where": map[string]any{"fieldFilter": map[string]any{
				"field": map[string]any{"fieldPath": "age"},
				"op":    "SOMETHING_WEIRD",
				"value": fbInt("1"),
			}},
		},
	} {
		var r starlark.Response
		if body == nil {
			r = f.call("firestore", "on_run_query", "POST", fbDocBase+":runQuery",
				map[string]string{"project": fbProject}, nil, map[string]any{}, firebaseAuth)
		} else {
			r = f.call("firestore", "on_run_query", "POST", fbDocBase+":runQuery",
				map[string]string{"project": fbProject}, nil,
				map[string]any{"structuredQuery": body}, firebaseAuth)
		}
		if r.Status != 400 || fbErr(t, r)["status"] != "INVALID_ARGUMENT" {
			t.Fatalf("runQuery %s -> %d %v, want 400 INVALID_ARGUMENT", name, r.Status, r.Body)
		}
	}
}

// TestFirebaseFCMSendFanout: FCM sends — a token send is stored and named,
// sends must carry exactly one target, topic and condition sends fan out to
// the subscribed device tokens (union and intersection), unsubscribing
// shrinks the fanout, and the stored message list is project-scoped.
func TestFirebaseFCMSendFanout(t *testing.T) {
	f := newFirebaseFixture(t, time.Unix(1_750_000_000, 0).UTC())

	const sendPath = "/v1/projects/" + fbProject + "/messages:send"
	send := func(project string, message map[string]any) starlark.Response {
		return f.call("fcm", "on_send_message", "POST", "/v1/projects/"+project+"/messages:send",
			map[string]string{"project": project}, nil,
			map[string]any{"message": message}, firebaseAuth)
	}
	listMessages := func(project string) starlark.Response {
		return f.call("fcm", "on_list_messages", "GET", "/v1/projects/"+project+"/messages",
			map[string]string{"project": project}, nil, nil, firebaseAuth)
	}
	subscribe := func(topic string, tokens []any) starlark.Response {
		return f.call("fcm", "on_subscribe", "POST", "/v1/projects/"+fbProject+"/topics/"+topic+":subscribe",
			map[string]string{"project": fbProject, "topic": topic}, nil,
			map[string]any{"tokens": tokens}, firebaseAuth)
	}

	// ===== a token send is stored with its recipient and answers with its resource name =====
	sent := send(fbProject, map[string]any{
		"token":        "device-a",
		"notification": map[string]any{"title": "Order shipped", "body": "It is on the way"},
		"data":         map[string]any{"orderId": "order-123"},
	})
	if sent.Status != 200 {
		t.Fatalf("send -> %d: %v", sent.Status, sent.Body)
	}
	if sent.Body["name"] != "projects/"+fbProject+"/messages/1" {
		t.Fatalf("send name = %v, want projects/%s/messages/1", sent.Body["name"], fbProject)
	}
	msgs := listMessages(fbProject)
	if msgs.Status != 200 {
		t.Fatalf("list messages -> %d: %v", msgs.Status, msgs.Body)
	}
	stored := fbFindMessage(t, msgs, "token", "device-a")
	if got := fbRecipients(t, stored); len(got) != 1 || got[0] != "device-a" {
		t.Fatalf("stored recipients = %v, want [device-a]", got)
	}

	// ===== a send must carry exactly one target =====
	noTarget := send(fbProject, map[string]any{"notification": map[string]any{"title": "Nowhere"}})
	if noTarget.Status != 400 || fbErr(t, noTarget)["status"] != "INVALID_ARGUMENT" {
		t.Fatalf("no-target send -> %d %v, want 400 INVALID_ARGUMENT", noTarget.Status, noTarget.Body)
	}
	multiTarget := send(fbProject, map[string]any{"token": "device-a", "topic": "news"})
	if multiTarget.Status != 400 {
		t.Fatalf("multi-target send -> %d, want 400", multiTarget.Status)
	}

	// ===== topic and condition sends fan out to the subscribed tokens =====
	for topic, tokens := range map[string][]any{
		"news":   {"device-a", "device-b"},
		"vip":    {"device-b"},
		"sports": {"device-c"},
	} {
		r := subscribe(topic, tokens)
		if r.Status != 200 || r.Body["success"] != true || r.Body["subscribed"] != int64(len(tokens)) {
			t.Fatalf("subscribe %s -> %d %v", topic, r.Status, r.Body)
		}
	}
	if r := send(fbProject, map[string]any{"topic": "news", "notification": map[string]any{"title": "News flash"}}); r.Status != 200 {
		t.Fatalf("topic send -> %d: %v", r.Status, r.Body)
	}
	if r := send(fbProject, map[string]any{"condition": "'news' in topics || 'sports' in topics"}); r.Status != 200 {
		t.Fatalf("condition union send -> %d: %v", r.Status, r.Body)
	}
	if r := send(fbProject, map[string]any{"condition": "'news' in topics && 'vip' in topics"}); r.Status != 200 {
		t.Fatalf("condition intersection send -> %d: %v", r.Status, r.Body)
	}
	msgs = listMessages(fbProject)
	newsMsg := fbFindMessage(t, msgs, "topic", "news")
	if got := fbRecipients(t, newsMsg); len(got) != 2 {
		t.Fatalf("news fanout = %v, want device-a + device-b", got)
	}
	unionMsg := fbFindMessage(t, msgs, "condition", "'news' in topics || 'sports' in topics")
	if got := fbRecipients(t, unionMsg); len(got) != 3 {
		t.Fatalf("condition union fanout = %v, want a+b+c", got)
	}
	interMsg := fbFindMessage(t, msgs, "condition", "'news' in topics && 'vip' in topics")
	if got := fbRecipients(t, interMsg); len(got) != 1 || got[0] != "device-b" {
		t.Fatalf("condition intersection fanout = %v, want [device-b] only", got)
	}

	// ===== unsubscribing shrinks the fanout and a bare subscribe is a 400 =====
	unsub := f.call("fcm", "on_unsubscribe", "POST", "/v1/projects/"+fbProject+"/topics/news:unsubscribe",
		map[string]string{"project": fbProject, "topic": "news"}, nil,
		map[string]any{"token": "device-a"}, firebaseAuth)
	if unsub.Status != 200 || unsub.Body["unsubscribed"] != int64(1) {
		t.Fatalf("unsubscribe -> %d %v", unsub.Status, unsub.Body)
	}
	if r := send(fbProject, map[string]any{"topic": "news"}); r.Status != 200 {
		t.Fatalf("topic send after unsubscribe -> %d: %v", r.Status, r.Body)
	}
	// Two news sends are stored now; the latest must have the shrunken fanout.
	latest := map[string]any{"id": ""}
	for _, m := range listMessages(fbProject).Body["messages"].([]any) {
		mm := m.(map[string]any)
		if mm["topic"] == "news" && mm["id"].(string) > latest["id"].(string) {
			latest = mm
		}
	}
	if got := fbRecipients(t, latest); len(got) != 1 || got[0] != "device-b" {
		t.Fatalf("news fanout after unsubscribe = %v, want [device-b]", got)
	}
	bare := f.call("fcm", "on_subscribe", "POST", "/v1/projects/"+fbProject+"/topics/news:subscribe",
		map[string]string{"project": fbProject, "topic": "news"}, nil, map[string]any{}, firebaseAuth)
	if bare.Status != 400 {
		t.Fatalf("subscribe with no token -> %d, want 400 (MISSING_TOKEN)", bare.Status)
	}

	// ===== the stored message list is project-scoped =====
	before := len(listMessages(fbProject).Body["messages"].([]any))
	if r := send("other-project", map[string]any{"token": "device-z"}); r.Status != 200 {
		t.Fatalf("other-project send -> %d: %v", r.Status, r.Body)
	}
	if after := len(listMessages(fbProject).Body["messages"].([]any)); after != before {
		t.Fatalf("other-project send leaked into %s list: %d -> %d", fbProject, before, after)
	}
	if other := listMessages("other-project"); len(other.Body["messages"].([]any)) != 1 {
		t.Fatalf("other-project list = %v, want its single message", other.Body)
	}
}
