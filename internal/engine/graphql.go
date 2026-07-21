package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/vektah/gqlparser/v2/ast"
	sk "go.starlark.net/starlark"
	"stuntapi.com/stunt/internal/adapter"
	"stuntapi.com/stunt/internal/graphqlsim"
	"stuntapi.com/stunt/internal/starlark"
)

// graphqlSchemaCache caches the parsed GraphQL schema per adapter directory.
// Schemas are parsed once at first request and reused. A load error is
// cached too so we fail fast on every subsequent request.
type graphqlSchemaCache struct {
	schema  *ast.Schema
	loadErr error
}

// graphqlPath returns the configured GraphQL endpoint path, defaulting to
// "/graphql".
func graphqlPath(st *serviceState) string {
	if st.adapter.Graphql != nil && st.adapter.Graphql.Path != "" {
		return st.adapter.Graphql.Path
	}
	return "/graphql"
}

// loadGraphqlSchema parses the adapter's SDL into a *ast.Schema, caching
// the result on the serviceState for reuse.
func (e *Engine) loadGraphqlSchema(st *serviceState) (*ast.Schema, error) {
	// Check cache on serviceState.
	st.mu.Lock()
	cached, ok := st.gqlSchema.(*graphqlSchemaCache)
	st.mu.Unlock()
	if ok {
		return cached.schema, cached.loadErr
	}

	sdlBytes, err := st.adapter.SchemaSDL()
	if err != nil {
		cached = &graphqlSchemaCache{loadErr: err}
	} else {
		schema, err := graphqlsim.LoadSchema(sdlBytes)
		if err != nil {
			cached = &graphqlSchemaCache{loadErr: err}
		} else {
			cached = &graphqlSchemaCache{schema: schema}
		}
	}

	st.mu.Lock()
	st.gqlSchema = cached
	st.mu.Unlock()
	return cached.schema, cached.loadErr
}

// graphqlRequest represents the JSON body of a GraphQL POST request.
type graphqlRequest struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
	// Extensions and other fields are ignored.
}

// handleGraphql processes a GraphQL HTTP request (POST or GET).
func (e *Engine) handleGraphql(w http.ResponseWriter, r *http.Request, st *serviceState) {
	schema, err := e.loadGraphqlSchema(st)
	if err != nil {
		writeGraphqlError(w, http.StatusInternalServerError, fmt.Sprintf("schema load error: %v", err))
		return
	}

	var gqlReq graphqlRequest

	switch r.Method {
	case http.MethodPost:
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			writeGraphqlError(w, http.StatusBadRequest, fmt.Sprintf("read body: %v", err))
			return
		}
		if err := json.Unmarshal(body, &gqlReq); err != nil {
			writeGraphqlError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
			return
		}
	case http.MethodGet:
		gqlReq.Query = r.URL.Query().Get("query")
		gqlReq.OperationName = r.URL.Query().Get("operationName")
		if vs := r.URL.Query().Get("variables"); vs != "" {
			if err := json.Unmarshal([]byte(vs), &gqlReq.Variables); err != nil {
				writeGraphqlError(w, http.StatusBadRequest, fmt.Sprintf("invalid variables parameter: %v", err))
				return
			}
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		writeGraphqlError(w, http.StatusMethodNotAllowed, "GraphQL endpoint supports GET and POST only")
		return
	}

	if gqlReq.Query == "" {
		writeGraphqlError(w, http.StatusBadRequest, "missing query")
		return
	}

	resolverSet := &starlarkResolverSet{
		st:           st,
		spec:         st.adapter.Graphql,
		queryName:    schema.Query.Name,
		mutationName: "",
	}
	if schema.Mutation != nil {
		resolverSet.mutationName = schema.Mutation.Name
	}

	result, err := graphqlsim.Execute(r.Context(), schema, gqlReq.Query, gqlReq.Variables, gqlReq.OperationName, resolverSet, graphqlsim.Options{})
	if err != nil {
		// Parse/validation error or DoS limit — return a 400 with the error
		// as a GraphQL error response.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		data, _ := json.Marshal(map[string]any{
			"errors": []map[string]any{{"message": err.Error()}},
		})
		_, _ = w.Write(data)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	data, _ := json.Marshal(result)
	_, _ = w.Write(data)
}

// writeGraphqlError writes a single-error GraphQL JSON response.
func writeGraphqlError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	data, _ := json.Marshal(map[string]any{
		"errors": []map[string]any{{"message": message}},
	})
	_, _ = w.Write(data)
}

// --- Starlark resolver adapter ---

// starlarkResolverSet implements graphqlsim.ResolverSet by dispatching to
// convention-named Starlark functions. Root fields use on_<field>(args);
// object fields use resolve_<Type>_<field>(parent, args).
type starlarkResolverSet struct {
	st           *serviceState
	spec         *adapter.GraphqlSpec
	queryName    string
	mutationName string
}

// Lookup implements graphqlsim.ResolverSet.
func (rs *starlarkResolverSet) Lookup(parentType, field string) (graphqlsim.Resolver, bool) {
	scriptPath, _ := adapter.SplitHandler(rs.spec.Resolvers)

	vm, err := rs.st.getOrLoadVM(scriptPath)
	if err != nil {
		// Return a resolver that always fails — better than panicking.
		return func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
			return nil, fmt.Errorf("load resolver script: %w", err)
		}, true
	}

	isRoot := rs.isRootType(parentType)

	var fnName string
	if isRoot {
		fnName = "on_" + field
	} else {
		fnName = "resolve_" + parentType + "_" + field
	}

	if !vm.Has(fnName) {
		return nil, false
	}

	return func(ctx context.Context, parent map[string]any, args map[string]any) (any, error) {
		return rs.callResolver(vm, fnName, parent, args)
	}, true
}

func (rs *starlarkResolverSet) isRootType(typeName string) bool {
	return typeName == rs.queryName || typeName == rs.mutationName
}

// callResolver invokes the Starlark resolver function and converts the result.
func (rs *starlarkResolverSet) callResolver(vm *starlark.VM, fnName string, parent map[string]any, args map[string]any) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("resolver panic: %v", r)
		}
	}()

	// Build a single Starlark dict carrying both parent and args.
	callArg, err := starlark.GoToStarlark(map[string]any{
		"parent": parent,
		"args":   args,
	})
	if err != nil {
		return nil, fmt.Errorf("resolver args: %w", err)
	}

	rawResult, err := vm.CallRaw(fnName, callArg)
	if err != nil {
		return nil, err
	}

	return starlarkResultToGo(rawResult)
}

// starlarkResultToGo converts a raw Starlark value returned by a resolver
// into a Go value. A respond(...) dict (with key "body") yields the body;
// any other value is converted directly via starlark.ValueToGo.
func starlarkResultToGo(v sk.Value) (any, error) {
	if v == nil {
		return nil, nil
	}
	// Check if it's a respond(...) dict with a "body" key.
	if d, ok := v.(*sk.Dict); ok {
		bodyVal, found, _ := d.Get(sk.String("body"))
		if found {
			// Extract the body value. None body → nil.
			if _, isNone := bodyVal.(sk.NoneType); isNone {
				return nil, nil
			}
			return starlark.ValueToGo(bodyVal)
		}
		// It's a plain dict (not from respond) — convert as a map.
		return starlark.ValueToGo(v)
	}
	// None → nil.
	if _, ok := v.(sk.NoneType); ok {
		return nil, nil
	}
	return starlark.ValueToGo(v)
}
