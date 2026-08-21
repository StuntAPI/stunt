# Amazon DynamoDB-style adapter

A stunt adapter for simulating the **Amazon DynamoDB API (API version
2012-08-10)** locally — a key-value/document store speaking the DynamoDB JSON
protocol (`application/x-amz-json-1.0`). All data is synthetic — no real API
data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Amazon Web Services. "Amazon Web Services", "Amazon
> DynamoDB", and related marks are trademarks of their respective owners. See
> [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for **local
> development and testing only**.

## What it simulates

A behavioral mock of DynamoDB's single-endpoint protocol: every operation is a
`POST /` carrying the operation in the `X-Amz-Target` header
(`DynamoDB_20120810.<Operation>`) and a DynamoDB-JSON body. Responses use the
DynamoDB error envelope `{"__type": "com.amazonaws.dynamodb#...", "message":
"..."}`.

- **Items** are stored as DynamoDB **typed attribute values** (`S`, `N`, `B`,
  `BOOL`, `L`, `M`, `SS`, `NS`, `BS`, `NULL`) verbatim — what you PutItem is
  byte-for-byte what GetItem/Query/Scan return.
- **Key schema** (HASH, optional RANGE) comes from `CreateTable`; item
  operations validate the full key against it.
- **Expressions** support a documented subset (below) with a small
  recursive-descent parser: `KeyConditionExpression`, `FilterExpression`,
  `ConditionExpression`, `UpdateExpression`, `ProjectionExpression`, plus
  `ExpressionAttributeNames` (`#name`) / `ExpressionAttributeValues` (`:val`)
  substitution everywhere.
- **Stateful**: tables and items live in SQLite-backed collections and survive
  `stunt up` restarts; `stunt clean` resets to the seed fixtures.

### Seeded data

One table is seeded once per instance:

| Table         | Key schema         | Seeded items |
|---------------|--------------------|--------------|
| `demo-table`  | `pk` (S), no range | 4 synthetic items (`demo-1` .. `demo-4`) with S/N/BOOL/SS/L/M/NULL attribute examples |

The item ids (`demo-1`, `demo-2`, ...) and values are synthetic; every number
in the fixtures is 4 digits or fewer.

## Operations

| Operation | Notes |
|---|---|
| `CreateTable` | KeySchema + AttributeDefinitions validated (definitions must exactly cover the key attributes); `ResourceInUseException` on duplicates; `PAY_PER_REQUEST` or `ProvisionedThroughput` echoed |
| `DescribeTable` | `ItemCount` / `TableSizeBytes` computed live; `CreationDateTime` from the clock |
| `ListTables` | `Limit` + `ExclusiveStartTableName` → `LastEvaluatedTableName` (via the `paginate` builtin) |
| `DeleteTable` | Drops the table and every item in it |
| `PutItem` | Full typed-value validation; key attributes required in the item; `ConditionExpression`; `ReturnValues: ALL_OLD` |
| `GetItem` | Miss → `{}`; `ProjectionExpression`; `ConsistentRead` accepted (no-op — the store is always consistent) |
| `DeleteItem` | `ConditionExpression`; `ReturnValues: ALL_OLD`; idempotent on missing items |
| `UpdateItem` | `SET` / `REMOVE` / `ADD` (below); upserts like real DynamoDB; `ReturnValues` incl. `UPDATED_OLD`/`UPDATED_NEW` |
| `Query` | Partition-key equality + optional sort-key range; `ScanIndexForward`; `FilterExpression`; `Limit` + `ExclusiveStartKey` → `LastEvaluatedKey`; `Select: COUNT` |
| `Scan` | Full table; `FilterExpression`; same pagination; `Select: COUNT` |
| `BatchGetItem` | Up to 25 keys per table (validated); duplicates deduped; `UnprocessedKeys` always `{}` |
| `BatchWriteItem` | `PutRequest`/`DeleteRequest` mix, up to 25 per table; `UnprocessedItems` always `{}` |

`ReturnConsumedCapacity: TOTAL`/`INDEXES` echoes
`ConsumedCapacity: {TableName, CapacityUnits: 1}` (see divergences).

## Expression subset (the contract)

Supported, everywhere the parameter exists:

- **KeyConditionExpression**: `pk = :v`, optionally `AND sk <op> :v` where
  `<op>` is `=`, `<`, `<=`, `>`, `>=`, `BETWEEN :a AND :b`, or
  `begins_with(sk, :p)`.
- **FilterExpression / ConditionExpression**: comparisons (`=`, `<>`, `<`,
  `<=`, `>`, `>=`), `begins_with(a, :v)`, `a BETWEEN :lo AND :hi`,
  `attribute_exists(a)`, `attribute_not_exists(a)`, and `AND` between terms.
- **UpdateExpression**: `SET a = :v [, b = :v2 ...]`, `REMOVE a [, b ...]`,
  `ADD counter :n` (numeric add with exact decimal arithmetic; number/string
  set union). Sections may be chained: `SET a = :v REMOVE b ADD n :one`.
- **ProjectionExpression**: top-level attribute names, comma-separated.
- **`#name` / `:val`** substitution in all of the above (undefined
  placeholders → `ValidationException`, like real DynamoDB).

Explicitly **unsupported** (→ `ValidationException` with a clear message):

`OR`, `NOT`, parenthesized groups, `IN`, `size()`, document paths (`a.b.c` —
top-level attributes only), `DELETE` (the UpdateExpression set-remove clause),
`if_not_exists()` / `list_append()`, comparison functions
(`attribute_type`, `contains`), secondary indexes (GSIs/LSIs), and the legacy
pre-expression parameters (`Expected`, `AttributeUpdates`, `KeyConditions`,
`ScanFilter`, ...).

Comparison semantics follow real DynamoDB: numbers compare numerically (exact
decimal), strings/bytes lexicographically, mismatched types match nothing
except `<>`, and a missing attribute satisfies only `<>` /
`attribute_not_exists`.

A failed `ConditionExpression` returns **400**
`ConditionalCheckFailedException`, including the item as `Item` when
`ReturnValuesOnConditionCheckFailure=ALL_OLD`.

## Auth — AWS Signature Version 4 (SigV4), verified for real

The adapter **recomputes the signature**: the canonical request is rebuilt
from the incoming request (method, RFC 3986-encoded path, sorted/encoded
query, signed headers — `host` resolves from the transport Host — hashed
payload), the string-to-sign is formed with the `X-Amz-Date` header and the
Credential scope, and the signing key
`HMAC(HMAC(HMAC(HMAC("AWS4"+secret, date), region), "dynamodb"), "aws4_request")`
is derived from the documented synthetic secret. A real SDK pointed at this
adapter with the credentials below produces signatures that verify.

### Synthetic credentials (documented constants)

```
Access key ID:     AKIAIOSFODNN7EXAMPLE
Secret access key: wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
Region:            us-east-1 (any scope region is accepted; the service must be dynamodb)
```

These are the long-public example credentials from the AWS documentation —
no real account backs them.

### Verification scheme

1. **Structure**: `Credential` (`<AK>/<YYYYMMDD>/<region>/dynamodb/aws4_request`),
   `SignedHeaders`, and a hex `Signature` must be present, else 403.
2. **Access key**: must be the documented AKID, else 403 `UnrecognizedClientException`.
3. **Clock window**: `x-amz-date` is required and must be within ±15 minutes
   of the adapter clock (the engine's injectable clock), else 403.
4. **Signature**: the recomputed hex signature must match (the payload hash is
   `sha256` of the verbatim body bytes unless an `x-amz-content-sha256` header
   is present), else 403 `InvalidSignatureException`.

### Auth errors (403, DynamoDB error shape)

| `__type` | When |
|---|---|
| `MissingAuthenticationTokenException` | No `Authorization` header |
| `InvalidSignatureException` | Malformed header/scope, missing `x-amz-date`, bad hex signature, or signature mismatch |
| `UnrecognizedClientException` | Access key is not the documented AKID |
| `RequestTimeTooSkewedException` | `x-amz-date` outside the ±15-minute window |

### Pointing the AWS CLI at it

```bash
export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE
export AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
export AWS_REGION=us-east-1
# no valid credentials file needed; the static env pair signs fine

aws --endpoint-url http://localhost:PORT dynamodb list-tables

aws --endpoint-url http://localhost:PORT dynamodb create-table \
  --table-name demo2-table \
  --attribute-definitions AttributeName=pk,AttributeType=S \
  --key-schema AttributeName=pk,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST

aws --endpoint-url http://localhost:PORT dynamodb put-item \
  --table-name demo2-table --item '{"pk": {"S": "row-1"}, "n": {"N": "7"}}'

aws --endpoint-url http://localhost:PORT dynamodb get-item \
  --table-name demo2-table --key '{"pk": {"S": "row-1"}}'

aws --endpoint-url http://localhost:PORT dynamodb query \
  --table-name demo-table \
  --key-condition-expression "pk = :v" \
  --expression-attribute-values '{":v": {"S": "demo-1"}}'

# Missing auth → 403 {"__type":"com.amazonaws.dynamodb#MissingAuthenticationTokenException", ...}
```

### Pointing aws-sdk-go-v2 at it

```go
cfg, err := config.LoadDefaultConfig(ctx,
    config.WithRegion("us-east-1"),
    config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
        "AKIAIOSFODNN7EXAMPLE",
        "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
        "")),
)
if err != nil {
    log.Fatal(err)
}
client := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
    o.BaseEndpoint = aws.String("http://localhost:PORT") // your stunt service
})
out, err := client.ListTables(ctx, &dynamodb.ListTablesInput{})
```

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/` | `service.star#on_service_api` | All 12 operations, dispatched on `X-Amz-Target` |

Any unmatched route returns a 404 in the DynamoDB error shape (see the
catch-all rule in `adapter.yaml`).

## Backing stores

| Collection | Purpose |
|------------|---------|
| `tables` | Table schema + metadata (`{name, hashAttr/hashType, rangeAttr/rangeType, createdUnix, billingMode, provisioned}`), seeded from `fixtures/tables.jsonl` |
| `items` | One doc per item: `{id: "<table>|<hash>[|<range>]", table, k: key attributes inline, attrs: the full typed item}`, seeded from `fixtures/items.jsonl` |

## Concurrency

`concurrency_key` can only serialize on a **path** parameter, and DynamoDB's
single `POST /` route carries the table name in the JSON body — so per-table
serialization via `concurrency_key` is not possible here. Mutating item
operations instead rely on the SQLite-level atomicity of the collection
`insert`/`update` (each a single statement, and the store serializes on one
connection). The read-modify-write in `UpdateItem` (read item, apply update,
write back) is therefore not fully serialized under extreme concurrency — a
documented divergence; real DynamoDB serializes item writes per key.

## Divergences from real DynamoDB

1. **Capacity units are fake**: `ReturnConsumedCapacity` echoes
   `CapacityUnits: 1` per table; there is no RCU/WCU accounting.
2. **`UnprocessedKeys` / `UnprocessedItems` are always empty** — the local
   store never throttles.
3. **`Limit` applies to returned items** (post-filter). Real DynamoDB caps
   items *evaluated*, so a heavily filtered page can return fewer items than
   `Limit` while a `LastEvaluatedKey` remains.
4. **`Scan` order** is the internal key encoding order (deterministic); real
   Scan order is undefined.
5. **`TableSizeBytes`** is a computed estimate (sum of attribute-name lengths
   plus scalar value lengths), not the real 100-byte-per-item accounting.
6. **Seeded table's `CreationDateTime`** derives from the clock at read time
   (the seed fixture stores no epoch so it stays lint-clean); created tables
   store their real creation time.
7. **Tables become `ACTIVE` immediately** and `DeleteTable` is synchronous —
   no `CREATING`/`DELETING` lifecycle states.
8. **Expression subset** (see above): `OR`, `NOT`, parens, `IN`, `size()`,
   document paths, and set-`DELETE` are rejected rather than implemented.
9. **Part ETag-style request ids, retries, and 100-item cross-table batch
   caps** are not modeled; the per-table 25-key cap is enforced.
10. **UpdateItem always re-adds the key attributes** (REMOVE on a key
    attribute silently no-ops instead of erroring).
11. **Non-canonical number strings** (".5", "5.", "01") are accepted where
    real DynamoDB rejects them; numbers are compared/stored after
    normalization, so they behave as their canonical value.

## API version

```
api:
  name: "Amazon DynamoDB API"
  version: "2012-08-10"
```
