package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"stuntapi.com/stunt/internal/manifest"
)

func socialAdapter(t *testing.T, adapter string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "adapters", adapter))
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(&manifest.Manifest{Path: filepath.Join(t.TempDir(), "stunt.yaml"), Version: 1, Network: manifest.Network{Mode: "port", BasePort: 0}, Services: map[string]manifest.Service{"identity": {Adapter: path}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	addresses, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cancel)
	return addresses["identity"]
}

func TestSocialOAuthModernClients(t *testing.T) {
	for _, tc := range []struct{ name, authorize, token string }{
		{"google-style", "/o/oauth2/v2/auth", "/token"},
		{"github-style", "/login/oauth/authorize", "/login/oauth/access_token"},
		{"entra-id-style", "/common/oauth2/v2.0/authorize", "/common/oauth2/v2.0/token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := socialAdapter(t, tc.name)
			redirect := "http://localhost:3000/callback"
			q := url.Values{"client_id": {"synthetic-client"}, "redirect_uri": {redirect}, "state": {"synthetic-state"}, "response_type": {"code"}, "scope": {"openid email profile offline_access"}, "code_challenge": {"synthetic-verifier"}, "code_challenge_method": {"plain"}}
			response := googleGetNoRedirect(t, base+tc.authorize+"?"+q.Encode())
			response.Body.Close()
			if response.StatusCode != 302 {
				t.Fatalf("authorize status = %d", response.StatusCode)
			}
			callback, err := url.Parse(response.Header.Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			if callback.Query().Get("state") != "synthetic-state" {
				t.Fatal("state did not round trip")
			}
			form := url.Values{"grant_type": {"authorization_code"}, "client_id": {"synthetic-client"}, "client_secret": {"synthetic-secret"}, "redirect_uri": {redirect}, "code": {callback.Query().Get("code")}, "code_verifier": {"synthetic-verifier"}}
			if tc.name == "github-style" {
				form.Set("code_verifier", "wrong-verifier")
				if body, status := googlePostForm(t, base+tc.token, form); status != 400 {
					t.Fatalf("wrong PKCE verifier = %d: %s", status, body)
				}
				form.Set("code_verifier", "synthetic-verifier")
			}
			body, status := googlePostForm(t, base+tc.token, form)
			if status != 200 {
				t.Fatalf("exchange status = %d: %s", status, body)
			}
			var tokens map[string]any
			if err := json.Unmarshal([]byte(body), &tokens); err != nil {
				t.Fatal(err)
			}
			if tokens["access_token"] == nil {
				t.Fatal("no access token")
			}
			if tc.name == "entra-id-style" {
				token, _ := tokens["id_token"].(string)
				parts := strings.Split(token, ".")
				if len(parts) != 3 {
					t.Fatalf("invalid id_token: %v", tokens["id_token"])
				}
				payload, err := base64.RawURLEncoding.DecodeString(parts[1])
				if err != nil {
					t.Fatal(err)
				}
				var claims map[string]any
				if err := json.Unmarshal(payload, &claims); err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"oid", "tid", "email", "sub", "iss", "aud"} {
					if claims[key] == nil {
						t.Fatalf("missing identity claim %s: %s", key, payload)
					}
				}
			}
			if tc.name == "github-style" {
				for _, route := range []string{"/user", "/user/emails"} {
					request, _ := http.NewRequest("GET", base+route, nil)
					request.Header.Set("Authorization", "Bearer "+tokens["access_token"].(string))
					resp, err := http.DefaultClient.Do(request)
					if err != nil {
						t.Fatal(err)
					}
					raw, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if resp.StatusCode != 200 {
						t.Fatalf("%s = %d: %s", route, resp.StatusCode, raw)
					}
					bad := googleGetNoRedirect(t, base+route)
					bad.Body.Close()
					if bad.StatusCode != 401 {
						t.Fatalf("anonymous %s = %d", route, bad.StatusCode)
					}
				}
			}
			if body, status := googlePostForm(t, base+tc.token, form); status != 400 {
				t.Fatalf("code replay status = %d: %s", status, body)
			}
		})
	}
}

func TestAppleHybridFormPost(t *testing.T) {
	base := socialAdapter(t, "signin-with-apple-style")
	redirect := "http://localhost:3000/callback"
	q := url.Values{"client_id": {"com.example.signin.service"}, "redirect_uri": {redirect}, "state": {`state<&"`}, "response_type": {"code id_token"}, "response_mode": {"form_post"}, "scope": {"email name"}}
	resp := googleGetNoRedirect(t, base+"/auth/authorize?"+q.Encode())
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("form_post = %d: %s", resp.StatusCode, raw)
	}
	fields := map[string]string{}
	for _, match := range regexp.MustCompile(`name="([^"]+)" value="([^"]*)"`).FindAllStringSubmatch(string(raw), -1) {
		fields[match[1]] = html.UnescapeString(match[2])
	}
	if fields["state"] != q.Get("state") {
		t.Fatalf("state not escaped/round-tripped: %s", raw)
	}
	body, status := siwaPostForm(t, base+"/auth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {"com.example.signin.service"}, "client_secret": {siwaMintClientSecret(t, 3600)}, "redirect_uri": {redirect}, "code": {fields["code"]}})
	if status != 200 {
		t.Fatalf("exchange = %d: %s", status, body)
	}
	var tokens map[string]any
	if err := json.Unmarshal([]byte(body), &tokens); err != nil {
		t.Fatal(err)
	}
	// The front-channel and exchanged identity tokens identify the same login.
	for _, token := range []string{fields["id_token"], tokens["id_token"].(string)} {
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			t.Fatal("missing hybrid id_token")
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		var claims map[string]any
		if err := json.Unmarshal(payload, &claims); err != nil {
			t.Fatal(err)
		}
		if fields["sub"] == "" {
			fields["sub"] = claims["sub"].(string)
		} else if fields["sub"] != claims["sub"] {
			t.Fatal("hybrid and exchanged subjects differ")
		}
	}
}
