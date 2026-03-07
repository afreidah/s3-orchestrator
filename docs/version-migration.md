This document covers upgrading between versions of the S3 Orchestrator, including database migrations, configuration changes, and breaking changes.

## How Upgrades Work

### Database Migrations

Database schema changes are handled automatically. The orchestrator embeds [goose](https://github.com/pressly/goose) migrations in the binary and applies any pending migrations on startup. No manual migration step is required.

```
{"level":"INFO","msg":"Database migrations applied"}
```

If a migration fails, the orchestrator logs the error and exits without starting. Fix the underlying issue and restart.

### Configuration

Configuration changes are **not** applied automatically. When a new version adds or changes config fields:

- New optional fields use sensible defaults, so existing configs continue to work.
- Removed or renamed fields cause a validation error on startup with a clear message.
- The `validate` subcommand lets you check a config file against a new binary before deploying:

  ```bash
  s3-orchestrator validate -config config.yaml
  ```

### Checking Your Version

```bash
s3-orchestrator version
# s3-orchestrator v0.8.23 go1.26.0 linux/amd64
```

## Version History

### v0.11.x (current)

**New configuration fields:**

- `rate_limit.cleanup_interval` -- stale entry eviction interval (default: 1m)
- `rate_limit.cleanup_max_age` -- entries not seen within this window are evicted (default: 5m)
- `redis` section -- optional shared usage counters via Redis for multi-instance deployments
  - `redis.address` -- Redis host:port (required when section is present)
  - `redis.password` -- AUTH password (optional)
  - `redis.db` -- Redis database number (default: 0)
  - `redis.tls` -- enable TLS (default: false)
  - `redis.key_prefix` -- key namespace (default: "s3orch")
  - `redis.failure_threshold` -- circuit breaker threshold (default: 3)
  - `redis.open_timeout` -- circuit breaker probe delay (default: 15s)

**Behavioral changes:**

- `unsigned_payload` on HTTP backends is no longer force-disabled when explicitly set to `true`. Previously, the orchestrator always forced signed (buffered) payloads over HTTP regardless of config. Now an explicit `unsigned_payload: true` is respected, which is required for large uploads to HTTP backends (MinIO, etc.) to avoid buffering the entire object in memory. HTTPS backends continue to default to unsigned payload automatically.

**New features:**

- `x-amz-meta-*` user metadata passthrough on PutObject, GetObject, HeadObject, CopyObject, and multipart uploads
- `govulncheck` CI job for Go dependency vulnerability scanning
- Optional Redis shared counters for multi-instance usage tracking with circuit breaker fallback to local counters
- Dashboard file download — download individual objects directly from the file tree in the admin UI

**New dependencies:**

- `github.com/redis/go-redis/v9`

**Database migrations:**

- `00002_multipart_metadata.sql` -- adds `metadata` JSONB column to `multipart_uploads` table (auto-applied on startup)

### v0.8.x

**New configuration fields:**

- `server.log_level` -- runtime log level (debug, info, warn, error). Default: `info`. Reloadable via SIGHUP.

**New features:**

- Admin API and CLI (`s3-orchestrator admin`) for operational tasks
- Top-level `help` subcommand listing all available commands

### v0.7.x

**New configuration fields:**

- `lifecycle.rules[]` -- object expiration rules with prefix matching and configurable retention
- `server.shutdown_delay` -- pre-stop delay for load balancer deregistration (default: 0)

### v0.6.x

**New configuration fields:**

- `backends[].ingress_byte_limit` -- monthly ingress byte limit per backend
- `usage_flush.adaptive_enabled`, `usage_flush.adaptive_threshold`, `usage_flush.fast_interval` -- adaptive usage flushing near limits
- `circuit_breaker.parallel_broadcast` -- fan-out reads during degraded mode

### v0.5.x

**New configuration fields:**

- `backends[].api_request_limit`, `backends[].egress_byte_limit` -- monthly usage limits
- `usage_flush` section -- periodic usage counter flush settings
- `server.read_header_timeout`, `server.read_timeout`, `server.write_timeout`, `server.idle_timeout` -- HTTP server timeouts
- `server.tls` section -- TLS and mTLS support

### v0.4.x

**New configuration fields:**

- `ui` section -- built-in web dashboard
- `rate_limit` section -- per-IP rate limiting
- `circuit_breaker.cache_ttl` -- key-to-backend cache during degraded mode

### v0.3.x

**New configuration fields:**

- `replication` section -- cross-backend object replication
- `rebalance.concurrency` -- parallel move operations

### v0.2.x

**New configuration fields:**

- `rebalance` section -- periodic backend rebalancing
- `buckets` section -- multi-bucket support with per-bucket credentials
- `routing_strategy` -- pack or spread write routing

### v0.1.x

Initial release with core functionality:

- Single-backend S3 proxy with PostgreSQL metadata
- Quota-based write routing across multiple backends
- Basic SigV4 authentication

## Rollback Considerations

### Database

Goose migrations support `-- +goose Down` sections for rollback. However, rolling back database migrations is generally not recommended in production because:

- Down migrations may drop columns or tables that the newer version populated with data.
- The older binary may not understand schema changes made by the newer version's startup logic.

**Recommended approach:** take a database backup before upgrading and restore it if you need to roll back.

```bash
# Before upgrade
pg_dump -h localhost -U s3proxy s3proxy > backup.sql

# If rollback needed
psql -h localhost -U s3proxy s3proxy < backup.sql
```

### Configuration

Keep the previous config file when upgrading. New versions only add fields with defaults -- they don't change the meaning of existing fields.

### Binary

The orchestrator is a single static binary. Roll back by deploying the previous version:

```bash
# Debian package
apt install s3-orchestrator=0.7.0

# Docker
docker pull ghcr.io/afreidah/s3-orchestrator:v0.7.0

# Binary
# Replace with the previous version's binary and restart
```

## Breaking Changes Policy

Starting with v1.0.0, the project will follow semantic versioning:

- **Patch** (v1.0.x): bug fixes, no config or API changes
- **Minor** (v1.x.0): new features, new optional config fields, backward-compatible
- **Major** (vX.0.0): breaking config changes, removed fields, incompatible API changes

Pre-v1.0.0 releases may include breaking changes in minor versions. Always check this document before upgrading.
