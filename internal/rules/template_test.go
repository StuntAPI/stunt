package rules

import (
	"strings"
	"testing"
)

func TestRenderTemplateRequestAndFaker(t *testing.T) {
	req := Request{
		Method: "POST",
		Path:   "/v1/charges",
		Body:   []byte(`{"amount": 5000, "name": "Sam"}`),
	}
	fk := NewFaker(1)
	out, err := renderTemplate(`hi {{ .Request.Body.name }} amount={{ .Request.Body.amount }} {{ faker.Email }} {{ uuid }}`, req, fk)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "hi Sam amount=5000") {
		t.Fatalf("request fields not rendered: %q", s)
	}
	if !strings.Contains(s, "@") {
		t.Fatalf("faker.Email not rendered: %q", s)
	}
}

func TestRenderTemplateNonJSONBody(t *testing.T) {
	req := Request{Method: "GET", Path: "/x", Body: []byte("not json")}
	out, err := renderTemplate(`ok`, req, NewFaker(1))
	if err != nil || string(out) != "ok" {
		t.Fatalf("expected 'ok', got %q err=%v", out, err)
	}
}
