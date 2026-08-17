package conformance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/option"
)

// TestGoogleOAuthConformance drives Google's own auth stack (golang.org/x/
// oauth2 + the idtoken verifier from google-api-go-client) against the
// google-style adapter: the full authorization-code exchange, refresh
// with rotation, and — the marquee — idtoken.Validate verifying the
// adapter's RS256 id_token against the JWKS the adapter itself serves.
func TestGoogleOAuthConformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "google-style")

	// The idtoken validator fetches Google's certs URL; a rewrite client
	// pointing googleapis.com at the adapter makes it verify against the
	// adapter-served /oauth2/v3/certs instead.
	googleClient := &http.Client{
		Timeout:   30 * 1000 * 1000 * 1000,
		Transport: &rewriteTransport{to: mustURL(t, base), base: http.DefaultTransport},
	}

	conf := &oauth2.Config{
		ClientID:     "conformance-client",
		ClientSecret: "conformance-secret",
		Endpoint: oauth2.Endpoint{
			AuthURL:  base + "/o/oauth2/auth",
			TokenURL: base + "/o/oauth2/token",
		},
		RedirectURL: "http://localhost:9090/callback",
		Scopes:      []string{"openid", "email"},
	}

	// ===== Walk the authorize redirect to mint a code (the real first hop) =====

	authURL := conf.AuthCodeURL("conformance-state")
	// Do NOT follow the 302 — the redirect_uri is the client's own
	// callback (unroutable here); the code lives in the Location header.
	noRedirect := *googleClient
	noRedirect.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := noRedirect.Get(authURL)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != 302 || loc == "" {
		t.Fatalf("authorize -> %d Location=%q", resp.StatusCode, loc)
	}
	code := locQuery(t, loc, "code")
	if code == "" {
		t.Fatalf("authorize redirect carries no code: %s", loc)
	}
	Record(t, "x/oauth2", "google-style", "authorize redirect mints a single-use code")

	// ===== The token exchange (the x/oauth2 library does the POST) =====

	tok, err := conf.Exchange(context.WithValue(ctx, oauth2.HTTPClient, googleClient), code)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatal("no access token")
	}
	idTok, hasID := tok.Extra("id_token").(string)
	if !hasID || idTok == "" {
		t.Fatal("exchange response carries no id_token (openid scope)")
	}
	Record(t, "x/oauth2", "google-style", "authorization-code exchange -> tokens + id_token")

	// ===== idtoken.Validate — Google's own verifier against the adapter's JWKS =====

	validator, err := idtoken.NewValidator(ctx, option.WithHTTPClient(googleClient))
	if err != nil {
		t.Fatalf("idtoken.NewValidator: %v", err)
	}
	payload, err := validator.Validate(ctx, idTok, "conformance-client")
	if err != nil {
		t.Fatalf("idtoken.Validate (Google's RS256+JWKS verifier): %v", err)
	}
	if payload.Issuer == "" || payload.Subject == "" {
		t.Fatalf("validated payload: %+v", payload)
	}
	Record(t, "google-api-go-client/idtoken", "google-style", "idtoken.Validate verifies the adapter's RS256 id_token via its JWKS")

	// ===== Refresh with rotation (the old token dies) =====

	// Expire the cached token so TokenSource performs a real refresh
	// grant (otherwise x/oauth2 just returns its cache).
	tok.Expiry = time.Now().Add(-time.Minute)
	src := conf.TokenSource(ctx, tok)
	refreshed, err := src.Token()
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if refreshed.AccessToken == tok.AccessToken {
		t.Fatal("refresh returned the same access token (no rotation)")
	}
	Record(t, "x/oauth2", "google-style", "refresh grant rotates the access token")

	// The userinfo endpoint honors the refreshed token.
	ureq, _ := http.NewRequest("GET", base+"/oauth2/v3/userinfo", nil)
	ureq.Header.Set("Authorization", "Bearer "+refreshed.AccessToken)
	uresp, err := googleClient.Do(ureq)
	if err != nil {
		t.Fatalf("userinfo: %v", err)
	}
	ub, _ := io.ReadAll(uresp.Body)
	uresp.Body.Close()
	if uresp.StatusCode != 200 || !strings.Contains(string(ub), "sub") {
		t.Fatalf("userinfo with refreshed token -> %d: %s", uresp.StatusCode, ub)
	}
	Record(t, "x/oauth2", "google-style", "userinfo honors the refreshed token")

	// The JWKS the validator used is the adapter's own.
	cresp, err := googleClient.Get(base + "/oauth2/v3/certs")
	if err != nil {
		t.Fatal(err)
	}
	cb, _ := io.ReadAll(cresp.Body)
	cresp.Body.Close()
	var jwks struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(cb, &jwks); err != nil || len(jwks.Keys) == 0 {
		t.Fatalf("adapter JWKS: %v (%s)", err, cb)
	}
	Record(t, "google-api-go-client/idtoken", "google-style", "adapter serves a real JWKS")
}

func locQuery(t *testing.T, loc, key string) string {
	t.Helper()
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse redirect %s: %v", loc, err)
	}
	return u.Query().Get(key)
}

func mustURL(t *testing.T, s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return u
}
