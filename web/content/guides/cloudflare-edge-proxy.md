---
title: "Cloudflare Edge Proxy"
description: "Route a backend through a Cloudflare Worker so its reads leave the provider to Cloudflare: how the worker handles a request, how to deploy it, and the backend settings and limits that come with it."
weight: 3
---


Egress is data leaving a provider's network, and providers bill or limit it. Members of Cloudflare's [Bandwidth Alliance](https://www.cloudflare.com/bandwidth-alliance/) discount or waive those fees on traffic going to Cloudflare. A Cloudflare Worker in front of a backend sends that backend's reads through Cloudflare, so those terms apply.

The worker is in [`deploy/cloudflare-worker/`](https://github.com/afreidah/s3-orchestrator/tree/main/deploy/cloudflare-worker). Backblaze B2, IBM Cloud Object Storage and Oracle Cloud have all run behind it. Terms vary by provider, so check yours before changing a budget.

On one deployment, routing B2 through a worker moved 1,679 MB out of the backend in a day, and Backblaze counted 42 MB of it as egress.

A worker does nothing for a provider outside the alliance, or for Cloudflare R2, whose egress is already free.

## Proxied DNS Records and S3 Signatures

Pointing a proxied Cloudflare hostname like `b2-proxy.example.com` at a bucket doesn't work, because of how S3 requests are signed.

Every S3 request carries a signature. The client hashes parts of the request with a secret key, and the hostname is one of those parts. The server repeats the hash on what it received and accepts the request if the two match.

- If the hostname stays `b2-proxy.example.com`, the provider doesn't recognize it.
- If the hostname changes to the provider's on the way through, the hashes no longer match and the provider answers `SignatureDoesNotMatch`.

Something in the middle has to receive the request and sign a new one. That is the worker's job.

## How a Request Flows

A Cloudflare Worker is a small program that runs on Cloudflare's servers. When a request arrives for a hostname, Cloudflare runs the worker and lets it decide what to do. This is one read:

<svg class="edge-flow" width="714" height="446" style="display:block;max-width:100%;height:auto;margin:24px 0" viewBox="23 0 714 446" role="img" aria-labelledby="ef-t ef-d" xmlns="http://www.w3.org/2000/svg">
  <title id="ef-t">One read through the worker</title>
  <desc id="ef-d">Step 1: s3-orchestrator signs a request with the proxy key and sends it to b2-proxy.example.com. Step 2: the Cloudflare Worker checks the signature, and a wrong signature gets a 403 and stops there. Step 3: the worker builds a new request and signs it with the provider key. Step 4: the provider sends the object to the worker, on the path the alliance terms cover. Step 5: the worker streams the object back to s3-orchestrator.</desc>
  <style>
    .edge-flow text { font-family: inherit; fill: currentColor; }
    .edge-flow .lbl  { font-size: 16px; font-weight: 600; }
    .edge-flow .sub  { font-size: 14px; opacity: .72; }
    .edge-flow .step { font-size: 15px; }
    .edge-flow .mono { font-size: 13.5px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
    .edge-flow text.num { font-size: 13px; font-weight: 700; fill: #fff; text-anchor: middle; }
    .edge-flow .dot   { fill: #3b82f6; }
    .edge-flow .dot-b { fill: #ea580c; }
    .edge-flow .box  { fill: none; stroke: currentColor; stroke-opacity: .3; stroke-width: 1.5; }
    .edge-flow .ok   { fill: none; stroke: #3b82f6; stroke-width: 1.5; }
    .edge-flow .stop { fill: none; stroke: #ef4444; stroke-width: 1.5; }
    .edge-flow .ln   { fill: none; stroke: #3b82f6; stroke-width: 2; stroke-linecap: round; }
    .edge-flow .ln-r { fill: none; stroke: #ef4444; stroke-width: 2; stroke-linecap: round; }
    .edge-flow .ln-b { fill: none; stroke: #ea580c; stroke-width: 2; stroke-linecap: round; }
  </style>
  <defs>
    <marker id="efa" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto"><path d="M0 0l10 5-10 5z" fill="#3b82f6"/></marker>
    <marker id="efr" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto"><path d="M0 0l10 5-10 5z" fill="#ef4444"/></marker>
    <marker id="efb" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto"><path d="M0 0l10 5-10 5z" fill="#ea580c"/></marker>
  </defs>
  <rect x="24" y="14" width="712" height="72" rx="12" class="box"/>
  <text x="44" y="42" class="lbl">s3-orchestrator</text>
  <text x="44" y="66" class="mono">Host: b2-proxy.example.com</text>
  <path class="ln" marker-end="url(#efa)" d="M130 86V164"/>
  <circle cx="130" cy="124" r="12" class="dot"/><text x="130" y="129" class="num">1</text>
  <text x="154" y="120" class="lbl">request</text>
  <text x="154" y="140" class="sub">signed with the proxy key</text>
  <path class="ln-b" marker-end="url(#efb)" d="M470 166V88"/>
  <circle cx="470" cy="128" r="12" class="dot-b"/><text x="470" y="133" class="num">5</text>
  <text x="494" y="133" class="lbl">object</text>
  <rect x="24" y="166" width="712" height="116" rx="12" class="ok"/>
  <text x="44" y="194" class="lbl">Cloudflare Worker</text>
  <circle cx="56" cy="222" r="12" class="dot"/><text x="56" y="227" class="num">2</text>
  <text x="78" y="227" class="step">check the signature</text>
  <path class="ln-r" marker-end="url(#efr)" d="M244 222H494"/>
  <circle cx="56" cy="254" r="12" class="dot"/><text x="56" y="259" class="num">3</text>
  <text x="78" y="259" class="step">build a new request, sign it with the provider key</text>
  <rect x="496" y="180" width="222" height="84" rx="10" class="stop"/>
  <text x="512" y="214" class="lbl">wrong signature</text>
  <text x="512" y="236" class="sub">answer 403 and stop</text>
  <path class="ln" marker-end="url(#efa)" d="M130 282V360"/>
  <text x="154" y="318" class="lbl">new request</text>
  <text x="154" y="338" class="sub">signed with the provider key</text>
  <path class="ln-b" marker-end="url(#efb)" d="M470 362V284"/>
  <circle cx="470" cy="322" r="12" class="dot-b"/><text x="470" y="327" class="num">4</text>
  <text x="494" y="318" class="lbl">object</text>
  <text x="494" y="338" class="sub">egress to Cloudflare</text>
  <rect x="24" y="362" width="712" height="72" rx="12" class="box"/>
  <text x="44" y="390" class="lbl">Provider</text>
  <text x="44" y="414" class="mono">Host: s3.us-east-005.backblazeb2.com</text>
</svg>

1. The orchestrator signs a request with the **proxy key**, a key only the worker accepts, and sends it to the worker's hostname.
2. The worker checks that signature. If it's wrong, the worker answers with an S3 error and stops.
3. The worker builds a new request with the provider's hostname and signs it with the **provider key**, the access key from your provider account. See [How the Worker Rebuilds a Request](#how-the-worker-rebuilds-a-request).
4. The provider accepts it and sends the object to the worker. This is the transfer the alliance terms cover.
5. The worker streams the object back to the orchestrator without buffering it.

Every call to the backend takes this path, including writes and deletes, so the limits below apply to all of them.

The worker answers `501 NotImplemented` for presigned URLs and for `STREAMING-*` payload signatures. The orchestrator sends neither to a backend, so this only matters if something else is pointed at the worker.

## How the Worker Rebuilds a Request

The worker can't pass along the request it received, because that request is addressed to the worker and signed with the proxy key. The provider would reject it. So the worker makes a copy for the provider, changes what has to change, and signs the copy with the provider key.

Here is the same read before and after, with long values shortened.

The request the worker receives:

```text
GET /example-bucket/photos/cat.jpg
Host: b2-proxy.example.com
Authorization: AWS4-HMAC-SHA256
    Credential=PROXYKEYID/20261105/us-east-005/s3/aws4_request,
    SignedHeaders=host;x-amz-content-sha256;x-amz-date,
    Signature=8b2c3f6c...
X-Amz-Date: 20261105T175848Z
X-Amz-Content-Sha256: e3b0c442...
Range: bytes=0-1023
CF-Connecting-IP: 203.0.113.7
CF-Ray: 8d2f0a1b2c3d4e5f-LAX
X-Forwarded-For: 203.0.113.7
Connection: keep-alive
```

The request the worker sends to the provider:

```text
GET /example-bucket/photos/cat.jpg
Host: s3.us-east-005.backblazeb2.com
Authorization: AWS4-HMAC-SHA256
    Credential=PROVIDERKEYID/20261105/us-east-005/s3/aws4_request,
    SignedHeaders=host;x-amz-content-sha256;x-amz-date,
    Signature=60099f7b...
X-Amz-Date: 20261105T175849Z
X-Amz-Content-Sha256: e3b0c442...
Range: bytes=0-1023
```

**Kept**, because these say what the caller wants:

- The method, path and query string.
- The body, which streams through without the worker reading it.
- `X-Amz-Content-Sha256`, which describes the body. The body didn't change, so the value is still right.
- Other headers, like `Range`.

**Replaced**, because they were only true for the first request:

- `Host` becomes the provider's hostname, because the provider only answers to its own name.
- `X-Amz-Date` becomes the current time, because this is a new request and providers refuse old timestamps.
- `Authorization` gets a new key and signature, because the old one was made with the proxy key.

**Removed**, because they don't apply to the next hop:

- `CF-*`, `X-Forwarded-*` and `X-Real-IP`, which Cloudflare added for the worker.
- `Connection`, `Transfer-Encoding` and similar headers, which describe the connection into the worker.

To sign the copy, the worker writes it out as text in the exact format SigV4 requires. This text is called the canonical request:

```text
GET
/example-bucket/photos/cat.jpg

host:s3.us-east-005.backblazeb2.com
x-amz-content-sha256:e3b0c442...
x-amz-date:20261105T175849Z

host;x-amz-content-sha256;x-amz-date
e3b0c442...
```

From top to bottom, that's the method, the path, the query string (empty here), the signed headers, the names of those headers, and the hash of the body. The worker signs `host`, `content-type`, `content-md5` and every `x-amz-*` header on the request. Other headers, like `Range`, are sent but not signed.

The worker hashes that text with a key derived from the provider's secret, and the result is the `Signature` in the new `Authorization` header. The provider writes out the same text from the request it received and repeats the hash. If a single character differs, the signatures don't match. That's why the hostname can't just be changed on the way through: it's one of the lines in this text.

The worker then sends the copy to the provider. It doesn't follow redirects, because a redirect would send a request signed for one host to a different one.

## Keys

The worker uses two keypairs.

**The provider key** is the access key from your provider account. Create it in the provider's console and scope it to the one bucket. You give it to the worker as the `ORIGIN_ACCESS_KEY_ID` and `ORIGIN_SECRET_ACCESS_KEY` secrets in [Step 1](#step-1-deploy-the-worker). Cloudflare stores them, and they appear as fields on `env`, so the worker reads `env.ORIGIN_SECRET_ACCESS_KEY` when it signs. The orchestrator's config doesn't contain this key.

**The proxy key** is a keypair you generate yourself, for example with `openssl rand -hex 20`. Both ends need a copy, because a signature only verifies if both sides hold the same secret:

- One copy goes to the worker as the `PROXY_ACCESS_KEY_ID` and `PROXY_SECRET_ACCESS_KEY` secrets.
- The other goes in the backend's `access_key_id` and `secret_access_key` in [Step 3](#step-3-configure-the-backend).

The split keeps the provider key out of the orchestrator's config. The proxy key still needs the same care, because the worker re-signs any request that verifies and does not restrict the path. Whoever has the proxy key can do anything the provider key allows. **Scope the provider key to the one bucket.**

The worker holds one proxy keypair at a time. To rotate it, update the secrets and restart the orchestrator together. Requests signed with the old key fail in between.

Cloudflare terminates TLS, so it sees object keys and request headers, and it stores the provider key as a secret. With [encryption](../../docs/encryption/) on, the object bytes it sees are already ciphertext.

## Prerequisites

- A domain on Cloudflare
- An account on an alliance provider, and a key scoped to the bucket
- Node.js, to build and deploy the worker

## Step 1: Deploy the Worker

One worker fronts one backend. Three alliance backends means deploying the same script three times, under different names and routes.

### With Wrangler

Set the worker's name, origin and route in `deploy/cloudflare-worker/wrangler.toml`. The file ships without a route, so add one. `ORIGIN_HOST` is a bare hostname with no scheme.

```toml
name = "s3-orchestrator-b2-proxy"
main = "src/index.ts"
compatibility_date = "2026-09-07"
routes = [{ pattern = "b2-proxy.example.com/*", zone_name = "example.com" }]

[vars]
ORIGIN_HOST = "s3.us-east-005.backblazeb2.com"
ORIGIN_REGION = "us-east-005"
MAX_CLOCK_SKEW_SECONDS = "300"
```

`MAX_CLOCK_SKEW_SECONDS` is how far a request's timestamp can be from the worker's clock, in either direction.

Set the four secrets, then deploy:

```bash
cd deploy/cloudflare-worker
npx wrangler secret put ORIGIN_ACCESS_KEY_ID
npx wrangler secret put ORIGIN_SECRET_ACCESS_KEY
npx wrangler secret put PROXY_ACCESS_KEY_ID
npx wrangler secret put PROXY_SECRET_ACCESS_KEY
cd ../..
make worker-deploy
```

### With Terraform

Cloudflare runs JavaScript and the worker is TypeScript, so build it first. `make worker-build` writes `deploy/cloudflare-worker/dist/worker.js`.

```hcl
resource "cloudflare_workers_script" "b2_proxy" {
  account_id         = var.account_id
  script_name        = "s3-orchestrator-b2-proxy"
  content            = file("${path.module}/worker.js")
  main_module        = "worker.js"
  compatibility_date = "2026-09-07"

  bindings = [
    { name = "ORIGIN_HOST", type = "plain_text", text = "s3.us-east-005.backblazeb2.com" },
    { name = "ORIGIN_REGION", type = "plain_text", text = "us-east-005" },
    { name = "ORIGIN_ACCESS_KEY_ID", type = "secret_text", text = var.origin_access_key },
    { name = "ORIGIN_SECRET_ACCESS_KEY", type = "secret_text", text = var.origin_secret_key },
    { name = "PROXY_ACCESS_KEY_ID", type = "secret_text", text = var.proxy_access_key },
    { name = "PROXY_SECRET_ACCESS_KEY", type = "secret_text", text = var.proxy_secret_key },
  ]
}

resource "cloudflare_workers_route" "b2_proxy" {
  zone_id = var.zone_id
  pattern = "b2-proxy.example.com/*"
  script  = cloudflare_workers_script.b2_proxy.script_name
}
```

Terraform keeps `secret_text` values in state, so the keys are only as protected as the state backend.

To read the script from a bucket instead of a local file, `make worker-publish` uploads it to `s3://$S3O_BUCKET/s3-orchestrator/cloudflare-worker/<version>/worker.js`. It's stored as `text/plain`, because Terraform's `aws_s3_object` data source only returns a body it considers text.

## Step 2: Create the DNS Records

A worker route only matches a hostname that goes through Cloudflare, so the name has to exist there with the proxy turned on. The worker answers every request itself, so no server sits behind it. Create a proxied `A` record pointing at `192.0.2.1`, an address reserved for documentation.

If a DNS server on your network answers the zone with a wildcard, the orchestrator never reaches Cloudflare. Give the proxy hostname its own record there, pointing at a Cloudflare address for the zone.

To check the route, send an unsigned request from a host the orchestrator runs on:

```bash
curl -s -o /dev/null -w '%{http_code}\n' https://b2-proxy.example.com/
```

`403` means the worker answered.

## Step 3: Configure the Backend

Point the backend at the worker's hostname and give it the proxy key:

```yaml
backends:
  - name: "b2"
    endpoint: "https://b2-proxy.example.com"
    region: "us-east-005"
    bucket: "example-bucket"
    access_key_id: "<PROXY_ACCESS_KEY_ID>"
    secret_access_key: "<PROXY_SECRET_ACCESS_KEY>"
    force_path_style: true
    strip_sdk_headers: true
    max_object_size: 95000000      # under the zone's request body limit
    quota_bytes: 10000000000
    egress_byte_limit: 0           # unlimited, once the provider's terms are confirmed
```

- **`strip_sdk_headers: true`** is required. The Go SDK includes `accept-encoding` in its signature, and [Cloudflare overwrites that header](https://developers.cloudflare.com/fundamentals/reference/http-headers/) on the way in. The worker then sees a different request from the one that was signed, so every request fails with `SignatureDoesNotMatch`. The flag removes the header before signing.
- **`force_path_style: true`** is required. The route matches one hostname, so a bucket name in the hostname would not reach the worker.
- **`max_object_size`** keeps large objects off this backend. See [Request Body Size](#request-body-size).
- **`unsigned_payload`** can be either value. Over HTTPS the orchestrator sends `UNSIGNED-PAYLOAD` both ways, and the worker accepts it. Setting it to `false` buffers streamed bodies so the SDK can resend them after a `503`. See [Backends](../../docs/backends/).

Changing a backend's endpoint or credentials needs a restart. The bucket and object keys stay the same, so an existing backend moves behind a worker with nothing to copy.

## Limits

### Request Body Size

Cloudflare limits request bodies by the plan of the zone: 100 MB on Free and Pro, 200 MB on Business, and up to 5 GB on Enterprise. Over the limit, Cloudflare answers `413` with an HTML page, which the orchestrator logs as `error while deserializing xml error response`.

The limit applies to every PUT, including replication copies and rebalance moves. Smaller multipart parts don't help, because the orchestrator stores each part as its own object and `CompleteMultipartUpload` writes the finished object in one PUT.

Set `max_object_size` below the limit. It's compared against the stored size, after compression and encryption, and it keeps larger objects off the backend for writes, replication and rebalance.

{{< callout type="warning" >}}
`max_object_size` does not cover multipart uploads. The final size isn't known when a backend is picked, and completion has to happen on the backend holding the parts. An upload that ends up over the limit on a proxied backend fails at `CompleteMultipartUpload`. Retrying the completion fails the same way, so the client has to start a new upload, which may land on another backend.
{{< /callout >}}

### Workers Plan

Every backend call is a worker request, including HEADs, lists, scrubber reads and replication.

| | Workers Free | Workers Paid |
|---|---|---|
| Requests | 100,000 per day per account, resetting at midnight UTC | 10 million per month included, then $0.30 per million |
| CPU per request | 10 ms | 30 seconds by default |
| Bandwidth | Not billed | Not billed |

Past the Free limit, requests fail with Cloudflare error 1027. All workers on an account share that limit. One deployment running three workers averaged 12,000 requests a day and peaked at 65,000 in 24 hours, so plan for a drain or rebalance rather than an average day. These figures are from Cloudflare's [limits](https://developers.cloudflare.com/workers/platform/limits/) and [pricing](https://developers.cloudflare.com/workers/platform/pricing/) pages.

### Provider Request Limits

The alliance covers data transfer only, so every read still counts as a request against the provider's allowance. Relax `egress_byte_limit` on a proxied backend and leave the request budgets as they were.

A provider can also refuse reads on a cap of its own. A B2 account behind this worker started answering `403 AccessDenied: Cannot download file, download bandwidth or transaction (Class B) cap exceeded` after about 2,500 reads in a day, and recovered at midnight GMT when [Backblaze's daily counters reset](https://www.backblaze.com/docs/cloud-storage-data-caps-and-alerts). Until then, a read with no other reachable copy returned `502` to the client. Check the provider's caps as well as its pricing page. Request budgets here are monthly, so they can't express a daily cap exactly.

### Cloudflare's Terms

[Cloudflare's service terms](https://www.cloudflare.com/service-specific-terms-application-services/) limit serving video and other large files through the CDN on Free, Pro and Business outside its paid services, and name the Developer Platform among those. Read them against what you store.

### Caching

The worker doesn't cache. Each read goes to the provider.

## Troubleshooting

| Symptom | Likely cause |
|---------|--------------|
| `SignatureDoesNotMatch` on every request | `strip_sdk_headers` is not set, or `PROXY_SECRET_ACCESS_KEY` differs from the backend's `secret_access_key` |
| `InvalidAccessKeyId` with the message `Unknown access key.` | The backend's `access_key_id` is not `PROXY_ACCESS_KEY_ID` |
| `RequestTimeTooSkewed` | The orchestrator's clock is outside `MAX_CLOCK_SKEW_SECONDS` |
| `501 NotImplemented` | The caller is not the orchestrator: a presigned URL or a chunked payload signature |
| `403` with the provider's own error | `ORIGIN_*` credentials or region are wrong, the key can't reach this bucket, or a provider cap was reached |
| `413` with an XML parse error | The body is over the zone's limit; see [Request Body Size](#request-body-size) |
| Cloudflare error 1027 | The Workers Free daily limit |
| `404` or a page from your own ingress | A DNS server on your network is answering the proxy hostname |

## Development

`make worker-check` runs the typecheck and the test suite with coverage thresholds. The [worker README](https://github.com/afreidah/s3-orchestrator/tree/main/deploy/cloudflare-worker) covers the source layout.
