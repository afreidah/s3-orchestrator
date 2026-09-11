---
description: "How a credential resolves to a user, how that user's grants decide which virtual buckets it reaches, and how every request is authenticated and authorised before reaching a backend."
title: "Authentication and Credentials"
linkTitle: "Authentication"
weight: 22
---

## SigV4 and multi-bucket auth


A credential proves a caller is one **user**. That user holds a **grant** on each virtual bucket it may reach. On every request, the orchestrator:

1. Extracts the access key from the SigV4 `Authorization` header, presigned URL query parameters, or token from `X-Proxy-Token`.
2. Resolves the credential to the user behind it.
3. Verifies the signature (SigV4 header or presigned query parameters) or token.
4. Asks that user whether it holds a grant on the bucket in the URL path.

Three auth methods are supported, checked in order:

1. **AWS SigV4** (recommended) - Standard AWS Signature Version 4 via the `Authorization` header. Compatible with `aws cli`, SDKs, and any S3 client. Signature verification is constant-time: unknown access keys still compute a full HMAC to prevent timing side-channel enumeration. Streaming-payload uploads (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`, `STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER`, `STREAMING-UNSIGNED-PAYLOAD-TRAILER`) are accepted and the chunk chain is fully validated end-to-end.
2. **Presigned URLs** - SigV4 query-parameter authentication (`X-Amz-Algorithm`, `X-Amz-Credential`, etc.) for time-limited, shareable URLs. Works with any AWS SDK presign client. Maximum expiry: 7 days. Uses the same credentials as normal requests — no additional configuration required.
3. **Legacy token** - Simple `X-Proxy-Token` header for backward compatibility.

Access key IDs must be globally unique. Authentication is always required, and a user holding no grants authenticates successfully and is refused on every bucket - which is a clearer answer for a client that has been created but not yet granted anything than a failed signature would be.

For client usage examples (AWS CLI, rclone, boto3, Go SDK), see the [User Guide](user-guide.md).
For credential rotation procedures, see [docs/operations.md](operations.md#rotating-client-credentials).

## Where credentials come from

A deployment declares credentials in two places, and both are live at once.

**The config file** declares them per bucket, as below. A credential declared this way is merged in as a user reaching exactly the one bucket that declared it, so it resolves through the same user-and-grant chain as any other and the request path has no second case to handle.

**The store** holds users, their credentials, and their grants as rows, created through the provisioning API or the `bucket`, `user`, `credential` and `grant` CLI commands. A stored credential can reach several buckets, because its user can hold several grants, and a bucket can be granted to several users.

Config wins a collision, and the API refuses to modify anything the config file declares. See [config versus the provisioning API](configuration.md#config-versus-the-provisioning-api) for the precedence rule, and [the CLI reference](cli.md#bucket-user-credential-and-grant) for the commands.

## The admin surface

The admin API is authenticated by its own `X-Admin-Token` header rather than by SigV4, but the endpoints under `/admin/api/objects` reach the same object service the S3 API does. Those are authorized against the same grants and the same permissions: a credential's token passed in that header reaches object data with exactly what its grant carries, and nothing more.

The rest of the admin API is the control plane - backend drain, key rotation, provisioning, worker triggers. Those carry no permission a bucket grant can express, so they remain the configured admin token's to authorize, and a provisioned credential is refused on them.

The configured token also still reaches object data, which is how a deployment predating this behaviour keeps working. That path is deprecated and logs a warning; see [the admin API's authorization section](admin-api.md#authorization).

## Bucket configuration


Each bucket defines a virtual namespace with one or more credential sets.

```yaml
buckets:
  - name: "app1-files"
    # max_multipart_uploads: 100  # optional; limit active multipart uploads (0 = unlimited)
    credentials:
      - access_key_id: "AKID_APP1"
        secret_access_key: "secret1"

  - name: "app2-files"
    credentials:
      - access_key_id: "AKID_APP2_INGEST"
        secret_access_key: "secret2"
      - access_key_id: "AKID_APP2_ANALYTICS"
        secret_access_key: "secret3"
```

**Generating credentials:** Use `openssl rand` to produce random keys:

```bash
# Generate an access key ID (20 chars, uppercase + digits)
openssl rand -hex 10 | tr '[:lower:]' '[:upper:]'

# Generate a secret access key (40 chars, base64)
openssl rand -base64 30
```

The provisioning API mints both halves for you, so a stored credential needs neither command:

```bash
s3-orchestrator admin -addr $ADDR -token $TOKEN credential issue -user <user_id> -label "ingest job"
```

**Validation rules:**
- Bucket names must not contain `/`.
- Bucket names must be unique across the config.
- Access key IDs must be globally unique across all buckets.
- Proxy tokens must be globally unique across all buckets. A token proves one user, so a shared one has no unambiguous identity and startup fails.
- Each bucket must have at least one credential set.
- Each credential needs either `access_key_id` + `secret_access_key` (SigV4) or `token` (legacy).

### Several credentials on one bucket

Every credential that reaches a bucket has identical access to it. There is no read-only credential and no write-only credential: the two above can both list, upload, overwrite and delete everything under `app2-files`. The names say which service holds each key, not what it may do with it.

What separate credentials do buy is worth having anyway. Each rotates and is revoked on its own, so a leaked key is withdrawn without interrupting the other services sharing the bucket. Each resolves to its own user, so the audit trail attributes an action to the service that took it rather than to a key shared by several. And revoking one leaves its siblings working, which is what makes zero-downtime rotation possible.

Scoping access below the whole bucket - read-only grants, prefix-scoped credentials, and a policy model to express them - is tracked in [#356](https://github.com/afreidah/s3-orchestrator/issues/356).

SigV4 credentials also support presigned URLs automatically. Clients can generate time-limited presigned URLs using any AWS SDK presign client — no additional configuration is needed on the orchestrator side.
