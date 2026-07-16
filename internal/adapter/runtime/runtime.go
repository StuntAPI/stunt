// Package runtime wires the primitives (Collection + KV) stores into Starlark
// handler scripts as builtins, enabling stateful adapters that persist data
// across requests.
//
// Script API
//
// Collections — store_collection(name) returns a collection object with methods:
//
//	c = store_collection("charges")
//	id = c.insert({"amount": 100, "status": "pending"})  # → str
//	doc = c.get(id)                                       # → dict or None
//	docs = c.list()                                       # → list[dict]
//	c.update(id, {"status": "paid"})                       # replaces doc
//	c.delete(id)                                          # removes doc
//
// KV store — standalone builtins:
//
//	store_kv_set("svc", "key", "value")
//	v = store_kv_get("svc", "key")    # → str or None if missing
//	store_kv_delete("svc", "key")
package runtime

import (
	"database/sql"
	"fmt"

	"github.com/stunt-adapters/stunt/internal/primitives"
	"github.com/stunt-adapters/stunt/internal/primitives/kv"
	"github.com/stunt-adapters/stunt/internal/starlark"
	sk "go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// BuildBuiltins returns a Starlark StringDict exposing the given stores as
// builtins ready to pass to starlark.Load. Either store may be nil; the
// corresponding builtins will still be registered but will return an error if
// called without a backing store.
func BuildBuiltins(store *primitives.Store, kvStore *kv.KV) sk.StringDict {
	return sk.StringDict{
		"store_collection": sk.NewBuiltin("store_collection", func(thread *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var name string
			if err := sk.UnpackArgs("store_collection", args, kwargs, "name", &name); err != nil {
				return nil, err
			}
			if store == nil {
				return nil, fmt.Errorf("store_collection: no collection store configured")
			}
			col, err := store.Collection(name)
			if err != nil {
				return nil, err
			}
			return &collectionValue{col: col}, nil
		}),
		"store_kv_get": sk.NewBuiltin("store_kv_get", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var ns, key string
			if err := sk.UnpackArgs("store_kv_get", args, kwargs, "ns", &ns, "key", &key); err != nil {
				return nil, err
			}
			if kvStore == nil {
				return nil, fmt.Errorf("store_kv_get: no kv store configured")
			}
			val, err := kvStore.Get(ns, key)
			if err == sql.ErrNoRows {
				return sk.None, nil
			}
			if err != nil {
				return nil, err
			}
			return sk.String(val), nil
		}),
		"store_kv_set": sk.NewBuiltin("store_kv_set", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var ns, key, val string
			if err := sk.UnpackArgs("store_kv_set", args, kwargs, "ns", &ns, "key", &key, "value", &val); err != nil {
				return nil, err
			}
			if kvStore == nil {
				return nil, fmt.Errorf("store_kv_set: no kv store configured")
			}
			if err := kvStore.Set(ns, key, val); err != nil {
				return nil, err
			}
			return sk.None, nil
		}),
		"store_kv_delete": sk.NewBuiltin("store_kv_delete", func(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
			var ns, key string
			if err := sk.UnpackArgs("store_kv_delete", args, kwargs, "ns", &ns, "key", &key); err != nil {
				return nil, err
			}
			if kvStore == nil {
				return nil, fmt.Errorf("store_kv_delete: no kv store configured")
			}
			if err := kvStore.Delete(ns, key); err != nil {
				return nil, err
			}
			return sk.None, nil
		}),
	}
}

// --- collection object (starlark.Value with methods) ---

// collectionValue wraps a *primitives.Collection as a Starlark value with
// methods: insert, get, list, update, delete.
type collectionValue struct {
	col *primitives.Collection
}

func (c *collectionValue) String() string  { return "collection" }
func (c *collectionValue) Type() string    { return "collection" }
func (c *collectionValue) Freeze()         {}
func (c *collectionValue) Hash() (uint32, error) { return 0, nil }
func (c *collectionValue) Truth() sk.Bool  { return sk.True }

func (c *collectionValue) CompareSameType(_ syntax.Token, _ sk.Value, _ int) (bool, error) {
	return false, fmt.Errorf("collection does not support comparison")
}

// AttrNames returns the method names exposed to Starlark's dir().
func (c *collectionValue) AttrNames() []string {
	return []string{"insert", "get", "list", "update", "delete"}
}

// Attr returns the named method as a Starlark callable, or nil if not found.
func (c *collectionValue) Attr(name string) (sk.Value, error) {
	switch name {
	case "insert":
		return sk.NewBuiltin("collection.insert", c.insert), nil
	case "get":
		return sk.NewBuiltin("collection.get", c.get), nil
	case "list":
		return sk.NewBuiltin("collection.list", c.list), nil
	case "update":
		return sk.NewBuiltin("collection.update", c.update), nil
	case "delete":
		return sk.NewBuiltin("collection.delete", c.delete), nil
	default:
		return nil, nil // no such attribute
	}
}

func (c *collectionValue) insert(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var docVal sk.Value = sk.None
	if err := sk.UnpackArgs("insert", args, kwargs, "doc", &docVal); err != nil {
		return nil, err
	}
	doc, err := dictToGoMap(docVal)
	if err != nil {
		return nil, err
	}
	id, err := c.col.Insert(doc)
	if err != nil {
		return nil, err
	}
	return sk.String(id), nil
}

func (c *collectionValue) get(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var id string
	if err := sk.UnpackArgs("get", args, kwargs, "id", &id); err != nil {
		return nil, err
	}
	doc, err := c.col.Get(id)
	if err == sql.ErrNoRows {
		return sk.None, nil
	}
	if err != nil {
		return nil, err
	}
	return starlark.GoToStarlark(doc), nil
}

func (c *collectionValue) list(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	if err := sk.UnpackArgs("list", args, kwargs); err != nil {
		return nil, err
	}
	docs, err := c.col.List()
	if err != nil {
		return nil, err
	}
	elems := make([]sk.Value, len(docs))
	for i, doc := range docs {
		elems[i] = starlark.GoToStarlark(doc)
	}
	return sk.NewList(elems), nil
}

func (c *collectionValue) update(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var id string
	var docVal sk.Value = sk.None
	if err := sk.UnpackArgs("update", args, kwargs, "id", &id, "doc", &docVal); err != nil {
		return nil, err
	}
	doc, err := dictToGoMap(docVal)
	if err != nil {
		return nil, err
	}
	if err := c.col.Update(id, doc); err != nil {
		return nil, err
	}
	return sk.None, nil
}

func (c *collectionValue) delete(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var id string
	if err := sk.UnpackArgs("delete", args, kwargs, "id", &id); err != nil {
		return nil, err
	}
	if err := c.col.Delete(id); err != nil {
		return nil, err
	}
	return sk.None, nil
}

// dictToGoMap converts a Starlark dict (or None) into a Go map[string]any.
func dictToGoMap(v sk.Value) (map[string]any, error) {
	if v == sk.None {
		return map[string]any{}, nil
	}
	d, ok := v.(*sk.Dict)
	if !ok {
		return nil, fmt.Errorf("expected dict, got %s", v.Type())
	}
	return starlark.StarlarkToGo(d), nil
}
