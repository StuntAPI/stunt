package sdkmap

import (
	"testing"
)

const oas3JSON = `{
  "openapi": "3.0.4",
  "info": {"version": "1.2.3"},
  "paths": {
    "/users": {"get": {}, "post": {}},
    "/users/{id}": {"get": {}, "patch": {}, "parameters": [{"name": "id"}]},
    "/users/{id}:run": {"post": {}}
  }
}`

const oas3YAML = `openapi: 3.1.2
info:
  version: 9.9
paths:
  /v3/mail/send:
    post: {}
  /users/{user_id}/messages:
    get: {}
`

// Swagger 2 shape: Azure keeps everything under x-ms-paths and puts the
// account in a parameterized host, so paths can be empty.
const swagger2XMS = `swagger: '2.0'
info:
  version: "2021-05"
paths: {}
x-ms-paths:
  /{queueName}/messages?api-version=2021-05:
    post: {}
    delete: {}
  /{entityName}:
    get: {}
    put: {}
    delete: {}
`

func TestParseOpenAPIJSON(t *testing.T) {
	routes, version, err := ParseOpenAPI([]byte(oas3JSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if version != "1.2.3" {
		t.Errorf("version = %q", version)
	}
	// The bare `parameters` key must not become a route.
	if len(routes) != 5 {
		t.Fatalf("routes = %v", routes)
	}
	if _, ok := routeKeySet(routes)["GET /users/{id}:run"]; ok {
		t.Errorf(":verb route miscounted: %v", routes)
	}
	if _, ok := routeKeySet(routes)["POST /users/{id}:run"]; !ok {
		t.Errorf("missing :verb route: %v", routes)
	}
}

func TestParseOpenAPIYAML(t *testing.T) {
	routes, version, err := ParseOpenAPI([]byte(oas3YAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if version != "9.9" {
		t.Errorf("version = %q", version)
	}
	if _, ok := routeKeySet(routes)["POST /v3/mail/send"]; !ok {
		t.Errorf("missing mail route: %v", routes)
	}
}

func TestParseOpenAPISwagger2XMSPaths(t *testing.T) {
	routes, version, err := ParseOpenAPI([]byte(swagger2XMS))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if version != "2021-05" {
		t.Errorf("version = %q", version)
	}
	// x-ms-paths keys keep their query suffix — Azure distinguishes real
	// operations by it, so the route table must too (matching, not
	// inventory, is where queries stop mattering).
	if _, ok := routeKeySet(routes)["POST /{queueName}/messages?api-version=2021-05"]; !ok {
		t.Errorf("missing queue route: %v", routes)
	}
	if _, ok := routeKeySet(routes)["GET /{entityName}"]; !ok {
		t.Errorf("missing entity route: %v", routes)
	}
}

func TestParseOpenAPIRejectsNonSpec(t *testing.T) {
	if _, _, err := ParseOpenAPI([]byte(`{"hello": "world"}`)); err == nil {
		t.Fatal("non-spec JSON accepted")
	}
	if _, _, err := ParseOpenAPI([]byte("key: value\nother: 1\n")); err == nil {
		t.Fatal("non-spec YAML accepted")
	}
}

func routeKeySet(routes []Route) map[string]bool {
	m := map[string]bool{}
	for _, r := range routes {
		m[r.Method+" "+r.Path] = true
	}
	return m
}
