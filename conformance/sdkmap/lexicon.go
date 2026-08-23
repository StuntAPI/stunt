package sdkmap

import (
	"encoding/json"
	"fmt"
)

// lexiconDoc is the minimal projection of an AT Protocol lexicon schema:
// the NSID and the type of the primary ("main") definition. Parameters,
// input/output schemas and descriptions are irrelevant to route inventory.
type lexiconDoc struct {
	Lexicon int               `json:"lexicon"`
	ID      string            `json:"id"`
	Defs    map[string]lexDef `json:"defs"`
}

type lexDef struct {
	Type string `json:"type"`
}

// ParseLexicon extracts the HTTP route from one AT Protocol lexicon file.
// XRPC maps defs.main.type onto the verb — "procedure" -> POST,
// "query" -> GET — and serves it at /xrpc/<nsid> (the adapter manifest's
// own path spelling). Lexicons whose main def is a record, object,
// subscription or permission-set are data schemas or streams, not
// endpoints: no route, no error. Files with no main def (the *_defs.json
// shared-type files) likewise carry none. Anything else — a query or
// procedure without an id, or an unrecognized main type — is malformed and
// fails loud, so an upstream lexicon change cannot silently thin the
// table.
func ParseLexicon(data []byte) ([]Route, error) {
	var doc lexiconDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("sdkmap: not lexicon JSON: %w", err)
	}
	if doc.Lexicon == 0 || doc.ID == "" {
		return nil, fmt.Errorf("sdkmap: lexicon carries no lexicon/id field")
	}
	main, ok := doc.Defs["main"]
	if !ok {
		return nil, nil // *_defs.json shared-type file: no endpoint
	}
	switch main.Type {
	case "procedure":
		return []Route{{Method: "POST", Path: "/xrpc/" + doc.ID}}, nil
	case "query":
		return []Route{{Method: "GET", Path: "/xrpc/" + doc.ID}}, nil
	case "record", "object", "subscription", "permission-set":
		return nil, nil
	}
	return nil, fmt.Errorf("sdkmap: lexicon %q: unknown main def type %q", doc.ID, main.Type)
}
