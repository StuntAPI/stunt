package sdkmap

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// oasDoc is the minimal projection of an OpenAPI 2/3.x document: the
// version string and the path table. Everything else (schemas, params,
// responses) is irrelevant to route inventory.
type oasDoc struct {
	OpenAPI string `json:"openapi" yaml:"openapi"`
	Swagger string `json:"swagger" yaml:"swagger"`
	Info    struct {
		Version string `json:"version" yaml:"version"`
	} `json:"info" yaml:"info"`
	Paths   map[string]oasPathItem `json:"paths" yaml:"paths"`
	MSPaths map[string]oasPathItem `json:"x-ms-paths" yaml:"x-ms-paths"`
}

// oasPathItem maps the HTTP verb keys to their operation objects. Only the
// keys matter — the values are whatever the spec carries.
type oasPathItem map[string]any

var oasVerbs = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"patch": true, "head": true, "options": true,
}

// ParseOpenAPI extracts the route inventory from an OpenAPI 2.0 or 3.x
// document (JSON or YAML). Deprecated operations are included — real
// clients still call them. Azure's x-ms-paths variants (same path,
// distinguished by query params) merge into the plain path since matching
// ignores query strings.
func ParseOpenAPI(data []byte) ([]Route, string, error) {
	var doc oasDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, "", fmt.Errorf("sdkmap: not OpenAPI JSON or YAML: %w", err)
		}
	}
	if doc.OpenAPI == "" && doc.Swagger == "" {
		return nil, "", fmt.Errorf("sdkmap: no openapi/swagger version field")
	}
	version := doc.Info.Version

	seen := map[string]bool{}
	var out []Route
	add := func(path string, item oasPathItem) {
		for verb := range item {
			v := strings.ToUpper(verb)
			if !oasVerbs[verb] {
				continue
			}
			k := v + " " + path
			if !seen[k] {
				seen[k] = true
				out = append(out, Route{Method: v, Path: path})
			}
		}
	}
	for p, item := range doc.Paths {
		add(p, item)
	}
	for p, item := range doc.MSPaths {
		add(p, item)
	}
	if len(out) == 0 {
		return nil, version, fmt.Errorf("sdkmap: spec %q carries 0 routes — layout changed?", version)
	}
	return out, version, nil
}
