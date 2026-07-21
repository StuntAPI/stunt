package engine

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
	"stuntapi.com/stunt/internal/rules"
)

func TestServeTemplateAndExpr(t *testing.T) {
	m := &manifest.Manifest{
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"api": {Rules: []rules.Rule{
				// big charges get an id from a template; small ones echo amount
				{Match: rules.Match{Method: "POST", Path: "/charge"},
					When:    &rules.When{Expr: "request.body.amount > 1000"},
					Respond: rules.Respond{Status: 201, Body: &rules.Body{Template: `{"id":"{{ faker.ID "ch" }}","amount":{{ .Request.Body.amount }}}`}}},
				{Match: rules.Match{Method: "POST", Path: "/charge"},
					Respond: rules.Respond{Status: 200, Body: &rules.Body{Template: `{"amount":{{ .Request.Body.amount }},"small":true}`}}},
			}},
		},
	}
	e, err := New(m)
	if err != nil {
		t.Fatal(err)
	}
	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	time.Sleep(20 * time.Millisecond)

	big := post(t, addrs["api"]+"/charge", `{"amount":5000}`)
	if !strings.Contains(big, `"id":"ch_`) || !strings.Contains(big, `"amount":5000`) {
		t.Fatalf("big charge body wrong: %q", big)
	}
	small := post(t, addrs["api"]+"/charge", `{"amount":50}`)
	if !strings.Contains(small, `"amount":50`) || !strings.Contains(small, `"small":true`) {
		t.Fatalf("small charge body wrong: %q", small)
	}
}

func post(t *testing.T, url, body string) string {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
