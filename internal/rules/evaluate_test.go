package rules

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvaluateFirstMatchWins(t *testing.T) {
	rules := []Rule{
		{Name: "flaky", Match: Match{Method: "GET", Path: "/x"}, When: &When{Chance: 0}, Respond: Respond{Status: 503}},
		{Name: "ok", Match: Match{Method: "GET", Path: "/x"}, Respond: Respond{Status: 200, Body: &Body{Inline: map[string]any{"ok": true}}}},
	}
	d := Evaluate(Request{Method: "GET", Path: "/x"}, rules, NewRNG(1), NewFaker(1), "")
	if !d.Matched || d.Status != 200 {
		t.Fatalf("expected matched 200, got %+v", d)
	}
}

func TestEvaluateChanceInjectedFaultDeterministic(t *testing.T) {
	rules := []Rule{
		{Name: "flaky", Match: Match{Path: "/x"}, When: &When{Chance: 100}, Respond: Respond{Status: 500}},
		{Name: "ok", Match: Match{Path: "/x"}, Respond: Respond{Status: 200}},
	}
	d := Evaluate(Request{Path: "/x"}, rules, NewRNG(7), NewFaker(7), "")
	if !d.Matched || d.Status != 500 {
		t.Fatalf("expected injected 500, got %+v", d)
	}
}

func TestEvaluateNoMatch(t *testing.T) {
	rules := []Rule{{Match: Match{Path: "/x"}, Respond: Respond{Status: 200}}}
	d := Evaluate(Request{Path: "/y"}, rules, NewRNG(1), NewFaker(1), "")
	if d.Matched {
		t.Fatalf("expected no match, got %+v", d)
	}
}

func TestEvaluateTimeoutBehavior(t *testing.T) {
	rules := []Rule{{Match: Match{Path: "/x"}, Respond: Respond{Behavior: "timeout", LatencyMS: 5}}}
	d := Evaluate(Request{Path: "/x"}, rules, NewRNG(1), NewFaker(1), "")
	if !d.Matched || !d.Timeout || d.LatencyMS != 5 {
		t.Fatalf("expected timeout decision, got %+v", d)
	}
}

func TestEvaluateBodyFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "body.json")
	if err := os.WriteFile(path, []byte(`{"from":"file"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rules := []Rule{{Match: Match{Path: "/x"}, Respond: Respond{Status: 200, Body: &Body{File: "body.json"}}}}
	d := Evaluate(Request{Path: "/x"}, rules, NewRNG(1), NewFaker(1), dir)
	if !bytes.Equal(d.BodyBytes, []byte(`{"from":"file"}`)) {
		t.Fatalf("body bytes = %q", d.BodyBytes)
	}
}

func TestEvaluateBodyFileTraversalRejected(t *testing.T) {
	baseDir := t.TempDir()

	// Create a secret file in baseDir's parent to prove it is NOT read.
	absBase, _ := filepath.Abs(baseDir)
	parent := filepath.Dir(absBase)
	secretPath := filepath.Join(parent, "traversal_secret.json")
	if err := os.WriteFile(secretPath, []byte(`{"secret": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(secretPath) })

	rules := []Rule{{Match: Match{Path: "/x"}, Respond: Respond{Status: 200, Body: &Body{File: "../traversal_secret.json"}}}}
	d := Evaluate(Request{Path: "/x"}, rules, NewRNG(1), NewFaker(1), baseDir)
	body := string(d.BodyBytes)
	if strings.Contains(body, `"secret": true`) {
		t.Fatalf("PATH TRAVERSAL: outside file was read! body = %q", body)
	}
	if !strings.Contains(body, "path rejected") && !strings.Contains(body, "body file error") {
		t.Fatalf("expected a rejection message, got %q", body)
	}
}

func TestEvaluateBodyFileTraversalDeepRejected(t *testing.T) {
	baseDir := t.TempDir()

	rules := []Rule{{Match: Match{Path: "/x"}, Respond: Respond{Status: 200, Body: &Body{File: "../../etc/passwd"}}}}
	d := Evaluate(Request{Path: "/x"}, rules, NewRNG(1), NewFaker(1), baseDir)
	body := string(d.BodyBytes)
	if strings.Contains(body, "root:") {
		t.Fatalf("PATH TRAVERSAL: /etc/passwd was read! body = %q", body)
	}
	if !strings.Contains(body, "path rejected") && !strings.Contains(body, "body file error") {
		t.Fatalf("expected a rejection message, got %q", body)
	}
}

func TestEvaluateBodyFileAbsoluteRejected(t *testing.T) {
	baseDir := t.TempDir()

	// An absolute path outside baseDir should be rejected.
	tmp := filepath.Join(t.TempDir(), "abs.json")
	if err := os.WriteFile(tmp, []byte(`{"from": "abs"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	rules := []Rule{{Match: Match{Path: "/x"}, Respond: Respond{Status: 200, Body: &Body{File: tmp}}}}
	d := Evaluate(Request{Path: "/x"}, rules, NewRNG(1), NewFaker(1), baseDir)
	body := string(d.BodyBytes)
	if strings.Contains(body, `"from": "abs"`) {
		t.Fatalf("PATH TRAVERSAL: absolute outside file was read! body = %q", body)
	}
}

func TestEvaluateBodyTemplate(t *testing.T) {
	rules := []Rule{{
		Match:   Match{Method: "POST", Path: "/x"},
		Respond: Respond{Status: 201, Body: &Body{Template: `{"id":"{{ faker.ID "ch" }}","amount":{{ .Request.Body.amount }},"name":"{{ .Request.Body.name }}"}`}},
	}}
	req := Request{Method: "POST", Path: "/x", Body: []byte(`{"amount":5000,"name":"Sam"}`)}
	d := Evaluate(req, rules, NewRNG(1), NewFaker(1), "")
	if !d.Matched || d.Status != 201 {
		t.Fatalf("expected matched 201, got %+v", d)
	}
	s := string(d.BodyBytes)
	if !strings.Contains(s, `"amount":5000`) || !strings.Contains(s, `"name":"Sam"`) || !strings.Contains(s, `"id":"ch_`) {
		t.Fatalf("template not rendered into body: %q", s)
	}
}

func TestEvaluateWhenExprTrue(t *testing.T) {
	rules := []Rule{
		{Match: Match{Path: "/x"}, When: &When{Expr: "request.body.amount > 1000"}, Respond: Respond{Status: 200, Body: &Body{Inline: "big"}}},
		{Match: Match{Path: "/x"}, Respond: Respond{Status: 200, Body: &Body{Inline: "small"}}},
	}
	d := Evaluate(Request{Path: "/x", Body: []byte(`{"amount":5000}`)}, rules, NewRNG(1), NewFaker(1), "")
	if string(d.BodyBytes) != `"big"` {
		t.Fatalf("expr-true should match first rule; got %q", d.BodyBytes)
	}
}

func TestEvaluateWhenExprFalse(t *testing.T) {
	rules := []Rule{
		{Match: Match{Path: "/x"}, When: &When{Expr: "request.body.amount > 1000"}, Respond: Respond{Status: 200, Body: &Body{Inline: "big"}}},
		{Match: Match{Path: "/x"}, Respond: Respond{Status: 200, Body: &Body{Inline: "small"}}},
	}
	d := Evaluate(Request{Path: "/x", Body: []byte(`{"amount":50}`)}, rules, NewRNG(1), NewFaker(1), "")
	if string(d.BodyBytes) != `"small"` {
		t.Fatalf("expr-false should fall through; got %q", d.BodyBytes)
	}
}

func TestEvaluateChanceAndExprCombine(t *testing.T) {
	// chance 100 + expr true -> fires
	rules := []Rule{
		{Match: Match{Path: "/x"}, When: &When{Chance: 100, Expr: "request.method == \"POST\""}, Respond: Respond{Status: 200, Body: &Body{Inline: "hit"}}},
		{Match: Match{Path: "/x"}, Respond: Respond{Status: 200, Body: &Body{Inline: "miss"}}},
	}
	d := Evaluate(Request{Method: "POST", Path: "/x"}, rules, NewRNG(1), NewFaker(1), "")
	if string(d.BodyBytes) != `"hit"` {
		t.Fatalf("chance+expr both pass should fire; got %q", d.BodyBytes)
	}
}
