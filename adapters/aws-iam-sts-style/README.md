# aws-iam-sts-style

AWS IAM + STS (Security Token Service) API simulator for local testing.

> **Not affiliated with AWS.** Synthetic data only. See [DISCLAIMER](DISCLAIMER).

## Why

Assume-role chains are the #1 local-dev pain for AWS credential workflows.
Real code needs to call `AssumeRole` to get temporary credentials, then use
those creds for downstream S3/DynamoDB calls. This mock lets you exercise
that entire flow locally without real AWS credentials.

## API version

- **API**: AWS STS + IAM API
- **Version**: `2011-06-15`

## Auth — AWS Signature Version 4 (SigV4), verified for real

The adapter **recomputes the signature**: the canonical request is rebuilt
from the incoming request (method, RFC 3986-encoded path, sorted/encoded
query — where the query-API `Action`/`Version`/... parameters live —, signed
headers, sha256 of the verbatim body), the string-to-sign is formed with the
`X-Amz-Date` header and the Credential scope, and the signing key
`HMAC(HMAC(HMAC(HMAC("AWS4"+secret, date), region), service), "aws4_request")`
is derived from the documented synthetic secret. A real SDK configured with
the credentials below produces signatures that verify against this adapter.

### Synthetic credentials (documented constants)

```
Access key ID:     AKIAIOSFODNN7EXAMPLE
Secret access key: wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
```

These are the long-public example credentials from the AWS documentation —
no real account backs them. The service in the Credential scope may be `sts`
or `iam`.

### Verification scheme

1. **Structure**: `Credential`
   (`<AK>/<YYYYMMDD>/<region>/{sts,iam}/aws4_request`), `SignedHeaders`, and
   a hex `Signature` must be present (missing pieces → `403
   IncompleteSignature`, the real STS error).
2. **Access key**: must be the documented AKID, else `403
   InvalidClientTokenId` ("The security token included in the request is
   invalid." — the real STS error for an unknown key).
3. **Clock window**: `x-amz-date` is required and must be within ±15 minutes
   of the adapter clock (the engine's injectable clock), else `403
   RequestTimeTooSkewed`.
4. **Signature**: the recomputed hex signature must match, else `403
   SignatureDoesNotMatch`. The payload hash is sha256 of the verbatim raw
   body (empty for GETs; the form-encoded body for POSTs), so a request
   tampered after signing is rejected.

### Known limitations

- The adapter sees the **decoded** path and query, so the canonical URI and
  query string are rebuilt by re-encoding the decoded values. Duplicate
  query keys and non-canonical encodings in the original wire request cannot
  be distinguished.
- The RFC 1123 `Date` header fallback is not parsed; `x-amz-date` is required.

### Clock-derived response data

- `<Expiration>` on temporary credentials is `now + DurationSeconds` from the
  engine clock (real STS semantics; default 3600s).
- `<CreateDate>` on CreateRole/CreateAccessKey (and on the seeded role/user)
  derives from the clock as an RFC 3339 timestamp.

## Endpoints

AWS IAM/STS is a **query-API**: the operation is selected by the `Action`
query parameter. Both GET and POST carry the same parameters.

### STS actions

| Action | Description |
|--------|-------------|
| `?Action=AssumeRole` | Mint temporary credentials (ASIA...) for a role. **Stateful.** |
| `?Action=AssumeRoleWithWebIdentity` | OIDC federation — temp creds via web identity token. |
| `?Action=GetSessionToken` | Mint temp session credentials. |
| `?Action=GetCallerIdentity` | Returns Arn/UserId/Account — "who am I". |
| `?Action=DecodeAuthorizationMessage` | Decodes an encoded authorization message. |

### IAM actions

| Action | Description |
|--------|-------------|
| `?Action=ListRoles` | List IAM roles. **Stateful** (seeded + created roles appear). Honors `PathPrefix`, `MaxItems`, `Marker`. |
| `?Action=GetRole` | Get a single role by name. |
| `?Action=CreateRole` | Create a new IAM role. |
| `?Action=ListUsers` | List IAM users. Honors `PathPrefix`, `MaxItems`, `Marker`. |
| `?Action=CreateAccessKey` | Create an AKIA... long-term access key. |

List actions paginate with `MaxItems` (IAM default `100`) + `Marker` (the
opaque token from a prior truncated response); when truncated, the response
carries `<IsTruncated>true</IsTruncated>` plus a `<Marker>` element, and
`PathPrefix` filters entities by path prefix — all applied before rendering,
like the real query API.

## Credential provider chain

Local code resolves `AssumeRole` → temp creds → used for subsequent calls.
After calling `AssumeRole`, `GetCallerIdentity` reflects the assumed role,
modeling the credential provider chain.

Temp credentials use the `ASIA...` prefix; long-term keys use `AKIA...`,
matching real AWS key ID conventions.

## Response format

All responses are **XML** (`Content-Type: text/xml`), matching the real
AWS query API shape. Errors use `<ErrorResponse><Error><Type/><Code/><Message/></Error></ErrorResponse>`.

## Example

```
GET /?Action=AssumeRole&RoleArn=arn:aws:iam::123456789012:role/my-role&RoleSessionName=dev
Authorization: AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20260120/us-east-1/sts/aws4_request, SignedHeaders=host;x-amz-date, Signature=<computed-with-the-documented-secret>

→ 200 text/xml
<AssumeRoleResponse>
  <AssumeRoleResult>
    <Credentials>
      <AccessKeyId>ASIA...</AccessKeyId>
      <SecretAccessKey>...</SecretAccessKey>
      <SessionToken>...</SessionToken>
      <Expiration>2024-01-01T01:00:00Z</Expiration>
    </Credentials>
    <AssumedRoleUser>
      <Arn>arn:aws:iam::123456789012:role/my-role</Arn>
      <AssumedRoleId>AROA...:dev</AssumedRoleId>
    </AssumedRoleUser>
  </AssumeRoleResult>
</AssumeRoleResponse>
```
