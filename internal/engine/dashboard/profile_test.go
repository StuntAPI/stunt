package dashboard_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"stuntapi.com/stunt/internal/engine/dashboard"
)

func newProfileDashboard(t *testing.T, active map[string]string) (*dashboard.Dashboard, *httptest.Server) {
	t.Helper()
	d := dashboard.New(dummyStore(t))
	d.SetTokenForTest("tok")
	d.SetProfile(
		func() any {
			return map[string]any{
				"active_global": "",
				"active":        active,
				"catalog": map[string]any{
					"presets":  []any{},
					"services": map[string]any{},
				},
			}
		},
		func(name, service string) error {
			if name == "chaos" && service == "" {
				active["hello"] = name
				return nil
			}
			if name == "" {
				delete(active, service)
				return nil
			}
			return errFake{Name: name, Service: service}
		},
	)
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	return d, srv
}

// errFake mimics the engine's descriptive resolution errors.
type errFake struct {
	Name, Service string
}

func (e errFake) Error() string {
	return "unknown profile \"" + e.Name + "\" — available: presets: launch-day; hello: chaos"
}

func TestProfileEndpoint(t *testing.T) {
	active := map[string]string{"hello": "chaos"}
	_, srv := newProfileDashboard(t, active)
	hdr := func() http.Header { h := http.Header{}; h.Set("X-Stunt-Token", "tok"); return h }()

	// GET with token → current state + catalog.
	get, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/profile", nil)
	get.Header = hdr
	res, err := http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !bytes.Contains(body, []byte("\"chaos\"")) {
		t.Fatalf("GET /api/profile: status=%d body=%q", res.StatusCode, string(body))
	}
	// The token must never appear in any response payload.
	if bytes.Contains(body, []byte("tok")) {
		t.Fatalf("token echoed in profile payload: %q", string(body))
	}

	// POST activates; response reflects the new state.
	post, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/profile",
		strings.NewReader(`{"name":"chaos"}`))
	post.Header = hdr
	res, err = http.DefaultClient.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(res.Body)
	if res.StatusCode != 200 || active["hello"] != "chaos" {
		t.Fatalf("POST activate: status=%d active=%v body=%q", res.StatusCode, active, string(body))
	}

	// POST deactivate-all ({"name":""}).
	post, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/profile",
		strings.NewReader(`{"name":"","service":"hello"}`))
	post.Header = hdr
	res, _ = http.DefaultClient.Do(post)
	body, _ = io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("POST deactivate: status=%d body=%q", res.StatusCode, string(body))
	}
	if _, still := active["hello"]; still {
		t.Fatalf("deactivate left %v", active)
	}

	// Unauthenticated → 401.
	res, err = http.Post(srv.URL+"/api/profile", "application/json", strings.NewReader(`{"name":"chaos"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 401 {
		t.Fatalf("no-token POST -> %d, want 401", res.StatusCode)
	}
}

// A resolution failure surfaces the engine's descriptive error verbatim.
func TestProfileEndpointError(t *testing.T) {
	active := map[string]string{}
	_, srv := newProfileDashboard(t, active)
	post, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/profile",
		strings.NewReader(`{"name":"nope"}`))
	post.Header.Set("X-Stunt-Token", "tok")
	res, err := http.DefaultClient.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 400 {
		t.Fatalf("unknown profile -> %d, want 400", res.StatusCode)
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode error body: %v (%q)", err, string(body))
	}
	if !strings.Contains(payload.Error, "available") || !strings.Contains(payload.Error, "launch-day") {
		t.Fatalf("error should carry the availability line, got %q", payload.Error)
	}
}

// Unwired dashboard → 503, not a panic.
func TestProfileEndpointUnavailable(t *testing.T) {
	d := dashboard.New(dummyStore(t))
	d.SetTokenForTest("tok")
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	res, err := http.Get(srv.URL + "/api/profile")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 401 { // guard runs before the nil check
		t.Fatalf("unauthenticated GET -> %d", res.StatusCode)
	}
	get, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/profile", nil)
	get.Header.Set("X-Stunt-Token", "tok")
	res, err = http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 503 {
		t.Fatalf("unwired GET -> %d, want 503", res.StatusCode)
	}
}
