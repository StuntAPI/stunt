package graphqlsim

import (
	"github.com/vektah/gqlparser/v2/ast"
)

// introspectionSchema serializes the schema for __schema queries. The shape
// matches the GraphQL introspection output expected by GraphiQL and other
// GraphQL clients.
func introspectionSchema(schema *ast.Schema) map[string]any {
	types := make([]any, 0, len(schema.Types))
	for _, t := range schema.Types {
		// Skip introspection "builtin" types? No — GraphiQL expects all
		// types including introspection types (__Schema, __Type, etc.).
		types = append(types, serializeType(t))
	}

	result := map[string]any{
		"types": types,
	}
	if schema.Query != nil {
		result["queryType"] = map[string]any{"name": schema.Query.Name}
	}
	if schema.Mutation != nil {
		result["mutationType"] = map[string]any{"name": schema.Mutation.Name}
	}
	if schema.Subscription != nil {
		result["subscriptionType"] = map[string]any{"name": schema.Subscription.Name}
	}
	if len(schema.Directives) > 0 {
		directives := make([]any, 0, len(schema.Directives))
		for _, d := range schema.Directives {
			directives = append(directives, serializeDirective(d))
		}
		result["directives"] = directives
	}
	return result
}

// introspectionType returns the serialized type for __type(name:) queries.
func introspectionType(schema *ast.Schema, name string) map[string]any {
	if t, ok := schema.Types[name]; ok {
		return serializeType(t)
	}
	return nil
}

func serializeType(t *ast.Definition) map[string]any {
	result := map[string]any{
		"name": t.Name,
		"kind": string(t.Kind),
	}
	if t.Description != "" {
		result["description"] = t.Description
	}
	if len(t.Fields) > 0 {
		fields := make([]any, 0, len(t.Fields))
		for _, f := range t.Fields {
			if f.Name[:1] == "_" && f.Name != "__typename" {
				// Skip internal fields (prefixed with _) except __typename.
				continue
			}
			fields = append(fields, serializeField(f))
		}
		result["fields"] = fields
	}
	if len(t.EnumValues) > 0 {
		vals := make([]any, 0, len(t.EnumValues))
		for _, v := range t.EnumValues {
			ev := map[string]any{
				"name": v.Name,
			}
			if v.Description != "" {
				ev["description"] = v.Description
			}
			vals = append(vals, ev)
		}
		result["enumValues"] = vals
	}
	if len(t.Interfaces) > 0 {
		ifs := make([]any, 0, len(t.Interfaces))
		for _, i := range t.Interfaces {
			ifs = append(ifs, map[string]any{"name": i})
		}
		result["interfaces"] = ifs
	}
	if len(t.Types) > 0 {
		// Union member types.
		ts := make([]any, 0, len(t.Types))
		for _, tn := range t.Types {
			ts = append(ts, map[string]any{"name": tn})
		}
		result["possibleTypes"] = ts
	}
	return result
}

func serializeField(f *ast.FieldDefinition) map[string]any {
	result := map[string]any{
		"name": f.Name,
		"type": serializeTypeRef(f.Type),
	}
	if len(f.Arguments) > 0 {
		args := make([]any, 0, len(f.Arguments))
		for _, a := range f.Arguments {
			arg := map[string]any{
				"name": a.Name,
				"type": serializeTypeRef(a.Type),
			}
			args = append(args, arg)
		}
		result["args"] = args
	}
	return result
}

func serializeTypeRef(t *ast.Type) map[string]any {
	if t == nil {
		return nil
	}
	if t.NamedType != "" {
		return map[string]any{
			"kind":   "OBJECT",
			"name":   t.NamedType,
			"ofType": nil,
		}
	}
	// List type
	inner := serializeTypeRef(t.Elem)
	kind := "LIST"
	if t.NonNull {
		kind = "NON_NULL"
	}
	return map[string]any{
		"kind":   kind,
		"name":   nil,
		"ofType": inner,
	}
}

func serializeDirective(d *ast.DirectiveDefinition) map[string]any {
	result := map[string]any{
		"name": d.Name,
	}
	if len(d.Locations) > 0 {
		locs := make([]any, 0, len(d.Locations))
		for _, l := range d.Locations {
			locs = append(locs, string(l))
		}
		result["locations"] = locs
	}
	return result
}
