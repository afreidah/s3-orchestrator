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

Seven endpoints run long enough that a single response is unhelpful: `rebalance`, `replicate`, `over-replication`, `scrub`, `backfill-checksums`, `reconcile`, and a backend purge. They return their JSON result by default, but stream newline-delimited progress when the caller asks for it:

```bash
curl -H "X-Admin-Token: $TOKEN" \
  -H "Accept: application/x-ndjson" \
  -X POST http://localhost:9000/admin/api/scrub
```

Each line is one self-contained JSON object with an `event` field: `start` when the operation begins, `step_start` and `step_end` per item, and a final `result` carrying the outcome. The `s3-orchestrator admin` subcommand renders these as live progress.

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

## Skipped operations

Endpoints that trigger a worker report whether the pass actually ran. A response with `"status": "ok"` did the work; `"status": "skipped"` did not, and carries a `reason` explaining why -- usually that the feature is not configured. Replication endpoints skip when the factor is 1 or replication is unset; integrity endpoints skip when verification is disabled. Rebalance skips when backend utilization is already within the configured threshold, or when the strategy plans no moves.

This is not an error, so the status code is still `200`. Check `status` rather than the HTTP code when driving these from a script.

## Deliberate rejections

Two endpoints reject an input that would otherwise be a plausible convenience:

- `DELETE /admin/api/cache/prefix` requires a non-empty `prefix`. An empty one would drop every entry, and a full flush should be a deliberate call to `POST /admin/api/cache/flush` rather than an accidentally-empty parameter.
- `POST /admin/api/rotate-encryption-key` requires `old_key_id`. Rotating "whatever is current" is ambiguous during a partial rotation.

## Endpoint reference

{{< openapi src="repo/openapi.yaml" >}}
