# Cloudflare Bandwidth Alliance Edge Proxy

A Cloudflare Worker that fronts a Bandwidth Alliance backend so object egress leaves the provider to Cloudflare rather than to the client. Alliance members waive the transfer fee on that leg, which turns a metered backend into an unmetered one without changing anything about how s3-orchestrator addresses it.

Providers covered by the alliance include Backblaze B2, IBM Cloud Object Storage, Oracle Cloud Infrastructure Object Storage, Wasabi, DigitalOcean Spaces, Linode Object Storage, Vultr Object Storage, Scaleway Object Storage, Alibaba Cloud OSS, and Tencent Cloud COS.

## The problem it solves

Pointing a proxied CNAME at an object store does not work on its own. Cloudflare forwards the client's `Host`, so the origin sees the proxy hostname instead of its own endpoint. That breaks bucket routing, and because `host` is a signed header under SigV4, it invalidates the signature with it -- every request comes back `SignatureDoesNotMatch`.

This worker terminates the caller's request, verifies it against a proxy credential, rewrites `Host` to the native endpoint, and re-signs with the account credential before forwarding.

## Credentials

Two credentials, deliberately distinct:

| Binding | Purpose |
|---------|---------|
| `PROXY_ACCESS_KEY_ID` / `PROXY_SECRET_ACCESS_KEY` | What callers present. s3-orchestrator's backend entry uses these. |
| `ORIGIN_ACCESS_KEY_ID` / `ORIGIN_SECRET_ACCESS_KEY` | The provider account key. Never leaves the worker. |

Splitting them means the proxy credential can be rotated without touching the backend, and a caller holding it still cannot reach the bucket directly.

Verification is not optional. A worker that re-signs without authenticating the caller signs whatever reaches it with the account key, which makes the bucket readable and writable by anyone who can resolve the hostname.

## Configuration

`wrangler.toml` carries the origin description as vars. The four credential values are secrets:

```bash
wrangler secret put ORIGIN_ACCESS_KEY_ID
wrangler secret put ORIGIN_SECRET_ACCESS_KEY
wrangler secret put PROXY_ACCESS_KEY_ID
wrangler secret put PROXY_SECRET_ACCESS_KEY
```

One worker instance fronts one backend, so a pool spanning several alliance providers deploys this script once per provider under distinct names and routes.

The corresponding s3-orchestrator backend entry points at the worker hostname and uses the proxy credential:

```yaml
backends:
  - name: "b2"
    endpoint: "https://b2-proxy.example.com"
    region: "us-west-004"
    bucket: "example-bucket"
    access_key_id: "<PROXY_ACCESS_KEY_ID>"
    secret_access_key: "<PROXY_SECRET_ACCESS_KEY>"
    force_path_style: true
    unsigned_payload: true
```

`unsigned_payload: true` matters. A client using chunked payload signatures embeds per-chunk signatures derived from its own key, which cannot be re-signed without buffering the body; the worker answers those with `501 NotImplemented` rather than forwarding something the origin will reject.

## Limits

- Cloudflare's free and Pro plans cap request body size at 100 MB, so multipart part size has to stay below that.
- Presigned URLs carry their signature in the query string and take a different verification path. The worker rejects them explicitly rather than failing them confusingly.
- Requests are re-signed, not cached. The alliance waiver applies to the origin-to-Cloudflare leg regardless.

## Development

```bash
make worker-install    # npm ci
make worker-typecheck  # tsc --noEmit
make worker-test       # vitest
make worker-coverage   # vitest with thresholds enforced
make worker-check      # typecheck + coverage
make worker-deploy     # wrangler deploy
```

The worker is the only TypeScript in an otherwise Go repository and carries its own npm toolchain here, so a contributor who never touches it never installs Node. CI runs the checks only when this directory changes, and SonarQube skips it under the existing `deploy/**` exclusion.

`src/sigv4.ts` holds the canonicalization and signing, separated from the Workers runtime so it can be tested directly. Its expected signatures come from an independent implementation of the specification rather than from this code, so a normalization mistake made in both directions of the proxy still fails the suite.
