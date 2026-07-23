package dashboard_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"stuntapi.com/stunt/internal/engine/dashboard"
)

// noRedirectClient returns an http.Client that does NOT follow redirects,
// so a test can inspect the 302 + Set-Cookie from the bootstrap response
// directly (the default client has no cookie jar and would lose the cookie
// when following the redirect).
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// TestCookieBootstrap asserts that a browser-style request with a ?token=<tok>
// query param (no header, no cookie) gets a 302 redirect that SETS a
// stunt_token cookie and strips the token query from the Location.
func TestCookieBootstrap(t *testing.T) {
	d := dashboard.New(dummyStore(t))
	d.SetTokenForTest("tok")
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	// Browser nav: no header, no cookie, token in the query string. Do not
	// follow the redirect so we can inspect the 302 + Set-Cookie.
	client := noRedirectClient()
	res, err := client.Get(srv.URL + "/?token=tok")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusFound {
		t.Fatalf("want 302 redirect for bootstrap, got %d", res.StatusCode)
	}

	// Must set the stunt_token cookie with the token value.
	var gotCookie string
	for _, c := range res.Cookies() {
		if c.Name == "stunt_token" {
			gotCookie = c.Value
		}
	}
	if gotCookie != "tok" {
		t.Fatalf("want Set-Cookie stunt_token=tok, got %q", gotCookie)
	}

	// Location must NOT carry the token query anymore.
	loc := res.Header.Get("Location")
	if loc == "" {
		t.Fatalf("expected Location header")
	}
	if containsParam(loc, "token") {
		t.Fatalf("Location must strip the token query, got %q", loc)
	}
}

// TestCookieAuthFollowup asserts that a request presenting the bootstrap
// cookie (no header) is authorized (200), proving the ws client + page nav
// path works.
func TestCookieAuthFollowup(t *testing.T) {
	d := dashboard.New(dummyStore(t))
	d.SetTokenForTest("tok")
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	// Bootstrap to obtain the cookie (don't follow the redirect).
	client := noRedirectClient()
	res, err := client.Get(srv.URL + "/?token=tok")
	if err != nil {
		t.Fatalf("Get bootstrap: %v", err)
	}
	res.Body.Close()

	// Reuse the cookie on a follow-up API request (no header).
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/requests", nil)
	for _, c := range res.Cookies() {
		req.AddCookie(c)
	}
	res2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do follow-up: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("want 200 with cookie auth, got %d", res2.StatusCode)
	}
}

// TestWrongCookieRejected asserts that a stunt_token cookie with the wrong
// value (and no header) is rejected with 401.
func TestWrongCookieRejected(t *testing.T) {
	d := dashboard.New(dummyStore(t))
	d.SetTokenForTest("tok")
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/requests", nil)
	req.AddCookie(&http.Cookie{Name: "stunt_token", Value: "wrong"})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 for wrong cookie, got %d", res.StatusCode)
	}
}

// TestHeaderAuthRegression ensures the original X-Stunt-Token header path
// still works (no cookie, no query) after the cookie changes.
func TestHeaderAuthRegression(t *testing.T) {
	d := dashboard.New(dummyStore(t))
	d.SetTokenForTest("tok")
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/requests", nil)
	req.Header.Set("X-Stunt-Token", "tok")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("want 200 with header auth, got %d", res.StatusCode)
	}
}

// TestNoAuthRejectedRegression ensures a request with no header, no cookie,
// and no matching query is rejected (401), and does NOT set a cookie.
func TestNoAuthRejectedRegression(t *testing.T) {
	d := dashboard.New(dummyStore(t))
	d.SetTokenForTest("tok")
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/api/requests")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 with no auth, got %d", res.StatusCode)
	}
	for _, c := range res.Cookies() {
		if c.Name == "stunt_token" {
			t.Fatalf("must not set cookie on unauthenticated request, got %v", c)
		}
	}
}

// containsParam is a tiny helper that checks whether the given URL/path
// contains a query parameter named key. It avoids importing url.Parse for a
// one-off substring check on simple localhost paths.
func containsParam(rawURL, key string) bool {
	i := indexByte(rawURL, '?')
	if i < 0 {
		return false
	}
	query := rawURL[i+1:]
	for query != "" {
		var pair string
		if j := indexByte(query, '&'); j >= 0 {
			pair, query = query[:j], query[j+1:]
		} else {
			pair, query = query, ""
		}
		eq := indexByte(pair, '=')
		name := pair
		if eq >= 0 {
			name = pair[:eq]
		}
		if name == key {
			return true
		}
	}
	return false
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
