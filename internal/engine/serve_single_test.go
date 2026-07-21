package engine

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
	"stuntapi.com/stunt/internal/rules"
)

func TestServiceFromHost(t *testing.T) {
	cases := []struct {
		host, tld, want string
	}{
		{"myapp.localhost", "localhost", "myapp"},
		{"myapp.localhost:8443", "localhost", "myapp"},
		{"MYAPP.Localhost", "localhost", "myapp"},
		{"service.test", "test", "service"},
		{"unknown.localhost", "localhost", "unknown"},
		{"barehost", "localhost", "barehost"},
		{"a.b.localhost", "localhost", "a.b"},
	}
	for _, c := range cases {
		got := serviceFromHost(c.host, c.tld)
		if got != c.want {
			t.Errorf("serviceFromHost(%q, %q) = %q, want %q", c.host, c.tld, got, c.want)
		}
	}
}

func TestServeSingleDispatchesByHost(t *testing.T) {
	m := &manifest.Manifest{
		Version: 1,
		Network: manifest.Network{Mode: "subdomain", TLD: "localhost"},
		Services: map[string]manifest.Service{
			"alpha": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: "alpha"}}}}},
			"beta":  {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: "beta"}}}}},
		},
	}
	e, err := New(m)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr, shutdown, err := e.ServeSingle(ctx, "127.0.0.1:0", "localhost")
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown()
	time.Sleep(20 * time.Millisecond)

	// Request to alpha.localhost
	bodyA := getWithHost(t, addr+"/", "alpha.localhost")
	if bodyA != `"alpha"` {
		t.Errorf("alpha body = %q, want %q", bodyA, `"alpha"`)
	}

	// Request to beta.localhost
	bodyB := getWithHost(t, addr+"/", "beta.localhost")
	if bodyB != `"beta"` {
		t.Errorf("beta body = %q, want %q", bodyB, `"beta"`)
	}

	// Unknown host -> 404
	resp, err := http.Get(addr + "/")
	if err != nil {
		// http.Get will still work — the server responds to any request.
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unknown host status = %d, want 404", resp.StatusCode)
	}
}

func getWithHost(t *testing.T, url, host string) string {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
