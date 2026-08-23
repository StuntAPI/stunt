package sdkmap

import "testing"

const lexiconProcedure = `{
  "lexicon": 1,
  "id": "com.atproto.server.createSession",
  "defs": {
    "main": {
      "type": "procedure",
      "description": "Create an authentication session.",
      "input": {"encoding": "application/json"}
    }
  }
}`

const lexiconQuery = `{
  "lexicon": 1,
  "id": "app.bsky.feed.searchPosts",
  "defs": {
    "main": {
      "type": "query",
      "description": "Find posts matching search criteria.",
      "parameters": {"type": "params", "required": ["q"]}
    }
  }
}`

func TestParseLexicon(t *testing.T) {
	// Non-endpoint main defs and main-less shared-type files yield no route.
	for name, body := range map[string]string{
		"record":         `{"lexicon":1,"id":"app.bsky.feed.post","defs":{"main":{"type":"record"}}}`,
		"subscription":   `{"lexicon":1,"id":"com.atproto.sync.subscribeRepos","defs":{"main":{"type":"subscription"}}}`,
		"permission-set": `{"lexicon":1,"id":"app.bsky.authCreatePosts","defs":{"main":{"type":"permission-set"}}}`,
		"object":         `{"lexicon":1,"id":"app.bsky.embed.defs","defs":{"main":{"type":"object"}}}`,
		"no main":        `{"lexicon":1,"id":"app.bsky.actor.defs","defs":{"profileView":{"type":"object"}}}`,
	} {
		routes, err := ParseLexicon([]byte(body))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(routes) != 0 {
			t.Errorf("%s: %d routes, want 0", name, len(routes))
		}
	}

	for _, c := range []struct{ body, want string }{
		{lexiconProcedure, "POST /xrpc/com.atproto.server.createSession"},
		{lexiconQuery, "GET /xrpc/app.bsky.feed.searchPosts"},
	} {
		routes, err := ParseLexicon([]byte(c.body))
		if err != nil {
			t.Fatal(err)
		}
		if len(routes) != 1 || routes[0].Method+" "+routes[0].Path != c.want {
			t.Errorf("got %+v, want [%s]", routes, c.want)
		}
	}
}

func TestParseLexiconGuards(t *testing.T) {
	cases := []struct{ name, body string }{
		{"not json", `namespace files`},
		{"no id", `{"lexicon":1,"defs":{"main":{"type":"query"}}}`},
		{"unknown main type", `{"lexicon":1,"id":"x.y.z","defs":{"main":{"type":"subprocedure"}}}`},
	}
	for _, c := range cases {
		if _, err := ParseLexicon([]byte(c.body)); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}
