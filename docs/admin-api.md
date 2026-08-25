---
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

## Streaming progress

Nine endpoints run long enough that a single response is unhelpful: `rebalance`, `replicate`, `over-replication`, `scrub`, `backfill-checksums`, `reconcile`, `compress-existing`, `decompress-existing`, and a backend purge. They return their JSON result by default, but stream newline-delimited progress when the caller asks for it:

```bash
curl -H "X-Admin-Token: $TOKEN" \
  -H "Accept: application/x-ndjson" \
  -X POST http://localhost:9000/admin/api/scrub
```

Each line is one self-contained JSON object with an `event` field: `start` when the operation begins, `step_start` and `step_end` per item, and a final `result` carrying the outcome. The `s3-orchestrator admin` subcommand renders these as live progress.

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

{{< openapi src="repo/openapi.yaml" >}}
