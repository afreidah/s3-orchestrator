# Structured Logging Conventions

All operational logs in s3-orchestrator are structured `log/slog` JSON.
The conventions below pin every call site to one vocabulary so log
pipelines (Loki, Nomad UI, Grafana panels) can aggregate without
text-matching message strings.

The conventions are enforced by `golangci-lint`'s `sloglint` rules
(`.golangci.yml`) plus the `internal/observe/logfmt` helper package.

---

## Setup

```go
import "github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
```

`internal/observe/logfmt` exports the small set of helpers every log
call site uses:

| Helper | Returns | Purpose |
|---|---|---|
| `logfmt.Err(err)` | `slog.Attr` | Renders `err.Error()` under the `error` key. Returns an empty attr (dropped by slog) when `err` is nil so callers chain unconditionally. |
| `logfmt.Outcome(value)` | `slog.Attr` | `outcome` attr; pass one of `OutcomeOK`, `OutcomeError`, `OutcomeSkipped`, `OutcomeTimeout`, `OutcomeNotFound`. |
| `logfmt.Component(name)` | `slog.Attr` | `component` attr used at logger construction. |
| `logfmt.RequestIDFromCtx(ctx)` | `slog.Attr` | Pulls the audit request ID from context (empty attr if none). |
| `logfmt.LoggerFromCtx(ctx, base)` | `*slog.Logger` | Returns `base` scoped with the request ID when one is on context. |

---

## Per-component scoped logger

Every long-lived service holds a `*slog.Logger` field. Initialise it
once in the constructor with the canonical component name; all log
calls flow through it.

```go
type PendingReaper struct {
    deps  CleanupOps
    store PendingReaperStore
    log   *slog.Logger
    // ...
}

func NewPendingReaper(deps CleanupOps, store PendingReaperStore) *PendingReaper {
    return &PendingReaper{
        deps:  deps,
        store: store,
        log:   slog.Default().With(logfmt.Component("pending_reaper")),
    }
}

// Use the scoped logger; the message no longer carries the prefix.
r.log.WarnContext(ctx, "HEAD probe failed, leaving intent for next tick",
    "backend", p.BackendName,
    "key", p.ObjectKey,
    "intent_id", p.IntentID,
    logfmt.Err(err),
)
```

Free functions in the same package that legitimately have no receiver
(panic recovery, package-init helpers) call `slog.XContext(ctx, ...)`
directly with an explicit `logfmt.Component(...)` attr.

---

## Why `logfmt.Err`?

slog's JSON handler serialises a complex error type with `encoding/json`,
which produces `{}` for any error whose underlying struct lacks JSON
tags. JS log viewers (Nomad UI, some Loki frontends) render that as
`[object Object]` and the operator cannot grep:

```text
WARN Pending reaper: HEAD failed, leaving intent
  backend=e2 key=unified/backups/... error=[object Object]
```

`logfmt.Err(err)` always produces `slog.String("error", err.Error())`,
which renders the same way in every handler and downstream tool.

**Always pass errors through `logfmt.Err`.** Direct `"error", err`
returns `[object Object]` in production.

---

## Attribute glossary

These keys are canonical across the codebase. The CI lint enforces
the `forbidden-keys` list in `.golangci.yml`; new keys not on this
list are allowed but should be added here when they recur.

| Key | Type | Meaning |
|---|---|---|
| `component` | string | Long-lived service identifier (constant per logger). |
| `request_id` | string | Inbound HTTP request id from `X-Request-Id` or generated. |
| `backend` | string | Backend name (destination, single backend). |
| `src_backend` | string | Source backend (rebalance, replicate, drain). |
| `dst_backend` | string | Destination backend (same operations). |
| `bucket` | string | Virtual S3 bucket. |
| `key` | string | S3 object key (or internal-key form). |
| `prefix` | string | Object-key prefix (lifecycle, list). |
| `path` | string | HTTP path. |
| `method` | string | HTTP method. |
| `status` | int | HTTP status code. |
| `client_addr` | string | Remote client IP/port (`r.RemoteAddr` after trusted-proxy resolution). |
| `upload_id` | string | Multipart upload id. |
| `intent_id` | string | Pending PUT-intent id. |
| `cleanup_id` | int64 | `cleanup_queue.id`. |
| `notification_id` | int64 | `notification_outbox.id`. |
| `size_bytes` | int64 | Object size in bytes. |
| `duration_ms` | int64 | Elapsed time in ms (use `slog.Int64`). |
| `attempts` | int | Retry count. |
| `outcome` | string | Terminal-log status; see helpers above. |
| `error` | string | `err.Error()` only — never the raw error value. |

### Banned keys

The CI lint rejects these. Use the canonical name on the right.

| Banned | Use instead |
|---|---|
| `err`, `e` | `error` (via `logfmt.Err`) |
| `from_backend`, `source_backend` | `src_backend` |
| `to_backend` | `dst_backend` |
| `remote_addr`, `remote` | `client_addr` |

Use `snake_case` for every attribute key. The lint enforces this.

---

## Levels

| Level | Use |
|---|---|
| `Debug` | Verbose state; off by default. |
| `Info` | Lifecycle (startup/shutdown), terminal success of a notable operation, audit entries. |
| `Warn` | Recoverable failure — caller proceeds, operator should know (failover, degraded mode, retry-able errors). |
| `Error` | Unrecoverable failure of an operation — request fails, background tick aborts, integrity violation. |

Use `*Context` variants (`WarnContext`, `ErrorContext`, etc.) so the
trace handler injects `trace_id`/`span_id` when a span is active. The
`sloglint` config enforces `context: all`.

---

## Outcome attribute

Terminal log lines (loop summaries, request completions) carry an
`outcome` attr so dashboards can aggregate without parsing message
text:

```go
r.log.InfoContext(ctx, "rebalance pass complete",
    logfmt.Outcome(logfmt.OutcomeOK),
    "moved", n,
    "duration_ms", elapsed.Milliseconds(),
)
```

Use the `Outcome*` constants. New outcomes should be added to the
constant set in `internal/observe/logfmt/logfmt.go` and to the table
above.

---

## Audit logging is separate

`internal/observe/audit` emits its own structured entries marked with
`"audit": true` for security-relevant operations. Audit entries always
carry `request_id` and use dotted event names (`s3.PutObject`,
`storage.DeleteObject`, `cleanup_queue.processed`). The conventions
documented here apply to **operational** logs, not audit entries — see
`README.md` § Audit Logging for the audit contract.

---

## Adding a new component

1. Pick a `snake_case` component name (e.g. `vault_token_renewer`).
2. Add a `log *slog.Logger` field to the struct.
3. In the constructor, set `log: slog.Default().With(logfmt.Component("name"))`.
4. Use `r.log.XContext(ctx, ...)` everywhere in the type's methods.
5. Use the canonical attribute keys from the glossary; if a new key
   is needed, add it to the table above in the same PR.
