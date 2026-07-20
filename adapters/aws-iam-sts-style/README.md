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

## Auth

**SigV4** (AWS Signature Version 4) — structural validation only.

Accepts `Authorization: AWS4-HMAC-SHA256 Credential=<AK>/YYYYMMDD/<region>/<service>/aws4_request, SignedHeaders=..., Signature=<hex>`.

The service scope may be `sts` or `iam`. The HMAC signature is **not**
recomputed (documented stretch goal); any well-formed SigV4 header is accepted.

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
| `?Action=ListRoles` | List IAM roles. **Stateful** (seeded + created roles appear). |
| `?Action=GetRole` | Get a single role by name. |
| `?Action=CreateRole` | Create a new IAM role. |
| `?Action=ListUsers` | List IAM users. |
| `?Action=CreateAccessKey` | Create an AKIA... long-term access key. |

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
Authorization: AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20260120/us-east-1/sts/aws4_request, ...

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
