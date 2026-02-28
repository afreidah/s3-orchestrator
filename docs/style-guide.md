# S3 Orchestrator Coding Style Guide

**Author:** Alex Freidah

---

## Table of Contents

- [Core Principles](#core-principles)
- [Comment Types and Spacing](#comment-types-and-spacing)
- [File Headers](#file-headers)
- [Go Conventions](#go-conventions)
- [Error Handling](#error-handling)
- [Logging and Audit](#logging-and-audit)
- [Testing](#testing)
- [Code Style](#code-style)

---

## Core Principles

- **ASCII-only characters** - Never use Unicode em-dashes, en-dashes, or box-drawing characters
- **Dashes, not equals** - Always use `-` for dividers, never `=`
- **Box comment spacing** - ALL box comments (79-char file headers and 73-char sections) ALWAYS have a blank line after
- **Professional tone** - No personal references, no numbered lists, no casual language
- **Self-documenting** - Code explains *why*, not just *what*
- **Streaming over buffering** - Use `io.Pipe` and streaming patterns for object data; never buffer entire objects in memory
- **Context propagation** - Pass `context.Context` through all function chains for cancellation, tracing, and audit correlation

---

## Comment Types and Spacing

### File Header (79 characters)

**Format:**
```go
// -------------------------------------------------------------------------------
// Title of File or Component
//
// Author: Alex Freidah
//
// 2-4 sentence description of the file's purpose, scope, and key functionality.
// Include architecture notes, design decisions, or important context that helps
// readers understand the overall purpose.
// -------------------------------------------------------------------------------

package mypackage
```

**Spacing Rules:**
- Blank line after title
- Blank line after metadata
- Blank line before closing divider
- **Blank line after closing divider** - always separate box from code

### Major Section Box (73 characters)

**Format:**
```go
// -------------------------------------------------------------------------
// SECTION NAME
// -------------------------------------------------------------------------

func doSomething() {
    // ...
}
```

**Spacing Rules:**
- Use ALL CAPS for section name
- **Blank line AFTER closing divider** - separates section from code
- Used for major logical divisions (e.g., PUBLIC API, INTERNALS, TYPES)

### Single-Line Comments

Standard Go comments placed directly above the code they describe:

```go
// Parse request path
bucket, key, ok := parsePath(r.URL.Path)
if !ok {
    return errInvalidPath
}
```

- **NO blank line before code** - placed directly above the block
- Use lowercase or sentence case
- Used for minor divisions or labels within functions

### Inline Comments

```go
m.usage.Record(backendName, 2, movedSize, 0) // Get + Delete, egress
```

- Use sparingly
- Explain *why*, not *what*
- Keep concise (< 50 characters)

---

## Comment Type Decision Tree

```
Is this a file header?
  YES -> Use 79-char divider, blank line AFTER

Is this a major section (types, public API, internals)?
  YES -> Use 73-char box, blank line AFTER

Is this a minor division or label within a function?
  YES -> Use a standard single-line comment, NO blank line before code

Is this explaining a specific line?
  YES -> Use inline comment
```

**Key Rule:** ALL box comments (79-char and 73-char) have a blank line after. Single-line comments have no extra spacing.

---

## File Headers

Every `.go` file starts with a 79-char header block:

```go
// -------------------------------------------------------------------------------
// Object Operations - PUT, GET, HEAD, DELETE, COPY
//
// Author: Alex Freidah
//
// Object-level CRUD operations on the BackendManager. Handles backend selection
// via routing strategy, read failover across replicas, broadcast reads during
// degraded mode, and usage limit enforcement on reads and writes.
// -------------------------------------------------------------------------------

package storage
```

**Rules:**
- Use `//` comments (not `/* */` blocks)
- Title line describes the file's scope, not the package
- Description covers purpose, key behaviors, and dependencies
- The `package` declaration follows immediately after the closing divider + blank line

---

## Go Conventions

### Indentation

- **1 tab** - Go standard (`gofmt` enforced)

### Imports

Group imports in three blocks separated by blank lines:

```go
import (
    "context"
    "fmt"
    "time"

    "github.com/afreidah/s3-orchestrator/internal/audit"
    "github.com/afreidah/s3-orchestrator/internal/telemetry"

    "go.opentelemetry.io/otel/attribute"
    "github.com/prometheus/client_golang/prometheus"
)
```

Order: stdlib, internal packages, external packages.

### Naming

- **Exported types** get standard Go doc comments placed directly above the declaration
- **Constants** grouped by concern with `const` blocks, named in `CamelCase`
- **Sentinel errors** use `Err` prefix: `ErrObjectNotFound`, `ErrDBUnavailable`
- **S3Error type** wraps HTTP status + S3 error code for typed error responses
- **Operation names** are consistent PascalCase strings matching S3 API names: `"PutObject"`, `"GetObject"`, `"CreateMultipartUpload"`

### Struct Organization

Group related fields with inline comments explaining non-obvious fields:

```go
type BackendManager struct {
    backends        map[string]ObjectBackend
    store           MetadataStore
    order           []string
    cache           *LocationCache
    backendTimeout  time.Duration
    usage           *UsageTracker
    metrics         *MetricsCollector
    dashboard       *DashboardAggregator
    routingStrategy string
    rebalanceCfg    atomic.Pointer[config.RebalanceConfig]
    replicationCfg  atomic.Pointer[config.ReplicationConfig]
}
```

### Concurrency Patterns

- **Atomic pointers** for hot-reloadable config (`atomic.Pointer[T]`)
- **Atomic counters** for usage tracking, flushed periodically to the database
- **Context-scoped timeouts** via helper methods (`m.withTimeout(ctx)`) for backend calls
- **Graceful shutdown** via `context.WithCancel` + signal handling

---

## Error Handling

### S3 Errors

Use the `S3Error` type for errors that map to S3 HTTP responses:

```go
var ErrObjectNotFound = &S3Error{
    StatusCode: 404,
    Code:       "NoSuchKey",
    Message:    "The specified key does not exist",
}
```

Handlers use `writeStorageError` to convert `S3Error` instances into XML responses. Untyped errors fall back to `502 InternalError`.

### Error Classification

The `classifyWriteError` helper distinguishes database unavailability (circuit breaker open) from other failures, returning appropriate S3 error codes:

- `ErrDBUnavailable` -> `503 ServiceUnavailable`
- Other errors -> `502 InternalError`

### Background Operation Errors

Background workers (rebalancer, replicator, cleanup) log errors and continue rather than crashing. Individual item failures are logged with `slog.Warn` and skipped; the batch proceeds with remaining items.

---

## Logging and Audit

### Structured Logging

All logging uses `log/slog` with JSON output to stdout. The logger is initialized in `main.go`:

```go
slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
    Level: slog.LevelInfo,
})))
```

### Audit Logging

The `internal/audit` package provides structured audit entries for security-relevant operations. Audit entries are distinguished by the `"audit": true` field.

**S3 API requests** produce two correlated audit entries sharing the same `request_id`:

- HTTP layer (`s3.PutObject`, `s3.GetObject`, etc.) - method, path, bucket, status, duration
- Storage layer (`storage.PutObject`, `storage.GetObject`, etc.) - key, backend, size

**Internal operations** generate their own correlation IDs:

```go
ctx = audit.WithRequestID(ctx, audit.NewID())
audit.Log(ctx, "rebalance.start",
    slog.String("strategy", cfg.Strategy),
    slog.Int("batch_size", cfg.BatchSize),
)
```

**Rules:**
- Use `audit.Log` for operations that change state or serve data (not for debug/health checks)
- Always pass context so the request ID propagates
- Event names use dotted notation: `"s3.PutObject"`, `"storage.DeleteObject"`, `"rebalance.move"`
- Include enough attributes to reconstruct the operation without reading other log lines

### Request ID Propagation

Request IDs flow through context via `audit.WithRequestID` / `audit.RequestID`:

- S3 API requests: extracted from `X-Request-Id` header or generated, set on context before auth
- Internal operations: generated at the start of each background task tick or batch run
- The ID is also set as a `s3proxy.request_id` attribute on OpenTelemetry spans

### Log Levels

| Level | Use |
|-------|-----|
| `slog.Info` | Startup, shutdown, config reload, audit entries |
| `slog.Warn` | Recoverable failures (failover to replica, orphan cleanup, non-critical background errors) |
| `slog.Error` | Unrecoverable failures (startup errors, DB connection loss, background task crashes) |

---

## Testing

### Unit Tests

- Test files live alongside the code they test: `server_test.go`, `manager_objects_test.go`
- Use table-driven tests for operations with multiple input/output combinations
- Mock interfaces (`MetadataStore`, `ObjectBackend`) live in `internal/testutil/`
- Test names follow `TestFunctionName_Scenario` convention

### Integration Tests

- Located in `internal/integration/`
- Gated behind the `integration` build tag
- Run in-process with real MinIO and PostgreSQL containers
- Cover end-to-end flows: CRUD, quota enforcement, multipart, replication, circuit breaker

### Test Patterns

- **Mock stores** implement the full `MetadataStore` interface with configurable responses
- **FailableStore** wraps a mock to inject errors for circuit breaker testing
- Test assertions use standard `testing.T` methods, not external assertion libraries

### Coverage Exclusions

Go has no built-in coverage ignore directive. Use the Codecov `// codecov:ignore` inline comment to exclude untestable code (process entry points, `os.Exit` wrappers) from coverage reports. Always include a reason after the directive:

```go
func runValidate() { // codecov:ignore -- os.Exit wrapper, logic tested via validateConfig
    // ...
    os.Exit(1)
}
```

Use this sparingly and only for code that genuinely cannot be unit tested:
- `main()` and subcommand entry points that call `os.Exit`
- Trivial wrappers with no branching logic
- Signal handlers and process lifecycle glue

Extract testable logic into separate functions that return errors instead of calling `os.Exit` directly.

---

## Code Style

### Character Rules

**ALWAYS USE:**
- ASCII dash: `-` (hyphen-minus, U+002D)
- Standard ASCII characters only

**NEVER USE:**
- Unicode em-dash (U+2014)
- Unicode en-dash (U+2013)
- Unicode box-drawing (U+2500)
- Equals signs for dividers

### Professional Tone

Avoid:
- Personal references: "Let me show you...", "We need to..."
- Numbered lists in comments: "1. First do this", "2. Then do that"
- Conversational tone: "Now we're going to..."
- Future tense: "This will create...", "We'll configure..."

Use:
- Present tense: "Creates", "Configures", "Manages"
- Declarative statements: "Service runs on port 9000"
- Technical precision: "Uses SigV4 for request authentication"
- Impersonal voice: "The manager selects...", "The circuit breaker wraps..."

---

## Quick Reference

| Comment Type | Length | Spacing After | Use Case |
|-------------|--------|---------------|----------|
| File header | 79 chars | 1 blank line | Top of every `.go` file |
| Major section | 73 chars | 1 blank line | Major divisions (types, API, internals) |
| Single-line comment | Variable | None | Minor divisions within functions |
| Inline | Brief | N/A | Specific line explanation |

---

## Examples

### Good

```go
// -------------------------------------------------------------------------------
// Replicator - Background Replica Creation Worker
//
// Author: Alex Freidah
//
// Creates additional copies of under-replicated objects across backends. Objects
// are written to one backend on PUT; this worker asynchronously ensures each
// object reaches the configured replication factor. Uses conditional DB inserts
// to safely handle concurrent overwrites and deletes.
// -------------------------------------------------------------------------------

package storage

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Replicate finds under-replicated objects and creates additional copies to
// reach the target replication factor. Returns the number of copies created.
func (m *BackendManager) Replicate(ctx context.Context, cfg config.ReplicationConfig) (int, error) {
    start := time.Now()
    ctx = audit.WithRequestID(ctx, audit.NewID())

    // Find under-replicated objects
    locations, err := m.store.GetUnderReplicatedObjects(ctx, cfg.Factor, cfg.BatchSize)
    if err != nil {
        return 0, fmt.Errorf("failed to query under-replicated objects: %w", err)
    }
    // ...
}
```

### Bad

```go
// ==================================
// Replicator
//
// This module will handle replication for the user.
// Here's how it works:
// 1. First we find objects that need replication
// 2. Then we copy them to other backends
// 3. Finally we record the results
// ==================================

package storage

// Let's create the replicate function
func (m *BackendManager) Replicate(ctx context.Context, cfg config.ReplicationConfig) (int, error) {
    // get the start time
    start := time.Now()
    // ...
}
```

---

**Remember:** Comments should explain *why* decisions were made, not *what* the code does. The code itself should be clear enough to understand *what* it does.
