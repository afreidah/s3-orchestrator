---
description: "The operational control plane for a running instance: backend health, worker state, replication and cleanup queues, integrity passes, and caches."
title: "Admin API"
linkTitle: "Admin API"
---

The admin API is the operational control plane for a running instance: backend health, worker state, replication and cleanup queues, integrity passes, cache control, and destructive backend removal.

The reference below is generated from the server's route table, so it always matches the code that serves it. Everything the endpoints exchange is described there; this page covers the parts a schema cannot express.

## Authentication

Every endpoint requires the `X-Admin-Token` header:

```bash
curl -H "X-Admin-Token: YOUR_ADMIN_TOKEN" \
  http://localhost:9000/admin/api/status
```

The token is the `ui.admin_token` value from the configuration file, falling back to `ui.admin_key` when it is not set. The `s3-orchestrator admin` subcommand reads it from configuration automatically, so prefer that for interactive use.

Requests without a valid token get `401` with a JSON body. Request bodies are capped at 1 MB.

## Authorization

The endpoints under `/admin/api/objects` read and write object data, reaching the same service the S3 API does. They are authorized against the permissions the caller's grant carries, not against the token alone: browsing needs `list`, downloading needs `read`, uploading needs `write`, removing a key or a prefix needs `delete`, and the tag endpoints need `tags`. A caller whose grant does not carry what the operation needs gets `403`.

A provisioned credential reaches those endpoints with exactly the grants it holds. Pass its token in the same `X-Admin-Token` header:

```bash
curl -H "X-Admin-Token: $CREDENTIAL_TOKEN" \
  http://localhost:9000/admin/api/objects/photos/holiday.jpg
```

Two limits follow from grants being per bucket. A prefix naming no single bucket -- the empty prefix, or a partial name like `pho` -- cannot be authorized against one grant and is refused. And a provisioned credential reaches only the object endpoints: the control plane carries no permissions a bucket grant can express, so a fleet operation like draining a backend is refused with `403`.

The configured `ui.admin_token` still reaches every endpoint, which is what an existing deployment relies on. That is deprecated: using it to reach object data logs a warning naming the endpoint, and a future release removes it in favour of one credential type across both surfaces. Issue a provisioned credential for anything touching object data.

## Streaming progress

Twelve endpoints run long enough that a single response is unhelpful: `rebalance`, `replicate`, `over-replication`, `scrub`, `backfill-checksums`, `reconcile`, `lifecycle`, `compress-existing`, `decompress-existing`, `encrypt-existing`, `decrypt-existing`, and a backend purge. They return their JSON result by default, but stream newline-delimited progress when the caller asks for it:

```bash
curl -H "X-Admin-Token: $TOKEN" \
  -H "Accept: application/x-ndjson" \
  -X POST http://localhost:9000/admin/api/scrub
```

Each line is one self-contained JSON object with an `event` field: `start` when the operation begins, `step_start` and `step_end` per item, and a final `result` carrying the outcome. The `s3-orchestrator admin` subcommand renders these as live progress.

## Restricting a pass to one backend

Six of the maintenance passes -- `scrub`, `backfill-checksums`, `encrypt-existing`, `decrypt-existing`, `compress-existing` and `decompress-existing` -- accept a `backend` parameter naming the one backend whose copies they read:

```bash
curl -H "X-Admin-Token: $TOKEN" \
  -X POST "http://localhost:9000/admin/api/compress-existing?backend=wasabi-eu"
```

Omitting it runs the pass against every backend, which is what these did before the parameter existed. `reconcile` and the cleanup DLQ endpoints already took the same parameter and are unchanged.

Three reasons to reach for it. A re-encode or re-encrypt pass rewrites every object the deployment holds, and scoping it is how an operator tries one provider before committing the fleet. The `delay_ms` pacing on `backfill-checksums` applies to the whole pass rather than the backend it is reading from, so a run sized for the most expensive provider throttles the rest. And a backend restored from a failure needs only its own copies hashed or re-encrypted.

The filter is applied when candidates are selected, not after, so `max` is spent on copies the pass will act on rather than on rows it would discard. A name this instance does not serve is refused with `400`: a filter matching nothing is indistinguishable from a fleet with no work left, so a typo would otherwise report a clean pass over a backend that was never read.

## When a conversion meets a live write

The four conversions -- `encrypt-existing`, `decrypt-existing`, `compress-existing` and `decompress-existing` -- read a stored copy, transform it, and write it back. A client writing the same key in between is a race the pass is expected to lose sometimes, and the result carries a `changed` count for the copies it lost.

A conversion commits only while the copy still reports the ETag it read. When a client has replaced the object, no row matches and nothing is recorded, so the ledger never ends up describing bytes a newer write already replaced. Those copies are reported as `changed` rather than `failed`: nothing is misconfigured, and the newer write has already stored the object in whatever form the write path was set to use.

A non-zero `changed` count is worth acting on. The transformed bytes reached the backend before the commit refused, so each one names a copy whose stored bytes are the pass's output written over a newer client write -- the object reverted. Each is recorded as a `storage.ConversionRaced` audit event naming the key, the backend and the ETag the pass expected, and counted by `s3o_bulk_rewrite_copy_changed_total`. If you see them, run the conversion against a quieter window.

## Verifying one object

`POST /admin/api/object-scrub?key=...` reads every recorded copy of one key and compares it against the stored content hash, without waiting for the scrub queue to reach it. The response reports one verdict per copy -- `verified`, `mismatch`, `unreadable`, or `not_hashed` -- so a replicated object with one good copy and one bad one names the backend at fault.

A `mismatch` is acted on, not just reported: the bad copy is discarded and replication rebuilds it from a good one. The endpoint returns `404` when no copies of the key are recorded and `409` when integrity verification is disabled.

## Reading and writing objects

`/admin/api/objects` covers the object namespace itself: browse a page of it, stream one object down, store one, or remove a key or a whole prefix. These exist so an operator on a terminal can inspect and repair data without a browser session or a second S3 client:

```bash
# Stream one object to a local file.
curl -H "X-Admin-Token: $TOKEN" \
  http://localhost:9000/admin/api/objects/backups/db/2026-08-15.sql -o dump.sql

# Store one. The body is the object bytes; Content-Length is required.
curl -X PUT -H "X-Admin-Token: $TOKEN" \
  --data-binary @dump.sql \
  http://localhost:9000/admin/api/objects/backups/db/2026-08-15.sql
```

Every key must name a configured virtual bucket, the same requirement the dashboard enforces, so a typo cannot write outside the namespace the orchestrator serves. Uploads are capped at 512 MiB; a larger one is refused before it reaches a backend.

`GET /admin/api/objects` browses hierarchically by default: omit `delimiter` and keys are grouped into directories, which is what a file browser wants. Send `delimiter=` explicitly - present but empty - and the listing is flat, every key under the prefix in one stream. That is what a caller counting or sweeping a subtree needs, and it is how the TUI knows how many objects a prefix delete is about to remove before it asks.

Deletes report how many objects they removed, so a caller can tell a no-op from a mass removal:

```bash
curl -X DELETE -H "X-Admin-Token: $TOKEN" \
  "http://localhost:9000/admin/api/objects?prefix=backups/db/2025-"
# {"deleted":48}
```

A prefix delete that removes some objects and fails on others answers `500` carrying the counts it did achieve (`deleted`, `failed`, `total`), because the prefix is left half removed and the caller needs to know that rather than to retry blind.

## Object tags

`/admin/api/objects/tags/{key}` reads, replaces and clears one object's tag set. It exchanges JSON rather than the `Tagging` XML the S3 endpoints use, because its callers are `adminctl`, the dashboard and the TUI:

```bash
# Read the set.
curl -H "X-Admin-Token: $TOKEN" \
  http://localhost:9000/admin/api/objects/tags/photos/report.pdf
# {"tags":[{"key":"team","value":"infra"}]}

# Replace it wholesale.
curl -X PUT -H "X-Admin-Token: $TOKEN" \
  -d '{"tags":[{"key":"team","value":"infra"}]}' \
  http://localhost:9000/admin/api/objects/tags/photos/report.pdf
```

`PUT` replaces the whole set rather than merging into it, matching `PutObjectTagging`. An empty list leaves the object untagged, which is the same outcome as `DELETE`. An untagged object reads back as `{"tags":[]}` rather than `null`.

A key holding no copies is refused with `404`: tags belong to an object, so there is nothing to attach them to. Exceeding a tag limit answers `400` with a message naming the measurement that broke it. Request bodies are capped at 64 KiB, far above the few kilobytes ten maximum-length tags occupy.

These endpoints reach the same stored set as the S3 `?tagging` operations, under the same per-key lock. See [Object Tagging](tagging.md) for the semantics and the limits.

## Removing a backend

`DELETE /admin/api/backends/{name}` is the one destructive endpoint, and the only one whose response shape depends on how it is called.

Without `purge=true` it drops the backend's database records and returns immediately. The objects stay on the backend's storage, so the removal is reversible by re-adding the backend and reconciling.

With `purge=true` it deletes the objects too, behind a two-phase confirmation:

```bash
# Phase 1: preview. Returns what would be destroyed, plus a token.
curl -X DELETE -H "X-Admin-Token: $TOKEN" \
  "http://localhost:9000/admin/api/backends/oci?purge=true"

# Phase 2: replay the token to execute. The token expires after 60 seconds.
curl -X DELETE -H "X-Admin-Token: $TOKEN" \
  "http://localhost:9000/admin/api/backends/oci?purge=true&confirm=THE_TOKEN"
```

The confirmation token is signed and scoped to the backend it was issued for; it cannot be reused for a different one.

## Provisioning buckets and credentials

A deployment declares virtual buckets and the credentials that reach them in two
places: the config file, and the store. `GET /admin/api/provisioning` returns
both at once - every bucket, every user with the buckets it reaches, and every
credential - each entry carrying a `source` of `config` or `store`.

`source` is what tells you whether an entry can be changed. Config-declared
entries are visible and read-only: the API answers `403` to any attempt to
modify one, because an operator reading the config file has to be able to trust
what it says. Change those by editing the file and sending `SIGHUP`.

Onboarding a client is three calls: create a user, mint a keypair for it, grant
it a bucket.

```bash
# 1. Create the identity. The response carries the generated user_id.
curl -X POST -H "X-Admin-Token: $TOKEN" \
  -d '{"name":"nightly-backup"}' \
  http://localhost:9000/admin/api/provisioning/users

# 2. Mint a keypair for it. This response is the only place the secret appears.
curl -X POST -H "X-Admin-Token: $TOKEN" \
  -d '{"user_id":"user-abc123","label":"backup job"}' \
  http://localhost:9000/admin/api/provisioning/credentials

# 3. Grant it a bucket, from either source.
curl -X POST -H "X-Admin-Token: $TOKEN" \
  -d '{"user_id":"user-abc123","bucket":"backups"}' \
  http://localhost:9000/admin/api/provisioning/grants
```

The secret is returned once, by the request that minted it, and is never read
back into any listing. A caller that loses it mints a replacement and revokes
the old one; several keypairs may name one user, which is what lets one be
rotated while the rest keep working.

Every change takes effect on the next request. The registry the request path
authenticates against is rebuilt before the call returns, so a credential
created here works immediately and one revoked here stops working immediately.

Removal is refused while something still depends on what is being removed: a
bucket that holds objects or is granted to a user, and a user that holds
credentials or grants, all answer `409` naming what is in the way. Emptying a
bucket and revoking a credential stay deliberate acts.

## Objects the orchestrator does not own

A backend's bucket can hold objects the orchestrator never wrote: data that predates it, or files placed there by something else. Reconcile records them at the key the backend holds them under and marks them **unmanaged**.

An unmanaged object counts toward the backend's `bytes_used`, because the bytes really are occupying the quota and placement decisions read those totals. Nothing else touches it: replication will not copy it, rebalance will not move it, drain will not relocate it, and scrub and checksum backfill skip it rather than spending egress reading a body the orchestrator does not manage. It is also unreachable through the S3 API, since no virtual bucket claims its key.

To bring such an object under management, move it under a virtual bucket's prefix on the backend; the next reconcile will pick it up as a normal object.

## Skipped operations

Endpoints that trigger a worker report whether the pass actually ran. A response with `"status": "ok"` did the work; `"status": "skipped"` did not, and carries a `reason` explaining why.

An operator who asks for a pass gets one. A worker that was never given a schedule still runs on demand: the endpoint falls back to the running configuration, then to defaults, rather than declining because nothing was configured. What remains skipped is work that would be meaningless: replication endpoints skip at factor 1, integrity endpoints skip when verification is disabled, encryption endpoints skip when no encryptor is configured, and rebalance skips when utilization is already within the threshold or the strategy plans no moves.

The compression endpoints skip only when no codec is available, which is not the same as compression being disabled. A codec is built either way so already-stored objects stay readable, so `compress-existing` is a legitimate thing to run on a fleet that has not turned compression on for writes yet.

This is not an error, so the status code is still `200`. Check `status` rather than the HTTP code when driving these from a script.

## Deliberate rejections

Three endpoints reject an input that would otherwise be a plausible convenience:

- `DELETE /admin/api/cache/prefix` requires a non-empty `prefix`. An empty one would drop every entry, and a full flush should be a deliberate call to `POST /admin/api/cache/flush` rather than an accidentally-empty parameter.
- `DELETE /admin/api/objects` requires a non-empty `prefix`. An empty one reads as "every object", which no request should be able to mean by omission.
- `POST /admin/api/rotate-encryption-key` requires `old_key_id`. Rotating "whatever is current" is ambiguous during a partial rotation.

## Endpoint reference

On the documentation site the full endpoint reference renders below, generated
from the server's own route table so it cannot drift from the code. Reading this
file on GitHub instead, the rendered form is not available: consult
[`docs/openapi.yaml`](openapi.yaml) directly, or point any OpenAPI viewer at it.
Every route, parameter, request body and response schema is in that file.

{{< openapi src="repo/openapi.yaml" >}}
