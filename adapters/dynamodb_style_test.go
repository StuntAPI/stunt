package adapters

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// These tests drive the dynamodb-style adapter script directly (lib.star
// preloaded) over a shared store and a VIRTUAL clock, with requests signed
// by a real SigV4 implementation so the adapter's signature recomputation
// is exercised, not bypassed.

const (
	ddbAccessKey = "AKIAIOSFODNN7EXAMPLE"
	ddbSecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	ddbRegion    = "us-east-1"
	ddbHost      = "dynamodb.us-east-1.amazonaws.com"
	ddbTarget    = "DynamoDB_20120810."
)

type dynamoFixture struct {
	t     *testing.T
	vc    *clock.Clock
	vm    *starlark.VM
	store *primitives.Store
}

func newDynamoFixture(t *testing.T, start time.Time) *dynamoFixture {
	t.Helper()
	dir := repoAdaptersDir(t)
	root := filepath.Join(dir, "dynamodb-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}

	tmp := t.TempDir()
	store, err := primitives.Open(filepath.Join(tmp, "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	kvStore, err := kv.Open(filepath.Join(tmp, "d.kv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kvStore.Close() })
	blobStore, err := blob.Open(filepath.Join(tmp, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobStore.Close() })

	vc := clock.NewVirtualClock(start)
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store:       store,
		KV:          kvStore,
		Blob:        blobStore,
		Clock:       vc,
		ServiceName: "test",
	})

	// Seed the declared collections from the adapter fixtures, exactly like
	// the engine does on boot (buildServiceState): tables.jsonl + items.jsonl.
	for _, res := range []struct{ name, seed string }{
		{"tables", "fixtures/tables.jsonl"},
		{"items", "fixtures/items.jsonl"},
	} {
		col, err := store.Collection(res.name)
		if err != nil {
			t.Fatalf("collection %s: %v", res.name, err)
		}
		if err := col.Seed(filepath.Join(root, res.seed)); err != nil {
			t.Fatalf("seed %s: %v", res.name, err)
		}
	}

	src, err := os.ReadFile(filepath.Join(root, "scripts", "service.star"))
	if err != nil {
		t.Fatalf("read service.star: %v", err)
	}
	vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
	if err != nil {
		t.Fatalf("LoadWithLib service.star: %v", err)
	}

	return &dynamoFixture{t: t, vc: vc, vm: vm, store: store}
}

func ddbSHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func ddbHMAC(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// ddbSignedHeaders signs a POST / DynamoDB call with real SigV4 over the
// header set the AWS SDKs sign (content-type, host, x-amz-date, x-amz-target),
// using the adapter's documented synthetic credentials.
func (f *dynamoFixture) ddbSignedHeaders(target string, body []byte, secret string) map[string]string {
	f.t.Helper()
	at := f.vc.Now().UTC()
	amzDate := at.Format("20060102T150405Z")
	date := amzDate[:8]
	payloadHash := ddbSHA256Hex(body)

	headers := map[string]string{
		"content-type": "application/x-amz-json-1.0",
		"x-amz-date":   amzDate,
		"x-amz-target": target,
	}
	signed := []string{"content-type", "host", "x-amz-date", "x-amz-target"}
	sort.Strings(signed)
	var ch strings.Builder
	for _, h := range signed {
		v := ddbHost
		if h != "host" {
			v = headers[h]
		}
		ch.WriteString(h + ":" + v + "\n")
	}
	creq := "POST\n" + "/" + "\n" + "" + "\n" + ch.String() + "\n" +
		strings.Join(signed, ";") + "\n" + payloadHash
	scope := date + "/" + ddbRegion + "/dynamodb/aws4_request"
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + ddbSHA256Hex([]byte(creq))
	kDate := ddbHMAC([]byte("AWS4"+secret), []byte(date))
	kRegion := ddbHMAC(kDate, []byte(ddbRegion))
	kService := ddbHMAC(kRegion, []byte("dynamodb"))
	kSigning := ddbHMAC(kService, []byte("aws4_request"))
	sig := hex.EncodeToString(ddbHMAC(kSigning, []byte(sts)))
	headers["Authorization"] = "AWS4-HMAC-SHA256 Credential=" + ddbAccessKey + "/" + scope +
		", SignedHeaders=" + strings.Join(signed, ";") + ", Signature=" + sig
	return headers
}

// call invokes an operation with a correctly signed request.
func (f *dynamoFixture) call(target string, payload map[string]any) starlark.Response {
	f.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	resp, err := f.vm.Call("on_service_api", starlark.Request{
		Method:  "POST",
		Path:    "/",
		Host:    ddbHost,
		Headers: f.ddbSignedHeaders(target, raw, ddbSecretKey),
		RawBody: string(raw),
	})
	if err != nil {
		f.t.Fatalf("on_service_api %s: %v", target, err)
	}
	return resp
}

// callWithHeaders invokes an operation with caller-provided headers (for the
// unsigned / tampered / skewed auth paths).
func (f *dynamoFixture) callWithHeaders(target string, payload map[string]any, headers map[string]string) starlark.Response {
	f.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	resp, err := f.vm.Call("on_service_api", starlark.Request{
		Method:  "POST",
		Path:    "/",
		Host:    ddbHost,
		Headers: headers,
		RawBody: string(raw),
	})
	if err != nil {
		f.t.Fatalf("on_service_api %s: %v", target, err)
	}
	return resp
}

// ddbUnsignedHeaders carries the protocol headers but no Authorization.
func ddbUnsignedHeaders(target string) map[string]string {
	return map[string]string{
		"content-type": "application/x-amz-json-1.0",
		"x-amz-target": target,
	}
}

// ddbErrType digs __type out of a DynamoDB error envelope.
func ddbErrType(t *testing.T, resp starlark.Response) string {
	t.Helper()
	et, _ := resp.Body["__type"].(string)
	return et
}

func ddbWantErr(t *testing.T, resp starlark.Response, status int, typeName string) {
	t.Helper()
	if resp.Status != status {
		t.Fatalf("status = %d, want %d (body %v)", resp.Status, status, resp.Body)
	}
	if got := ddbErrType(t, resp); !strings.HasSuffix(got, "#"+typeName) {
		t.Fatalf("__type = %q, want suffix #%s (body %v)", got, typeName, resp.Body)
	}
}

// ddbAttr returns a typed attribute value as a map ({"N": "7"} ...).
func ddbAttr(t *testing.T, item map[string]any, name string) map[string]any {
	t.Helper()
	v, ok := item[name].(map[string]any)
	if !ok {
		t.Fatalf("attribute %q is not a typed value: %v", name, item[name])
	}
	return v
}

// ddbInt coerces a response number (int64 from the Starlark conversion,
// float64 after a JSON round-trip) to an int for comparison.
func ddbInt(v any) int {
	switch x := v.(type) {
	case int64:
		return int(x)
	case int:
		return x
	case float64:
		return int(x)
	}
	return -1
}

// TestDynamoDBSigV4Auth: missing auth → 403; a real SigV4 signature passes;
// tampered signature, wrong secret, and clock skew are all rejected.
func TestDynamoDBSigV4Auth(t *testing.T) {
	f := newDynamoFixture(t, time.Unix(1_750_000_000, 0).UTC())
	list := map[string]any{}

	// Missing Authorization header → 403 in the DynamoDB error shape.
	resp := f.callWithHeaders(ddbTarget+"ListTables", list, ddbUnsignedHeaders(ddbTarget+"ListTables"))
	ddbWantErr(t, resp, 403, "MissingAuthenticationTokenException")

	// Correctly signed request passes and sees the seeded table.
	ok := f.call(ddbTarget+"ListTables", list)
	if ok.Status != 200 {
		t.Fatalf("signed ListTables -> %d: %v", ok.Status, ok.Body)
	}
	names, _ := ok.Body["TableNames"].([]any)
	found := false
	for _, n := range names {
		if n == "demo-table" {
			found = true
		}
	}
	if !found {
		t.Fatalf("TableNames = %v, want the seeded demo-table", names)
	}

	// Tampered signature (flip a hex digit, stays well-formed) → 403.
	raw, _ := json.Marshal(list)
	h := f.ddbSignedHeaders(ddbTarget+"ListTables", raw, ddbSecretKey)
	i := strings.Index(h["Authorization"], "Signature=")
	flip := "0"
	if h["Authorization"][i+len("Signature=")] == '0' {
		flip = "1"
	}
	h["Authorization"] = h["Authorization"][:i+len("Signature=")] + flip + h["Authorization"][i+len("Signature=")+1:]
	resp = f.callWithHeaders(ddbTarget+"ListTables", list, h)
	ddbWantErr(t, resp, 403, "InvalidSignatureException")

	// Wrong secret key → recomputed signature differs → 403.
	resp = f.callWithHeaders(ddbTarget+"ListTables", list, f.ddbSignedHeaders(ddbTarget+"ListTables", raw, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYWRONGSECRET0"))
	ddbWantErr(t, resp, 403, "InvalidSignatureException")

	// Clock skew: a request signed "20 minutes ago" (headers captured before
	// advancing the virtual clock) falls outside the ±15-minute window.
	stale := f.ddbSignedHeaders(ddbTarget+"ListTables", raw, ddbSecretKey)
	f.vc.Advance(20 * time.Minute)
	resp = f.callWithHeaders(ddbTarget+"ListTables", list, stale)
	ddbWantErr(t, resp, 403, "RequestTimeTooSkewedException")
}

// TestDynamoDBTableCrud covers CreateTable/DescribeTable/ListTables/
// DeleteTable including duplicate-create and missing-table errors.
func TestDynamoDBTableCrud(t *testing.T) {
	f := newDynamoFixture(t, time.Unix(1_750_000_000, 0).UTC())
	create := map[string]any{
		"TableName":             "crud-table",
		"AttributeDefinitions":  []any{map[string]any{"AttributeName": "pk", "AttributeType": "S"}},
		"KeySchema":             []any{map[string]any{"AttributeName": "pk", "KeyType": "HASH"}},
		"ProvisionedThroughput": map[string]any{"ReadCapacityUnits": 5, "WriteCapacityUnits": 5},
	}

	resp := f.call(ddbTarget+"CreateTable", create)
	if resp.Status != 200 {
		t.Fatalf("CreateTable -> %d: %v", resp.Status, resp.Body)
	}
	td, _ := resp.Body["TableDescription"].(map[string]any)
	if td["TableStatus"] != "ACTIVE" || ddbInt(td["ItemCount"]) != 0 {
		t.Fatalf("TableDescription = %v", td)
	}
	if !strings.HasSuffix(fmt.Sprint(td["TableArn"]), ":table/crud-table") {
		t.Fatalf("TableArn = %v", td["TableArn"])
	}

	// Duplicate → ResourceInUseException (400), like real DynamoDB.
	ddbWantErr(t, f.call(ddbTarget+"CreateTable", create), 400, "ResourceInUseException")

	// Key attribute not covered by AttributeDefinitions → ValidationException.
	bad := map[string]any{
		"TableName":            "bad-table",
		"AttributeDefinitions": []any{map[string]any{"AttributeName": "other", "AttributeType": "S"}},
		"KeySchema":            []any{map[string]any{"AttributeName": "pk", "KeyType": "HASH"}},
	}
	ddbWantErr(t, f.call(ddbTarget+"CreateTable", bad), 400, "ValidationException")

	// DescribeTable reflects the schema and live item count.
	desc := f.call(ddbTarget+"DescribeTable", map[string]any{"TableName": "crud-table"})
	if desc.Status != 200 {
		t.Fatalf("DescribeTable -> %d: %v", desc.Status, desc.Body)
	}
	tbl, _ := desc.Body["Table"].(map[string]any)
	schema, _ := tbl["KeySchema"].([]any)
	ks0, _ := schema[0].(map[string]any)
	if ks0["KeyType"] != "HASH" || ks0["AttributeName"] != "pk" {
		t.Fatalf("KeySchema = %v", schema)
	}

	// ListTables contains both tables; Limit=1 pages via LastEvaluatedTableName.
	lt := f.call(ddbTarget+"ListTables", map[string]any{})
	if lt.Status != 200 {
		t.Fatalf("ListTables -> %d: %v", lt.Status, lt.Body)
	}
	names, _ := lt.Body["TableNames"].([]any)
	if len(names) != 2 {
		t.Fatalf("TableNames = %v, want both tables", names)
	}
	page1 := f.call(ddbTarget+"ListTables", map[string]any{"Limit": 1})
	if page1.Body["LastEvaluatedTableName"] != "crud-table" {
		t.Fatalf("Limit=1 page = %v, want LastEvaluatedTableName crud-table (sorted first)", page1.Body)
	}
	page2 := f.call(ddbTarget+"ListTables", map[string]any{"Limit": 1, "ExclusiveStartTableName": "crud-table"})
	if page2.Body["LastEvaluatedTableName"] != nil {
		t.Fatalf("second page = %v, want no LastEvaluatedTableName", page2.Body)
	}
	if p2, _ := page2.Body["TableNames"].([]any); len(p2) != 1 || p2[0] != "demo-table" {
		t.Fatalf("second page TableNames = %v", page2.Body["TableNames"])
	}

	// DeleteTable then DescribeTable → ResourceNotFoundException.
	del := f.call(ddbTarget+"DeleteTable", map[string]any{"TableName": "crud-table"})
	if del.Status != 200 {
		t.Fatalf("DeleteTable -> %d: %v", del.Status, del.Body)
	}
	ddbWantErr(t, f.call(ddbTarget+"DescribeTable", map[string]any{"TableName": "crud-table"}), 400, "ResourceNotFoundException")
}

// TestDynamoDBItemRoundTrip: typed values (S/N/BOOL/SS/L/M) round-trip
// verbatim through PutItem/GetItem, with projection and error paths.
func TestDynamoDBItemRoundTrip(t *testing.T) {
	f := newDynamoFixture(t, time.Unix(1_750_000_000, 0).UTC())
	item := map[string]any{
		"pk":     map[string]any{"S": "rt-1"},
		"label":  map[string]any{"S": "round trip"},
		"qty":    map[string]any{"N": "42"},
		"price":  map[string]any{"N": "19.5"},
		"active": map[string]any{"BOOL": true},
		"tags":   map[string]any{"SS": []any{"x", "y"}},
		"flags":  map[string]any{"L": []any{map[string]any{"N": "1"}, map[string]any{"BOOL": false}}},
		"meta":   map[string]any{"M": map[string]any{"src": map[string]any{"S": "test"}}},
		"note":   map[string]any{"NULL": true},
	}

	put := f.call(ddbTarget+"PutItem", map[string]any{"TableName": "demo-table", "Item": item})
	if put.Status != 200 {
		t.Fatalf("PutItem -> %d: %v", put.Status, put.Body)
	}

	get := f.call(ddbTarget+"GetItem", map[string]any{
		"TableName": "demo-table",
		"Key":       map[string]any{"pk": map[string]any{"S": "rt-1"}},
	})
	if get.Status != 200 {
		t.Fatalf("GetItem -> %d: %v", get.Status, get.Body)
	}
	got, _ := get.Body["Item"].(map[string]any)
	if got == nil {
		t.Fatalf("GetItem returned no Item: %v", get.Body)
	}
	for _, want := range []struct {
		attr, typ, val string
	}{
		{"label", "S", "round trip"},
		{"qty", "N", "42"},
		{"price", "N", "19.5"},
	} {
		a := ddbAttr(t, got, want.attr)
		if a[want.typ] != want.val {
			t.Fatalf("%s = %v, want {%s: %s}", want.attr, a, want.typ, want.val)
		}
	}
	if ddbAttr(t, got, "active")["BOOL"] != true {
		t.Fatalf("active = %v", ddbAttr(t, got, "active"))
	}
	meta := ddbAttr(t, got, "meta")["M"].(map[string]any)
	if meta["src"].(map[string]any)["S"] != "test" {
		t.Fatalf("meta.M.src = %v", meta)
	}
	if ddbAttr(t, got, "note")["NULL"] != true {
		t.Fatalf("note = %v", ddbAttr(t, got, "note"))
	}

	// ProjectionExpression keeps only the named top-level attributes.
	proj := f.call(ddbTarget+"GetItem", map[string]any{
		"TableName":            "demo-table",
		"Key":                  map[string]any{"pk": map[string]any{"S": "rt-1"}},
		"ProjectionExpression": "label, qty",
	})
	pItem, _ := proj.Body["Item"].(map[string]any)
	if len(pItem) != 2 || pItem["label"] == nil || pItem["qty"] == nil {
		t.Fatalf("projected Item = %v, want only label+qty", pItem)
	}

	// Miss → empty object; unknown table → ResourceNotFoundException.
	miss := f.call(ddbTarget+"GetItem", map[string]any{
		"TableName": "demo-table",
		"Key":       map[string]any{"pk": map[string]any{"S": "nope"}},
	})
	if miss.Status != 200 || miss.Body["Item"] != nil {
		t.Fatalf("GetItem miss = %d %v, want 200 with no Item", miss.Status, miss.Body)
	}
	ddbWantErr(t, f.call(ddbTarget+"GetItem", map[string]any{
		"TableName": "missing-table",
		"Key":       map[string]any{"pk": map[string]any{"S": "x"}},
	}), 400, "ResourceNotFoundException")

	// Missing key attribute in the item → ValidationException.
	ddbWantErr(t, f.call(ddbTarget+"PutItem", map[string]any{
		"TableName": "demo-table",
		"Item":      map[string]any{"label": map[string]any{"S": "keyless"}},
	}), 400, "ValidationException")

	// Unknown type descriptor → ValidationException.
	ddbWantErr(t, f.call(ddbTarget+"PutItem", map[string]any{
		"TableName": "demo-table",
		"Item": map[string]any{
			"pk":  map[string]any{"S": "bad-type"},
			"bad": map[string]any{"X": "v"},
		},
	}), 400, "ValidationException")

	// Incomplete key on GetItem → ValidationException.
	ddbWantErr(t, f.call(ddbTarget+"GetItem", map[string]any{
		"TableName": "demo-table",
		"Key":       map[string]any{},
	}), 400, "ValidationException")
}

// TestDynamoDBUpdateItem: SET / REMOVE / ADD (numeric, exact decimal) with
// upsert semantics and the ReturnValues variants.
func TestDynamoDBUpdateItem(t *testing.T) {
	f := newDynamoFixture(t, time.Unix(1_750_000_000, 0).UTC())
	key := map[string]any{"pk": map[string]any{"S": "cnt-1"}}

	// SET on a missing item upserts: the item is created with its key.
	set := f.call(ddbTarget+"UpdateItem", map[string]any{
		"TableName":        "demo-table",
		"Key":              key,
		"UpdateExpression": "SET label = :l, n = :n",
		"ExpressionAttributeValues": map[string]any{
			":l": map[string]any{"S": "counter"},
			":n": map[string]any{"N": "10"},
		},
		"ReturnValues": "ALL_NEW",
	})
	if set.Status != 200 {
		t.Fatalf("UpdateItem SET -> %d: %v", set.Status, set.Body)
	}
	attrs, _ := set.Body["Attributes"].(map[string]any)
	if ddbAttr(t, attrs, "n")["N"] != "10" || ddbAttr(t, attrs, "label")["S"] != "counter" {
		t.Fatalf("ALL_NEW Attributes = %v", attrs)
	}

	// ADD performs a numeric add (exact decimal: 10 + 0.5 = 10.5).
	add := f.call(ddbTarget+"UpdateItem", map[string]any{
		"TableName":        "demo-table",
		"Key":              key,
		"UpdateExpression": "ADD n :d",
		"ExpressionAttributeValues": map[string]any{
			":d": map[string]any{"N": "0.5"},
		},
		"ReturnValues": "UPDATED_NEW",
	})
	if add.Status != 200 {
		t.Fatalf("UpdateItem ADD -> %d: %v", add.Status, add.Body)
	}
	ua, _ := add.Body["Attributes"].(map[string]any)
	if len(ua) != 1 || ddbAttr(t, ua, "n")["N"] != "10.5" {
		t.Fatalf("UPDATED_NEW after ADD = %v, want n=10.5 only", ua)
	}

	// REMOVE drops the attribute; UPDATED_OLD reports the removed value.
	rem := f.call(ddbTarget+"UpdateItem", map[string]any{
		"TableName":        "demo-table",
		"Key":              key,
		"UpdateExpression": "REMOVE label SET extra = :e",
		"ExpressionAttributeValues": map[string]any{
			":e": map[string]any{"S": "v"},
		},
		"ReturnValues": "UPDATED_OLD",
	})
	if rem.Status != 200 {
		t.Fatalf("UpdateItem REMOVE -> %d: %v", rem.Status, rem.Body)
	}
	ro, _ := rem.Body["Attributes"].(map[string]any)
	if len(ro) != 1 || ddbAttr(t, ro, "label")["S"] != "counter" {
		t.Fatalf("UPDATED_OLD after REMOVE = %v, want the old label only", ro)
	}
	got := f.call(ddbTarget+"GetItem", map[string]any{"TableName": "demo-table", "Key": key})
	item, _ := got.Body["Item"].(map[string]any)
	if _, still := item["label"]; still {
		t.Fatalf("label still present after REMOVE: %v", item)
	}

	// ADD against a non-numeric attribute → ValidationException.
	ddbWantErr(t, f.call(ddbTarget+"UpdateItem", map[string]any{
		"TableName":        "demo-table",
		"Key":              key,
		"UpdateExpression": "ADD extra :n",
		"ExpressionAttributeValues": map[string]any{
			":n": map[string]any{"N": "1"},
		},
	}), 400, "ValidationException")

	// Invalid ReturnValues → ValidationException.
	ddbWantErr(t, f.call(ddbTarget+"UpdateItem", map[string]any{
		"TableName":        "demo-table",
		"Key":              key,
		"UpdateExpression": "SET n = :n",
		"ExpressionAttributeValues": map[string]any{
			":n": map[string]any{"N": "1"},
		},
		"ReturnValues": "BOGUS",
	}), 400, "ValidationException")
}

// TestDynamoDBQuerySortKey: numeric sort-key ordering, BETWEEN / >= ranges,
// begins_with on a string sort key, ScanIndexForward, and key pagination.
func TestDynamoDBQuerySortKey(t *testing.T) {
	f := newDynamoFixture(t, time.Unix(1_750_000_000, 0).UTC())

	// Table with a numeric sort key: "10" must sort AFTER "9" numerically
	// (a lexicographic sort would place it first).
	resp := f.call(ddbTarget+"CreateTable", map[string]any{
		"TableName": "range-table",
		"AttributeDefinitions": []any{
			map[string]any{"AttributeName": "pk", "AttributeType": "S"},
			map[string]any{"AttributeName": "sk", "AttributeType": "N"},
		},
		"KeySchema": []any{
			map[string]any{"AttributeName": "pk", "KeyType": "HASH"},
			map[string]any{"AttributeName": "sk", "KeyType": "RANGE"},
		},
		"BillingMode": "PAY_PER_REQUEST",
	})
	if resp.Status != 200 {
		t.Fatalf("CreateTable range-table -> %d: %v", resp.Status, resp.Body)
	}
	for _, row := range []string{"u1|1", "u1|2", "u1|9", "u1|10", "u2|5"} {
		parts := strings.SplitN(row, "|", 2)
		p := f.call(ddbTarget+"PutItem", map[string]any{
			"TableName": "range-table",
			"Item": map[string]any{
				"pk": map[string]any{"S": parts[0]},
				"sk": map[string]any{"N": parts[1]},
			},
		})
		if p.Status != 200 {
			t.Fatalf("PutItem %s -> %d: %v", row, p.Status, p.Body)
		}
	}

	q := f.call(ddbTarget+"Query", map[string]any{
		"TableName":                 "range-table",
		"KeyConditionExpression":    "pk = :p",
		"ExpressionAttributeValues": map[string]any{":p": map[string]any{"S": "u1"}},
	})
	if q.Status != 200 {
		t.Fatalf("Query -> %d: %v", q.Status, q.Body)
	}
	var sks []string
	for _, it := range q.Body["Items"].([]any) {
		sks = append(sks, ddbAttr(t, it.(map[string]any), "sk")["N"].(string))
	}
	if strings.Join(sks, ",") != "1,2,9,10" {
		t.Fatalf("sk order = %v, want numeric order 1,2,9,10", sks)
	}

	// BETWEEN range.
	q = f.call(ddbTarget+"Query", map[string]any{
		"TableName":              "range-table",
		"KeyConditionExpression": "pk = :p AND sk BETWEEN :a AND :b",
		"ExpressionAttributeValues": map[string]any{
			":p": map[string]any{"S": "u1"},
			":a": map[string]any{"N": "2"},
			":b": map[string]any{"N": "9"},
		},
	})
	items := q.Body["Items"].([]any)
	if q.Status != 200 || len(items) != 2 {
		t.Fatalf("BETWEEN Query -> %d (%d items): %v", q.Status, len(items), q.Body)
	}

	// >= range.
	q = f.call(ddbTarget+"Query", map[string]any{
		"TableName":              "range-table",
		"KeyConditionExpression": "pk = :p AND sk >= :a",
		"ExpressionAttributeValues": map[string]any{
			":p": map[string]any{"S": "u1"},
			":a": map[string]any{"N": "9"},
		},
	})
	if n := len(q.Body["Items"].([]any)); n != 2 {
		t.Fatalf("sk >= 9 matched %d items, want 2", n)
	}

	// Descending scan direction reverses the sort key.
	q = f.call(ddbTarget+"Query", map[string]any{
		"TableName":                 "range-table",
		"KeyConditionExpression":    "pk = :p",
		"ExpressionAttributeValues": map[string]any{":p": map[string]any{"S": "u1"}},
		"ScanIndexForward":          false,
	})
	var desc []string
	for _, it := range q.Body["Items"].([]any) {
		desc = append(desc, ddbAttr(t, it.(map[string]any), "sk")["N"].(string))
	}
	if strings.Join(desc, ",") != "10,9,2,1" {
		t.Fatalf("descending sk order = %v, want 10,9,2,1", desc)
	}

	// Limit + ExclusiveStartKey pagination: page 1 of 2, then the rest.
	page1 := f.call(ddbTarget+"Query", map[string]any{
		"TableName":                 "range-table",
		"KeyConditionExpression":    "pk = :p",
		"ExpressionAttributeValues": map[string]any{":p": map[string]any{"S": "u1"}},
		"Limit":                     2,
	})
	lek, _ := page1.Body["LastEvaluatedKey"].(map[string]any)
	if lek == nil || ddbAttr(t, lek, "sk")["N"] != "2" {
		t.Fatalf("page1 = %v, want LastEvaluatedKey sk=2", page1.Body)
	}
	page2 := f.call(ddbTarget+"Query", map[string]any{
		"TableName":                 "range-table",
		"KeyConditionExpression":    "pk = :p",
		"ExpressionAttributeValues": map[string]any{":p": map[string]any{"S": "u1"}},
		"Limit":                     2,
		"ExclusiveStartKey":         lek,
	})
	p2 := page2.Body["Items"].([]any)
	if len(p2) != 2 || ddbAttr(t, p2[0].(map[string]any), "sk")["N"] != "9" {
		t.Fatalf("page2 = %v", page2.Body)
	}
	if page2.Body["LastEvaluatedKey"] != nil {
		t.Fatalf("final page still has LastEvaluatedKey: %v", page2.Body)
	}

	// Wrong partition attribute in the key condition → ValidationException.
	ddbWantErr(t, f.call(ddbTarget+"Query", map[string]any{
		"TableName":                 "range-table",
		"KeyConditionExpression":    "sk = :p",
		"ExpressionAttributeValues": map[string]any{":p": map[string]any{"N": "1"}},
	}), 400, "ValidationException")

	// begins_with needs a string sort key: second table.
	if r := f.call(ddbTarget+"CreateTable", map[string]any{
		"TableName": "str-table",
		"AttributeDefinitions": []any{
			map[string]any{"AttributeName": "pk", "AttributeType": "S"},
			map[string]any{"AttributeName": "sk", "AttributeType": "S"},
		},
		"KeySchema": []any{
			map[string]any{"AttributeName": "pk", "KeyType": "HASH"},
			map[string]any{"AttributeName": "sk", "KeyType": "RANGE"},
		},
		"BillingMode": "PAY_PER_REQUEST",
	}); r.Status != 200 {
		t.Fatalf("CreateTable str-table -> %d: %v", r.Status, r.Body)
	}
	for _, sk := range []string{"a-1", "a-2", "b-1"} {
		if p := f.call(ddbTarget+"PutItem", map[string]any{
			"TableName": "str-table",
			"Item": map[string]any{
				"pk": map[string]any{"S": "u1"},
				"sk": map[string]any{"S": sk},
			},
		}); p.Status != 200 {
			t.Fatalf("PutItem %s -> %d: %v", sk, p.Status, p.Body)
		}
	}
	q = f.call(ddbTarget+"Query", map[string]any{
		"TableName":              "str-table",
		"KeyConditionExpression": "pk = :p AND begins_with(sk, :pre)",
		"ExpressionAttributeValues": map[string]any{
			":p":   map[string]any{"S": "u1"},
			":pre": map[string]any{"S": "a"},
		},
	})
	bw := q.Body["Items"].([]any)
	if q.Status != 200 || len(bw) != 2 {
		t.Fatalf("begins_with Query -> %d (%d items): %v", q.Status, len(bw), q.Body)
	}
}

// TestDynamoDBScanFilter: Scan returns the whole seeded table, honors the
// FilterExpression subset, and supports Select: COUNT.
func TestDynamoDBScanFilter(t *testing.T) {
	f := newDynamoFixture(t, time.Unix(1_750_000_000, 0).UTC())

	all := f.call(ddbTarget+"Scan", map[string]any{"TableName": "demo-table"})
	if all.Status != 200 || ddbInt(all.Body["Count"]) != 4 {
		t.Fatalf("Scan -> %d %v, want the 4 seeded items", all.Status, all.Body)
	}

	// Numeric + BOOL filter over the seeded qty values (12, 34, 7, 0) and
	// active flags (true, false, true, true): only demo-1 qualifies.
	filtered := f.call(ddbTarget+"Scan", map[string]any{
		"TableName":        "demo-table",
		"FilterExpression": "qty > :v AND active = :t",
		"ExpressionAttributeValues": map[string]any{
			":v": map[string]any{"N": "10"},
			":t": map[string]any{"BOOL": true},
		},
	})
	if filtered.Status != 200 {
		t.Fatalf("Scan filter -> %d: %v", filtered.Status, filtered.Body)
	}
	items := filtered.Body["Items"].([]any)
	if len(items) != 1 || ddbInt(filtered.Body["Count"]) != 1 || ddbInt(filtered.Body["ScannedCount"]) != 4 {
		t.Fatalf("filtered Scan = %v", filtered.Body)
	}
	if ddbAttr(t, items[0].(map[string]any), "pk")["S"] != "demo-1" {
		t.Fatalf("filtered item = %v, want demo-1", items[0])
	}

	// attribute_not_exists over an attribute only one seeded item carries.
	notExists := f.call(ddbTarget+"Scan", map[string]any{
		"TableName":        "demo-table",
		"FilterExpression": "attribute_exists(#m)",
		"ExpressionAttributeNames": map[string]any{
			"#m": "meta",
		},
	})
	if ne := notExists.Body["Items"].([]any); len(ne) != 1 {
		t.Fatalf("attribute_exists(meta) matched %d items, want the 1 seeded item", len(ne))
	}

	// Select COUNT returns counts only.
	countOnly := f.call(ddbTarget+"Scan", map[string]any{
		"TableName":        "demo-table",
		"FilterExpression": "qty > :v",
		"ExpressionAttributeValues": map[string]any{
			":v": map[string]any{"N": "10"},
		},
		"Select": "COUNT",
	})
	if countOnly.Body["Items"] != nil || ddbInt(countOnly.Body["Count"]) != 2 {
		t.Fatalf("Select COUNT = %v", countOnly.Body)
	}

	// Scan pagination over the seeded items.
	page1 := f.call(ddbTarget+"Scan", map[string]any{"TableName": "demo-table", "Limit": 3})
	lek, _ := page1.Body["LastEvaluatedKey"].(map[string]any)
	if lek == nil {
		t.Fatalf("Limit=3 Scan returned no LastEvaluatedKey: %v", page1.Body)
	}
	page2 := f.call(ddbTarget+"Scan", map[string]any{"TableName": "demo-table", "ExclusiveStartKey": lek})
	if p2 := page2.Body["Items"].([]any); len(p2) != 1 {
		t.Fatalf("second Scan page = %v, want the 1 remaining item", page2.Body)
	}

	ddbWantErr(t, f.call(ddbTarget+"Scan", map[string]any{"TableName": "missing-table"}), 400, "ResourceNotFoundException")
}

// TestDynamoDBConditionalWrites: attribute_not_exists guards, the
// ConditionalCheckFailedException envelope (with the item on
// ReturnValuesOnConditionCheckFailure=ALL_OLD), and consumed-capacity echo.
func TestDynamoDBConditionalWrites(t *testing.T) {
	f := newDynamoFixture(t, time.Unix(1_750_000_000, 0).UTC())
	key := map[string]any{"pk": map[string]any{"S": "cond-1"}}

	// Insert-if-absent succeeds on a fresh key.
	first := f.call(ddbTarget+"PutItem", map[string]any{
		"TableName":           "demo-table",
		"Item":                map[string]any{"pk": key["pk"], "label": map[string]any{"S": "original"}},
		"ConditionExpression": "attribute_not_exists(pk)",
	})
	if first.Status != 200 {
		t.Fatalf("conditional PutItem -> %d: %v", first.Status, first.Body)
	}

	// Same condition on the existing key fails, returning the old item.
	second := f.call(ddbTarget+"PutItem", map[string]any{
		"TableName":                           "demo-table",
		"Item":                                map[string]any{"pk": key["pk"], "label": map[string]any{"S": "clobbered"}},
		"ConditionExpression":                 "attribute_not_exists(pk)",
		"ReturnValuesOnConditionCheckFailure": "ALL_OLD",
	})
	ddbWantErr(t, second, 400, "ConditionalCheckFailedException")
	old, _ := second.Body["Item"].(map[string]any)
	if old == nil || ddbAttr(t, old, "label")["S"] != "original" {
		t.Fatalf("ConditionalCheckFailed Item = %v, want the old item", second.Body["Item"])
	}
	// The failed write must not have clobbered the item.
	got := f.call(ddbTarget+"GetItem", map[string]any{"TableName": "demo-table", "Key": key})
	if item, _ := got.Body["Item"].(map[string]any); ddbAttr(t, item, "label")["S"] != "original" {
		t.Fatalf("failed conditional write mutated the item: %v", item)
	}

	// DeleteItem with a condition that does not hold.
	del := f.call(ddbTarget+"DeleteItem", map[string]any{
		"TableName":           "demo-table",
		"Key":                 key,
		"ConditionExpression": "label = :nope",
		"ExpressionAttributeValues": map[string]any{
			":nope": map[string]any{"S": "other"},
		},
	})
	ddbWantErr(t, del, 400, "ConditionalCheckFailedException")

	// Condition that holds allows the delete; ALL_OLD returns the item.
	delOk := f.call(ddbTarget+"DeleteItem", map[string]any{
		"TableName":           "demo-table",
		"Key":                 key,
		"ConditionExpression": "label = :want",
		"ExpressionAttributeValues": map[string]any{
			":want": map[string]any{"S": "original"},
		},
		"ReturnValues": "ALL_OLD",
	})
	if delOk.Status != 200 {
		t.Fatalf("conditional DeleteItem -> %d: %v", delOk.Status, delOk.Body)
	}
	if rv, _ := delOk.Body["Attributes"].(map[string]any); ddbAttr(t, rv, "label")["S"] != "original" {
		t.Fatalf("ALL_OLD Attributes = %v", delOk.Body)
	}

	// ReturnConsumedCapacity is echoed when requested.
	cap := f.call(ddbTarget+"GetItem", map[string]any{
		"TableName":              "demo-table",
		"Key":                    map[string]any{"pk": map[string]any{"S": "demo-1"}},
		"ReturnConsumedCapacity": "TOTAL",
	})
	cc, _ := cap.Body["ConsumedCapacity"].(map[string]any)
	if cc == nil || cc["TableName"] != "demo-table" || cc["CapacityUnits"] == nil {
		t.Fatalf("ConsumedCapacity = %v", cap.Body)
	}
}

// TestDynamoDBBatchOperations: BatchGetItem/BatchWriteItem round-trips, the
// 25-key per-table cap, and the always-empty Unprocessed* maps.
func TestDynamoDBBatchOperations(t *testing.T) {
	f := newDynamoFixture(t, time.Unix(1_750_000_000, 0).UTC())

	if r := f.call(ddbTarget+"CreateTable", map[string]any{
		"TableName":            "batch-table",
		"AttributeDefinitions": []any{map[string]any{"AttributeName": "pk", "AttributeType": "S"}},
		"KeySchema":            []any{map[string]any{"AttributeName": "pk", "KeyType": "HASH"}},
		"BillingMode":          "PAY_PER_REQUEST",
	}); r.Status != 200 {
		t.Fatalf("CreateTable batch-table -> %d: %v", r.Status, r.Body)
	}

	// BatchWriteItem: a PutRequest/DeleteRequest mix.
	var writes []any
	for _, id := range []string{"b-1", "b-2", "b-3"} {
		writes = append(writes, map[string]any{
			"PutRequest": map[string]any{
				"Item": map[string]any{
					"pk":    map[string]any{"S": id},
					"label": map[string]any{"S": "batch " + id},
				},
			},
		})
	}
	writes = append(writes, map[string]any{
		"DeleteRequest": map[string]any{
			"Key": map[string]any{"pk": map[string]any{"S": "b-3"}},
		},
	})
	wr := f.call(ddbTarget+"BatchWriteItem", map[string]any{
		"RequestItems": map[string]any{"batch-table": writes},
	})
	if wr.Status != 200 {
		t.Fatalf("BatchWriteItem -> %d: %v", wr.Status, wr.Body)
	}
	if un, _ := wr.Body["UnprocessedItems"].(map[string]any); len(un) != 0 {
		t.Fatalf("UnprocessedItems = %v, want empty", un)
	}

	// BatchGetItem across the new table and the seeded demo-table.
	gr := f.call(ddbTarget+"BatchGetItem", map[string]any{
		"RequestItems": map[string]any{
			"batch-table": map[string]any{
				"Keys": []any{
					map[string]any{"pk": map[string]any{"S": "b-1"}},
					map[string]any{"pk": map[string]any{"S": "b-2"}},
					map[string]any{"pk": map[string]any{"S": "b-3"}}, // deleted above
				},
			},
			"demo-table": map[string]any{
				"Keys": []any{map[string]any{"pk": map[string]any{"S": "demo-2"}}},
			},
		},
	})
	if gr.Status != 200 {
		t.Fatalf("BatchGetItem -> %d: %v", gr.Status, gr.Body)
	}
	responses, _ := gr.Body["Responses"].(map[string]any)
	batchItems, _ := responses["batch-table"].([]any)
	if len(batchItems) != 2 {
		t.Fatalf("batch-table responses = %v, want the 2 surviving items", responses["batch-table"])
	}
	demoItems, _ := responses["demo-table"].([]any)
	if len(demoItems) != 1 || ddbAttr(t, demoItems[0].(map[string]any), "label")["S"] != "Beta demo widget" {
		t.Fatalf("demo-table responses = %v", responses["demo-table"])
	}
	if un, _ := gr.Body["UnprocessedKeys"].(map[string]any); len(un) != 0 {
		t.Fatalf("UnprocessedKeys = %v, want empty", un)
	}

	// 26 keys in one table exceeds the cap → ValidationException.
	var tooMany []any
	for i := 0; i < 26; i++ {
		tooMany = append(tooMany, map[string]any{"pk": map[string]any{"S": "k"}})
	}
	ddbWantErr(t, f.call(ddbTarget+"BatchGetItem", map[string]any{
		"RequestItems": map[string]any{"batch-table": map[string]any{"Keys": tooMany}},
	}), 400, "ValidationException")

	var tooManyWrites []any
	for i := 0; i < 26; i++ {
		tooManyWrites = append(tooManyWrites, map[string]any{
			"PutRequest": map[string]any{"Item": map[string]any{"pk": map[string]any{"S": "k"}}},
		})
	}
	ddbWantErr(t, f.call(ddbTarget+"BatchWriteItem", map[string]any{
		"RequestItems": map[string]any{"batch-table": tooManyWrites},
	}), 400, "ValidationException")

	// Unknown table in a batch → ResourceNotFoundException.
	ddbWantErr(t, f.call(ddbTarget+"BatchGetItem", map[string]any{
		"RequestItems": map[string]any{
			"missing-table": map[string]any{"Keys": []any{map[string]any{"pk": map[string]any{"S": "x"}}}},
		},
	}), 400, "ResourceNotFoundException")
}

// TestDynamoDBExpressionSubset: the unsupported constructs fail with clear
// ValidationExceptions instead of being silently mis-evaluated.
func TestDynamoDBExpressionSubset(t *testing.T) {
	f := newDynamoFixture(t, time.Unix(1_750_000_000, 0).UTC())
	key := map[string]any{"pk": map[string]any{"S": "demo-1"}}

	// OR is unsupported.
	orErr := f.call(ddbTarget+"PutItem", map[string]any{
		"TableName":           "demo-table",
		"Item":                map[string]any{"pk": key["pk"], "label": map[string]any{"S": "v"}},
		"ConditionExpression": "attribute_exists(label) OR attribute_exists(qty)",
	})
	ddbWantErr(t, orErr, 400, "ValidationException")
	if m, _ := orErr.Body["message"].(string); !strings.Contains(m, "OR") {
		t.Fatalf("message = %q, want it to name OR", m)
	}

	// Nested document paths are unsupported (filter + projection).
	pathErr := f.call(ddbTarget+"Scan", map[string]any{
		"TableName":        "demo-table",
		"FilterExpression": "meta.src = :v",
		"ExpressionAttributeValues": map[string]any{
			":v": map[string]any{"S": "seed"},
		},
	})
	ddbWantErr(t, pathErr, 400, "ValidationException")

	projErr := f.call(ddbTarget+"GetItem", map[string]any{
		"TableName":            "demo-table",
		"Key":                  key,
		"ProjectionExpression": "meta.src",
	})
	ddbWantErr(t, projErr, 400, "ValidationException")

	// DELETE (the set-remove UpdateExpression clause) is unsupported.
	delErr := f.call(ddbTarget+"UpdateItem", map[string]any{
		"TableName":        "demo-table",
		"Key":              key,
		"UpdateExpression": "DELETE tags :t",
		"ExpressionAttributeValues": map[string]any{
			":t": map[string]any{"SS": []any{"alpha"}},
		},
	})
	ddbWantErr(t, delErr, 400, "ValidationException")

	// Undefined expression attribute value.
	undefErr := f.call(ddbTarget+"Query", map[string]any{
		"TableName":              "demo-table",
		"KeyConditionExpression": "pk = :missing",
	})
	ddbWantErr(t, undefErr, 400, "ValidationException")

	// Legacy parameter family is rejected with a pointer to the
	// ExpressionAttribute* forms.
	legacyErr := f.call(ddbTarget+"GetItem", map[string]any{
		"TableName":       "demo-table",
		"Key":             key,
		"AttributesToGet": []any{"label"},
	})
	ddbWantErr(t, legacyErr, 400, "ValidationException")

	// Unknown operation → 400 UnknownOperationException envelope.
	unk := f.call(ddbTarget+"TimeTravel", map[string]any{})
	ddbWantErr(t, unk, 400, "UnknownOperationException")
}
