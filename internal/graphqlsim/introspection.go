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
		types = append(types, serializeType(schema, t))
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
			directives = append(directives, serializeDirective(schema, d))
		}
		result["directives"] = directives
	}
	return result
}

// introspectionType returns the serialized type for __type(name:) queries.
func introspectionType(schema *ast.Schema, name string) map[string]any {
	if t, ok := schema.Types[name]; ok {
		return serializeType(schema, t)
	}
	return nil
}

func serializeType(schema *ast.Schema, t *ast.Definition) map[string]any {
	result := map[string]any{
		"name": t.Name,
		"kind": string(t.Kind),
	}
	if t.Description != "" {
		result["description"] = t.Description
	}
	// Input objects expose inputFields (not fields).
	if t.Kind == ast.InputObject {
		if len(t.Fields) > 0 {
			inputFields := make([]any, 0, len(t.Fields))
			for _, f := range t.Fields {
				inputFields = append(inputFields, serializeInputValue(schema, f))
			}
			result["inputFields"] = inputFields
		}
	} else if len(t.Fields) > 0 {
		fields := make([]any, 0, len(t.Fields))
		for _, f := range t.Fields {
			if f.Name[:1] == "_" && f.Name != "__typename" {
				// Skip internal fields (prefixed with _) except __typename.
				continue
			}
			fields = append(fields, serializeField(schema, f))
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

func serializeField(schema *ast.Schema, f *ast.FieldDefinition) map[string]any {
	result := map[string]any{
		"name": f.Name,
		"type": serializeTypeRef(schema, f.Type),
	}
	if len(f.Arguments) > 0 {
		args := make([]any, 0, len(f.Arguments))
		for _, a := range f.Arguments {
			arg := map[string]any{
				"name": a.Name,
				"type": serializeTypeRef(schema, a.Type),
			}
			args = append(args, arg)
		}
		result["args"] = args
	}
	return result
}

// serializeInputValue serializes a field/argument for use as an __InputValue
// (input object fields and field arguments).
func serializeInputValue(schema *ast.Schema, f *ast.FieldDefinition) map[string]any {
	result := map[string]any{
		"name": f.Name,
		"type": serializeTypeRef(schema, f.Type),
	}
	if f.Description != "" {
		result["description"] = f.Description
	}
	if f.DefaultValue != nil {
		result["defaultValue"] = f.DefaultValue.String()
	}
	return result
}

// serializeTypeRef serializes a type reference for introspection. The kind
// is determined by looking up the actual type definition in the schema.
// NonNull is checked FIRST so that NonNull named types (e.g. String!) get a
// NON_NULL wrapper rather than losing it.
func serializeTypeRef(schema *ast.Schema, t *ast.Type) map[string]any {
	if t == nil {
		return nil
	}
	// Check NonNull FIRST — before NamedType.
	if t.NonNull {
		inner := serializeTypeRef(schema, &ast.Type{
			NamedType: t.NamedType,
			Elem:      t.Elem,
		})
		return map[string]any{
			"kind":   "NON_NULL",
			"name":   nil,
			"ofType": inner,
		}
	}
	// Named type: look up actual kind from schema.
	if t.NamedType != "" {
		kind := "SCALAR" // default for built-in scalars not in schema.Types
		if def, ok := schema.Types[t.NamedType]; ok {
			kind = string(def.Kind)
		}
		return map[string]any{
			"kind":   kind,
			"name":   t.NamedType,
			"ofType": nil,
		}
	}
	// List type.
	inner := serializeTypeRef(schema, t.Elem)
	return map[string]any{
		"kind":   "LIST",
		"name":   nil,
		"ofType": inner,
	}
}

func serializeDirective(schema *ast.Schema, d *ast.DirectiveDefinition) map[string]any {
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
	if len(d.Arguments) > 0 {
		args := make([]any, 0, len(d.Arguments))
		for _, a := range d.Arguments {
			arg := map[string]any{
				"name": a.Name,
				"type": serializeTypeRef(schema, a.Type),
			}
			args = append(args, arg)
		}
		result["args"] = args
	}
	return result
}
