package rules

import (
	"bytes"
	"encoding/json"
	"fmt"
	"text/template"
	"time"
)

type requestView struct {
	Method  string
	Path    string
	Headers map[string]string
	Body    any
}

type tmplData struct {
	Request requestView
}

// renderTemplate executes tpl against the request context plus helper funcs.
// funcs: faker (Faker methods), now (time.Time), uuid (string).
func renderTemplate(tpl string, req Request, fk *Faker) ([]byte, error) {
	t, err := template.New("body").Funcs(template.FuncMap{
		"faker": func() *Faker { return fk },
		"now":   func() time.Time { return time.Now().UTC() },
		"uuid":  func() string { return fk.ID("") },
	}).Parse(tpl)
	if err != nil {
		return nil, fmt.Errorf("template parse: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, tmplData{Request: requestView{
		Method:  req.Method,
		Path:    req.Path,
		Headers: req.Headers,
		Body:    parseJSON(req.Body),
	}}); err != nil {
		return nil, fmt.Errorf("template exec: %w", err)
	}
	return buf.Bytes(), nil
}

// parseJSON parses raw JSON into a generic value; returns nil on empty/error.
func parseJSON(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}
