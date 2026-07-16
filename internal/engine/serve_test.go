package engine

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stunt-adapters/stunt/internal/manifest"
	"github.com/stunt-adapters/stunt/internal/rules"
)

func TestServeMultipleServices(t *testing.T) {
	m := &manifest.Manifest{
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"a": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: "A"}}}}},
			"b": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200, Body: &rules.Body{Inline: "B"}}}}},
		},
	}
	e := New(m)
	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if len(addrs) != 2 {
		t.Fatalf("got %d servers, want 2", len(addrs))
	}
	time.Sleep(20 * time.Millisecond)
	bodyA := getURL(t, addrs["a"]+"/")
	bodyB := getURL(t, addrs["b"]+"/")
	if bodyA != `"A"` || bodyB != `"B"` {
		t.Fatalf("bodies = %q, %q", bodyA, bodyB)
	}
}

func getURL(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
