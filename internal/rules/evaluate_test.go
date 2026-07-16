package rules

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEvaluateFirstMatchWins(t *testing.T) {
	rules := []Rule{
		{Name: "flaky", Match: Match{Method: "GET", Path: "/x"}, When: &When{Chance: 0}, Respond: Respond{Status: 503}},
		{Name: "ok", Match: Match{Method: "GET", Path: "/x"}, Respond: Respond{Status: 200, Body: &Body{Inline: map[string]any{"ok": true}}}},
	}
	d := Evaluate(Request{Method: "GET", Path: "/x"}, rules, NewRNG(1), "")
	if !d.Matched || d.Status != 200 {
		t.Fatalf("expected matched 200, got %+v", d)
	}
}

func TestEvaluateChanceInjectedFaultDeterministic(t *testing.T) {
	rules := []Rule{
		{Name: "flaky", Match: Match{Path: "/x"}, When: &When{Chance: 100}, Respond: Respond{Status: 500}},
		{Name: "ok", Match: Match{Path: "/x"}, Respond: Respond{Status: 200}},
	}
	d := Evaluate(Request{Path: "/x"}, rules, NewRNG(7), "")
	if !d.Matched || d.Status != 500 {
		t.Fatalf("expected injected 500, got %+v", d)
	}
}

func TestEvaluateNoMatch(t *testing.T) {
	rules := []Rule{{Match: Match{Path: "/x"}, Respond: Respond{Status: 200}}}
	d := Evaluate(Request{Path: "/y"}, rules, NewRNG(1), "")
	if d.Matched {
		t.Fatalf("expected no match, got %+v", d)
	}
}

func TestEvaluateTimeoutBehavior(t *testing.T) {
	rules := []Rule{{Match: Match{Path: "/x"}, Respond: Respond{Behavior: "timeout", LatencyMS: 5}}}
	d := Evaluate(Request{Path: "/x"}, rules, NewRNG(1), "")
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
	d := Evaluate(Request{Path: "/x"}, rules, NewRNG(1), dir)
	if !bytes.Equal(d.BodyBytes, []byte(`{"from":"file"}`)) {
		t.Fatalf("body bytes = %q", d.BodyBytes)
	}
}
