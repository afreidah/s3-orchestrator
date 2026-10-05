---
description: "Choosing between embedded SQLite and PostgreSQL, sizing the connection pool, and the migrations that manage the metadata schema."
title: "Database"
linkTitle: "Database"
weight: 24
---

## database

The `driver` field selects between SQLite (embedded, zero-dependency) and PostgreSQL (required for multi-instance deployments). When `driver` is omitted, the orchestrator infers `postgres` if `host` is set, otherwise `sqlite`.

**SQLite (default for single-instance):**

```yaml
database:
  driver: sqlite
  path: "s3-orchestrator.db"     # default: s3-orchestrator.db
```

SQLite requires no external dependencies. The database file is created automatically on first start. Advisory lock-based leader election is replaced by a process-local mutex, so multi-instance deployments are not supported with SQLite.

**PostgreSQL (required for multi-instance):**

```yaml
database:
  driver: postgres
  host: "db.example.com"        # required
  port: 5432                     # default: 5432
  database: "s3orchestrator"     # required
  user: "s3orchestrator"         # required
  password: "${DB_PASSWORD}"
  ssl_mode: "require"            # default: require (use "disable" for local dev)
  max_conns: 50                  # default: 50; size to 2-3x max_concurrent_requests
  min_conns: 10                  # default: 5
  max_conn_lifetime: "5m"        # default: 5m
```

Pool settings (`max_conns`, `min_conns`, `max_conn_lifetime`) control the pgx connection pool. Size `max_conns` to 2-3x your `max_concurrent_requests` setting. See [Performance Tuning - Connection Pool Sizing](performance-tuning.md#connection-pool-sizing) for detailed guidance.


## Engines and schema

SQLite is the default for single-instance use; PostgreSQL is required for multi-instance deployments.

The orchestrator supports two metadata-store engines:

- **SQLite** (default) - embedded, zero-dependency, single-instance. Schema is
  applied at startup from a single consolidated `schema.sql` and pinned by a
  `schema_version` table, so a database written by a newer binary is refused
  rather than silently mis-read.
- **PostgreSQL** - required for multi-instance deployments. Connects via
  pgx/v5 pools and auto-applies versioned migrations on startup using
  [goose](https://github.com/pressly/goose); migration files are embedded
  in the binary and tracked via a `goose_db_version` table so only
  unapplied migrations run.

Engine-agnostic orchestration lives in `internal/store/core/` (transactional
business logic against a `TxAdapter` interface). Each engine package
(`internal/store/postgres/`, `internal/store/sqlite/`) is a thin adapter
that implements the same `TxAdapter`, so the same code drives both engines.

The schema currently provisions:

| Table | Purpose |
|-------|---------|
| `backend_quotas` | Per-backend byte limit and orphan bytes tracking |
| `backend_quota_stripes` | Each backend's stored byte total, split across rows so concurrent writes do not serialize on one |
| `object_locations` | Maps object keys to backends with size tracking. `storage_key` is the path the copy's bytes occupy on its backend: each write stores its bytes at `<object key>!<id>`, so two writes of one key never share a path, and a copy whose bytes are at the object key carries `storage_key = object_key`. `(backend_name, storage_key)` is unique. `size_bytes` counts what the backend holds, so the `compression_*` columns and `logical_size` are what describe the object it decodes to; a NULL algorithm means the bytes are stored verbatim. The `managed` flag is false for objects reconcile found outside every configured virtual bucket prefix, and for encrypted objects it found with no key to decrypt them: they count toward quota, but replication, rebalance, integrity and drain skip them, and S3 listings and reads treat them as absent |
| `object_tags` | One row per [tag](tagging.md) on an object, keyed `(object_key, tag_key)`. Keyed by object key alone, not by backend, because a tag set describes the object and per-replica rows would let copies of one key disagree. `idx_object_tags_lookup` on `(tag_key, tag_value)` serves the reverse lookup: which objects carry a given tag |
| `multipart_uploads` | In-progress multipart upload metadata, including the `tagging` column holding a query-string-encoded tag set from `CreateMultipartUpload` until the upload completes |
| `multipart_parts` | Individual parts for active multipart uploads |
| `backend_usage` | Monthly per-backend API request and data transfer counters. `api_requests` counts every call made, including operations no budget charges, so it stays an honest record of request volume |
| `backend_request_usage` | Monthly per-backend request counts keyed by the pool names config declares, which is what admission judges a request against. Pools are additive - an operation charges every pool containing it - so these do not sum to `backend_usage.api_requests` and are not a decomposition of it |
| `cleanup_queue` | Retry queue for failed backend object deletions. The worker deletes at `storage_key`; `object_key` names the object the orphan belonged to |
| `cleanup_dlq` | Dead-letter for `cleanup_queue` rows that exhausted retries; surfaces unrecoverable orphans for operator action. Carries the same `storage_key` and `object_key` as the queue row |
| `pending_objects` | In-flight PUT intents recorded before the backend write so a DB outage can't silently destroy the prior copy. `storage_key` is the path the upload writes, which the reaper HEADs and deletes at. Carries the same stored-form columns as `object_locations`, so an intent the reaper promotes describes bytes that can actually be read |
| `notification_outbox` | Durable webhook event delivery queue |
| `backend_drains` | One row per backend with a [drain](operations.md#draining-a-backend), from the moment it starts until it is cancelled or the backend is removed: its state (`draining`, `drained`, or `failed`), objects moved, the error that stopped a failed drain, and when it started and finished. No foreign key to `backend_quotas`, so the record outlives that row |

The schema also defines one view, `backend_capacity`: per backend, the byte limit, stored bytes, orphan bytes, in-flight bytes (incomplete multipart parts plus the intents of writes still uploading), the bytes still available (NULL when there is no limit), and whether the backend accepts writes, which it does not while it has a drain record. Every admission test reads it, so a pending intent, a replica, and a multipart upload are all refused by the same definition, and the quota tracker refreshes its baselines from the same figures.

Quota updates are transactional: object location inserts/deletes and quota counter changes happen atomically.

`object_tags` carries no foreign key, because there is no table to point at: `object_locations` is keyed `(object_key, backend_name)` and nothing is keyed on object key alone, so `ON DELETE CASCADE` cannot express the relationship. The store clears the rows explicitly instead, in the same transaction and under the same key lock as the write that orphaned them - every path that puts a new object at a key or removes the last copy of one.

All Postgres SQL queries live in `internal/store/postgres/sqlc/queries/` as annotated `.sql` files. Type-safe Go code is generated by sqlc into `internal/store/postgres/sqlc/`. To regenerate after editing queries:

```bash
make generate
```
