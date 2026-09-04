---
description: "Structured log/slog conventions: the shared key vocabulary every call site uses, and the sloglint rules that keep it enforced in CI."
---

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
| `logfmt.Err(err)` | `slog.Attr` | Nil-safe `error`-key helper. Returns an empty attr (dropped by slog) when `err` is nil; otherwise equivalent to `slog.String("error", err.Error())`. Use in typed-attr calls (`slog.LogAttrs`, `audit.Log`) or where `err` may be nil — see the error-attribute section below. |
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

## Error attributes

Error values are rendered safely by the runtime, not by call-site
discipline. The slog pipeline is wrapped with `logfmt.ErrAttrHandler`,
which replaces any attribute whose value implements `error` with its
`Error()` string before the inner handler serialises the record. The
wrapper covers raw `"error", err` pairs, `slog.Any("error", err)`, and
group-nested errors. The runtime composes the handler in
`internal/observe/telemetry`, so every production log line goes through
it.

Pick the form that matches the slog API you are already using:

```go
// Key-value API (slog.ErrorContext, slog.WarnContext, etc.) — preferred.
log.ErrorContext(ctx, "replication failed", "error", err)

// Typed-attr API (slog.LogAttrs, audit.Log) — bare pair won't compile
// against ...slog.Attr, so use logfmt.Err.
slog.LogAttrs(ctx, slog.LevelWarn, "replication failed",
    slog.String("backend", name),
    logfmt.Err(err))
```

Default to the bare `"error", err` form: it is the shortest, reads as
plain slog, and the runtime's `logfmt.ErrAttrHandler` renders the error
identically to the helper form. Reach for `logfmt.Err(err)` only when:

- The surrounding call uses the typed-attr API (`slog.LogAttrs`,
  `audit.Log`, anything taking `...slog.Attr`), where bare pair does not
  type-check.
- `err` may legitimately be `nil` and you want the attribute dropped
  rather than logging `error=<nil>`.

`slog.Any("error", err)` is a third option the handler accepts, but it
adds noise without a reason to prefer it over the bare pair; do not use
it for new code.

### What is still rejected

The CI lint rejects these patterns regardless of handler behaviour, so
a future handler swap cannot silently break log structure:

| Pattern | Why |
|---|---|
| `"err", err`, `"e", err` | Non-canonical key — operators grep on `error=`. |
| `"error", err.Error()` | Strips type before the handler sees it, defeating the central rendering rule and the handler-contract tests. |
| `"error", fmt.Sprintf("%v", err)` | Same problem; loses error identity for downstream tooling. |
| `"error_message", err` | Non-canonical key. |

The handler contract has tests in
`internal/observe/logfmt/handler_test.go` that pin the rendering
behaviour end-to-end, so a regression in the runtime composition fails
the suite rather than silently shipping `[object Object]` to operators.

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
| `error` | string | Rendered from any `error` value by the runtime handler; call sites may pass the raw error directly. |

### Banned keys

The CI lint rejects these. Use the canonical name on the right.

| Banned | Use instead |
|---|---|
| `err`, `e` | `error` |
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

**Exception — bootstrap / DI providers.** Startup logs emitted from
`internal/di/*` (provider construction, one-shot wiring warnings) run
before any request or worker tick exists, so there is no span to
correlate to. These call sites use the non-context variants
(`slog.Info`, `slog.Warn`, ...) rather than passing
`context.Background()`. A `Background` context is not "the startup
context" — it carries no trace, no request id, and no cancellation —
so the *Context call would behave identically but mislead future
readers into looking for a meaningful ctx that isn't there. See #831.

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
