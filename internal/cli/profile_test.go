package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeProfileServer stands in for a running stunt dashboard's /api/profile.
type fakeProfileServer struct {
	active map[string]string
	global string
	got    []map[string]string
}

func (f *fakeProfileServer) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authOK := r.Header.Get("X-Stunt-Token") == "tok"
		if r.URL.Path != "/api/profile" || !authOK {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"active_global": f.global,
				"active":        f.active,
				"catalog": map[string]any{
					"presets": []map[string]string{{"name": "launch-day", "description": "both flake", "source": "manifest"}},
					"services": map[string][]map[string]string{
						"hello": {{"name": "chaos", "description": "everything flakes", "source": "manifest"}},
						"sqs":   {{"name": "throttled", "description": "empty receives", "source": "adapter"}},
					},
				},
			})
		case http.MethodPost:
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			f.got = append(f.got, body)
			if body["name"] == "ambiguous" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"profile \"ambiguous\" is ambiguous — defined by services a, b"}`))
				return
			}
			if body["name"] != "" && body["service"] != "" {
				f.active[body["service"]] = body["name"]
			} else if body["name"] != "" {
				// Bare-name "chaos" resolves to the unique service that
				// defines it (hello) — like the real engine; anything else
				// behaves like a global preset.
				if body["name"] == "chaos" {
					f.active["hello"] = "chaos"
				} else {
					f.global = body["name"]
					f.active["hello"] = body["name"]
				}
			} else if body["service"] != "" {
				delete(f.active, body["service"])
			} else {
				f.active = map[string]string{}
				f.global = ""
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"active_global": f.global, "active": f.active})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
}

func runProfileCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	fake := &fakeProfileServer{active: map[string]string{}}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	full := append([]string{"profile", "--url", srv.URL, "--token", "tok"}, args...)
	root.SetArgs(full)
	err := root.Execute()
	return out.String(), err
}

func TestProfileList(t *testing.T) {
	out, err := runProfileCmd(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"active: none", "launch-day — both flake", "hello", "chaos (manifest)", "sqs", "throttled (adapter)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output missing %q:\n%s", want, out)
		}
	}
}

func TestProfileShow(t *testing.T) {
	out, err := runProfileCmd(t, "show", "throttled")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "service sqs (adapter) — empty receives") {
		t.Fatalf("show output = %q", out)
	}
	if _, err := runProfileCmd(t, "show", "nope"); err == nil || !strings.Contains(err.Error(), "stunt profile list") {
		t.Fatalf("show unknown err = %v", err)
	}
}

func TestProfileActivateDeactivate(t *testing.T) {
	out, err := runProfileCmd(t, "activate", "chaos")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `activated "chaos" on hello`) {
		t.Fatalf("activate output = %q", out)
	}
	out, err = runProfileCmd(t, "activate", "throttled", "--service", "sqs")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `activated "throttled" on sqs`) {
		t.Fatalf("targeted activate output = %q", out)
	}
	// The server's descriptive error reaches the CLI verbatim.
	_, err = runProfileCmd(t, "activate", "ambiguous")
	if err == nil || !strings.Contains(err.Error(), "defined by services a, b") {
		t.Fatalf("ambiguous err = %v", err)
	}
	out, err = runProfileCmd(t, "deactivate", "--service", "sqs")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "deactivated sqs") {
		t.Fatalf("deactivate output = %q", out)
	}
	out, err = runProfileCmd(t, "deactivate")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "deactivated all profiles") {
		t.Fatalf("deactivate-all output = %q", out)
	}
}

func TestProfileListJSON(t *testing.T) {
	out, err := runProfileCmd(t, "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Catalog struct {
			Services map[string][]struct {
				Name string `json:"name"`
			} `json:"services"`
		} `json:"catalog"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("decode --json output: %v\n%s", err, out)
	}
	if len(v.Catalog.Services["sqs"]) != 1 || v.Catalog.Services["sqs"][0].Name != "throttled" {
		t.Fatalf("json services = %+v", v.Catalog.Services)
	}
}
