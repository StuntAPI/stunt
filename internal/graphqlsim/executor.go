// Package graphqlsim provides a thin GraphQL executor on top of the
// [gqlparser] AST. It parses SDL schemas, parses+validates query documents,
// and executes operations against a caller-provided [ResolverSet].
//
// The package is decoupled from Starlark: the engine adapts Starlark
// functions into [Resolver] closures. This mirrors how internal/grpcsim
// takes Go [grpcsim.Handler] functions rather than depending on the VM.
//
// # Resolver model
//
//   - Root fields (Query / Mutation): the [ResolverSet] maps
//     on_<field>(args) to a [Resolver]. A missing root resolver is a config
//     error.
//   - Object fields: resolve_<Type>_<field>(parent, args). If absent the
//     default resolver returns parent[fieldName] (dict key lookup).
//
// # Spec compliance (v1 scope)
//
// In: Query, Mutation, objects, lists, args (literal+variable+default),
// enums, __typename, named + inline fragments, @skip/@include, non-null +
// null propagation, built-in scalars, custom scalars (passthrough), partial
// results + errors array, DoS limits (depth/fields/timeout).
package graphqlsim

import (
	"context"
	"fmt"
	"time"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

// Default DoS limit values.
const (
	DefaultMaxDepth  = 10
	DefaultMaxFields = 1000
)

// Resolver resolves a single GraphQL field. parent is the parent object
// (nil for root fields). args is the resolved argument map (literals +
// variables + defaults already coerced by gqlparser's ArgumentMap).
type Resolver func(ctx context.Context, parent map[string]any, args map[string]any) (any, error)

// ResolverSet provides resolver lookup. For root fields (parentType is the
// Query or Mutation type), Lookup returns the on_<field> resolver. For
// object fields, Lookup returns the resolve_<Type>_<field> resolver or
// (found=false) to signal the default resolver.
type ResolverSet interface {
	// Lookup returns the resolver for the given parent type and field name.
	// found is false if no resolver is registered.
	Lookup(parentType, field string) (Resolver, bool)
}

// MapResolverSet is a simple in-memory ResolverSet backed by a map keyed
// by "Type.field". Useful for tests.
type MapResolverSet struct {
	Resolvers map[string]Resolver
}

// Lookup implements ResolverSet.
func (m *MapResolverSet) Lookup(parentType, field string) (Resolver, bool) {
	r, ok := m.Resolvers[parentType+"."+field]
	return r, ok
}

// Options controls DoS limits and other execution parameters.
type Options struct {
	// MaxDepth is the maximum selection-set nesting depth. Default 10.
	MaxDepth int

	// MaxFields is the maximum total number of fields across the entire
	// operation (after fragment expansion). Default 1000.
	MaxFields int

	// Timeout bounds the total execution time. If zero, no timeout is
	// applied (the caller's ctx still applies).
	Timeout time.Duration
}

// Location is a source position within a query document.
type Location struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// Error is a single GraphQL response error.
type Error struct {
	Message   string     `json:"message"`
	Path      []any      `json:"path,omitempty"`
	Locations []Location `json:"locations,omitempty"`
}

// Result is the GraphQL execution result.
type Result struct {
	Data   any     `json:"data"`
	Errors []Error `json:"errors,omitempty"`
}

// LoadSchema parses SDL bytes into a validated *ast.Schema.
func LoadSchema(sdl []byte) (*ast.Schema, error) {
	schema, err := gqlparser.LoadSchema(&ast.Source{Input: string(sdl)})
	if err != nil {
		return nil, fmt.Errorf("graphqlsim: load schema: %w", err)
	}
	return schema, nil
}

// Execute parses and validates a query, then executes it against the schema
// using the provided resolvers. The result always has Data (possibly null)
// and may have Errors. A returned error means the query could not be
// executed at all (parse/validation failure, DoS limit hit); in that case
// Result is nil.
func Execute(ctx context.Context, schema *ast.Schema, query string, variables map[string]any, operationName string, resolvers ResolverSet, opts Options) (*Result, error) {
	if variables == nil {
		variables = map[string]any{}
	}

	// Apply defaults.
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = DefaultMaxDepth
	}
	if opts.MaxFields <= 0 {
		opts.MaxFields = DefaultMaxFields
	}

	// Parse + validate the query against the schema.
	doc, gqlErr := gqlparser.LoadQuery(schema, query)
	if gqlErr != nil {
		return nil, fmt.Errorf("graphqlsim: %w", gqlErr)
	}

	// Select the operation.
	op, err := selectOperation(doc, operationName)
	if err != nil {
		return nil, err
	}

	// DoS limits: compute depth + field count before executing.
	depth, fields := analyzeOperation(op, doc.Fragments)
	if depth > opts.MaxDepth {
		return nil, fmt.Errorf("graphqlsim: query depth %d exceeds maximum %d", depth, opts.MaxDepth)
	}
	if fields > opts.MaxFields {
		return nil, fmt.Errorf("graphqlsim: query has %d fields, exceeds maximum %d", fields, opts.MaxFields)
	}

	// Apply timeout if configured.
	execCtx := ctx
	var cancel context.CancelFunc
	if opts.Timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	ex := &executor{
		schema:    schema,
		doc:       doc,
		resolvers: resolvers,
		variables: variables,
		rootType:  rootTypeFor(schema, op),
	}

	data, propagated := ex.executeSelectionSet(execCtx, ex.rootType, nil, op.SelectionSet, nil)

	var dataVal any = data
	if propagated {
		dataVal = nil
	}

	return &Result{
		Data:   dataVal,
		Errors: ex.errors,
	}, nil
}

// selectOperation finds the operation to run. If operationName is empty and
// there is exactly one operation, it is selected. If operationName is
// non-empty it must match.
func selectOperation(doc *ast.QueryDocument, operationName string) (*ast.OperationDefinition, error) {
	if len(doc.Operations) == 0 {
		return nil, fmt.Errorf("graphqlsim: no operations in query")
	}
	if operationName == "" {
		if len(doc.Operations) == 1 {
			return doc.Operations[0], nil
		}
		return nil, fmt.Errorf("graphqlsim: operation name required when multiple operations exist")
	}
	for i := range doc.Operations {
		if doc.Operations[i].Name == operationName {
			return doc.Operations[i], nil
		}
	}
	return nil, fmt.Errorf("graphqlsim: operation %q not found", operationName)
}

func rootTypeFor(schema *ast.Schema, op *ast.OperationDefinition) *ast.Definition {
	switch op.Operation {
	case ast.Mutation:
		return schema.Mutation
	case ast.Subscription:
		return schema.Subscription
	default:
		return schema.Query
	}
}

// --- executor ---

type executor struct {
	schema    *ast.Schema
	doc       *ast.QueryDocument
	resolvers ResolverSet
	variables map[string]any
	rootType  *ast.Definition
	errors    []Error
}

func (ex *executor) addError(message string, path []any, loc *ast.Position) {
	e := Error{Message: message}
	if len(path) > 0 {
		e.Path = append([]any{}, path...)
	}
	if loc != nil {
		e.Locations = []Location{{Line: loc.Line, Column: loc.Column}}
	}
	ex.errors = append(ex.errors, e)
}

// executeSelectionSet resolves all fields in a selection set against the
// given parent object and type. Returns (resultMap, propagated) where
// propagated=true means a non-null violation occurred and the result must be
// null.
func (ex *executor) executeSelectionSet(ctx context.Context, parentType *ast.Definition, source map[string]any, selections ast.SelectionSet, path []any) (map[string]any, bool) {
	// Collect fields (expand fragments, check directives).
	fields := ex.collectFields(selections, parentType)

	result := make(map[string]any, len(fields))
	for _, f := range fields {
		// __typename is a meta-field: return the object type name.
		if f.field.Name == "__typename" {
			result[f.field.Alias] = parentType.Name
			continue
		}

		fieldDef := parentType.Fields.ForName(f.field.Name)
		if fieldDef == nil {
			// Field doesn't exist on this type (shouldn't happen after
			// validation, but be safe).
			continue
		}

		fieldPath := append(append([]any{}, path...), f.field.Alias)

		// Build args.
		args := f.field.ArgumentMap(ex.variables)

		// Resolve.
		resolved, err := ex.resolveField(ctx, parentType.Name, f.field.Name, args, source)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				// Context deadline/cancellation — record as a real error.
				ex.addError(ctxErr.Error(), fieldPath, f.field.Position)
			} else {
				ex.addError(err.Error(), fieldPath, f.field.Position)
			}
			resolved = nil
		}

		// Complete the value.
		completed, propagated := ex.completeValue(ctx, fieldDef.Type, resolved, f.field.SelectionSet, fieldPath)
		if propagated {
			// Non-null violation: this object becomes null.
			return nil, true
		}
		result[f.field.Alias] = completed
	}
	return result, false
}

// resolveField looks up the resolver and calls it.
func (ex *executor) resolveField(ctx context.Context, parentTypeName, fieldName string, args map[string]any, parent map[string]any) (any, error) {
	// Introspection meta-fields (only valid on root Query type).
	if parentTypeName == ex.rootType.Name && ex.rootType == ex.schema.Query {
		switch fieldName {
		case "__schema":
			return introspectionSchema(ex.schema), nil
		case "__type":
			if name, ok := args["name"].(string); ok {
				return introspectionType(ex.schema, name), nil
			}
			return nil, nil
		}
	}

	isRoot := ex.isRootType(parentTypeName)
	resolver, found := ex.resolvers.Lookup(parentTypeName, fieldName)

	if isRoot {
		if !found {
			return nil, fmt.Errorf("no resolver for field %s.%s", parentTypeName, fieldName)
		}
		return resolver(ctx, nil, args)
	}

	// Object field: use resolver or default parent[key].
	if found {
		return resolver(ctx, parent, args)
	}
	// Default resolver.
	if parent != nil {
		if val, ok := parent[fieldName]; ok {
			return val, nil
		}
	}
	return nil, nil
}

func (ex *executor) isRootType(typeName string) bool {
	if ex.schema.Query != nil && ex.schema.Query.Name == typeName {
		return true
	}
	if ex.schema.Mutation != nil && ex.schema.Mutation.Name == typeName {
		return true
	}
	return false
}

// completeValue coerces a resolved Go value to the GraphQL type and recurses
// into selections for object/list types. Returns (value, propagated) where
// propagated=true means a non-null violation occurred.
func (ex *executor) completeValue(ctx context.Context, fieldType *ast.Type, result any, selections ast.SelectionSet, path []any) (any, bool) {
	// NonNull wrapper.
	if fieldType.NonNull {
		val, propagated := ex.completeValue(ctx, &ast.Type{NamedType: fieldType.NamedType, Elem: fieldType.Elem}, result, selections, path)
		if propagated {
			return nil, true
		}
		if val == nil {
			ex.addError("Cannot return null for non-nullable field", path, nil)
			return nil, true
		}
		return val, false
	}

	// Nullable type — null is fine.
	if result == nil {
		return nil, false
	}

	// List type.
	if fieldType.NamedType == "" && fieldType.Elem != nil {
		return ex.completeList(ctx, fieldType.Elem, result, selections, path)
	}

	// Named type.
	typeDef := ex.schema.Types[fieldType.Name()]
	if typeDef == nil {
		return nil, false
	}

	switch typeDef.Kind {
	case ast.Object, ast.Interface, ast.Union:
		runtimeType := ex.resolveRuntimeType(result, typeDef)
		objMap, _ := result.(map[string]any)
		objResult, propagated := ex.executeSelectionSet(ctx, runtimeType, objMap, selections, path)
		if propagated {
			// Non-null violation inside the object: this object becomes
			// null. If the field type is NonNull, the wrapper above will
			// re-propagate; otherwise the null is absorbed here.
			return nil, false
		}
		return objResult, false
	default:
		// Scalar or enum — serialize.
		return completeScalar(result), false
	}
}

// completeList handles list types. Each element is completed individually.
// If a non-null element is null, the entire list becomes null (propagation).
func (ex *executor) completeList(ctx context.Context, elemType *ast.Type, result any, selections ast.SelectionSet, path []any) (any, bool) {
	items := toAnySlice(result)
	if items == nil {
		return nil, false
	}

	completed := make([]any, len(items))
	for i, item := range items {
		elemPath := append(append([]any{}, path...), i)
		val, propagated := ex.completeValue(ctx, elemType, item, selections, elemPath)
		if propagated {
			// Non-null violation in an element: the list becomes null.
			// If the list type is NonNull, the wrapper above will
			// re-propagate; otherwise the null is absorbed here.
			return nil, false
		}
		completed[i] = val
	}
	return completed, false
}

// resolveRuntimeType determines the concrete object type for a resolved
// value. For object types, it's the type itself. For abstract types
// (interface/union), it checks the __typename key in the result.
func (ex *executor) resolveRuntimeType(result any, typeDef *ast.Definition) *ast.Definition {
	if typeDef.Kind == ast.Object {
		return typeDef
	}

	// Interface or Union: look for __typename in the result.
	if obj, ok := result.(map[string]any); ok {
		if tn, ok := obj["__typename"].(string); ok {
			if td := ex.schema.Types[tn]; td != nil {
				return td
			}
		}
	}

	// Fallback: if there's exactly one possible type, use it.
	possible := ex.schema.GetPossibleTypes(typeDef)
	if len(possible) == 1 {
		return possible[0]
	}

	return typeDef
}

// completeScalar serializes a Go value to a JSON-compatible GraphQL scalar.
// Custom scalars pass through as-is.
func completeScalar(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string, bool, int, int64, float32, float64:
		return x
	case []byte:
		return string(x)
	default:
		// Pass through anything that JSON can marshal (maps, slices, etc.).
		return x
	}
}

// toAnySlice converts a Go slice/array to []any. Returns nil if the value
// is not slice-like.
func toAnySlice(v any) []any {
	if v == nil {
		return nil
	}
	switch x := v.(type) {
	case []any:
		return x
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out
	case []int:
		out := make([]any, len(x))
		for i, n := range x {
			out[i] = n
		}
		return out
	case []int64:
		out := make([]any, len(x))
		for i, n := range x {
			out[i] = n
		}
		return out
	case []float64:
		out := make([]any, len(x))
		for i, n := range x {
			out[i] = n
		}
		return out
	case []bool:
		out := make([]any, len(x))
		for i, b := range x {
			out[i] = b
		}
		return out
	case []map[string]any:
		out := make([]any, len(x))
		for i, m := range x {
			out[i] = m
		}
		return out
	default:
		return nil
	}
}
