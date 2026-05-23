**Author:** Alex Freidah

---

## Table of Contents

- [Core Principles](#core-principles)
- [Comment Types and Spacing](#comment-types-and-spacing)
- [File Headers](#file-headers)
- [Go Conventions](#go-conventions)
- [Project Structure and Layers](#project-structure-and-layers)
- [Dependency Injection](#dependency-injection)
- [Adding a New Component](#adding-a-new-component)
- [Error Handling](#error-handling)
- [Logging and Audit](#logging-and-audit)
- [Tracing](#tracing)
- [Metrics](#metrics)
- [Testing](#testing)
- [Code Style](#code-style)
- [Versioning](#versioning)
- [Documentation Updates](#documentation-updates)
- [Branch Naming](#branch-naming)

---

## Core Principles

- **ASCII-only characters** - Never use Unicode em-dashes, en-dashes, or box-drawing characters
- **Dashes, not equals** - Always use `-` for dividers, never `=`
- **Box comment spacing** - ALL box comments (79-char file headers and 73-char sections) ALWAYS have a blank line after
- **Professional tone** - No personal references, no numbered lists, no casual language
- **Self-documenting** - Code explains *why*, not just *what*
- **Streaming over buffering** - Use `io.Pipe` and streaming patterns for object data; never buffer entire objects in memory
- **Buffer pooling** - Use `bufpool.Copy` instead of `io.Copy` for all streaming I/O to reuse buffers and reduce GC pressure
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

package proxy
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

    "github.com/afreidah/s3-orchestrator/internal/observe/audit"
    "github.com/afreidah/s3-orchestrator/internal/observe/telemetry"

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
    *backendCore                                     // embeds backend fleet, admission, drain, metrics
    stores          core.MetadataStore               // wide composite metadata-store contract
    pendingEnabled  bool                             // mirrors cfg.PendingEnabled for write-path branches
    routingStrategy config.RoutingStrategy           // RoutingPack or RoutingSpread
    rebalanceCfg    syncutil.AtomicConfig[config.RebalanceConfig]
    replicationCfg  syncutil.AtomicConfig[config.ReplicationConfig]
    Rebalancer      *worker.Rebalancer
    Replicator      *worker.Replicator
    Scrubber        *worker.Scrubber
}
```

### Interface Design: Consumer-Declared Interfaces

This codebase follows the Go-idiomatic "accept interfaces, return structs" pattern: **producer packages export concrete `*Type` values with no producer-side interface**, and **each consumer declares its own narrow interface** listing only the methods it actually calls. The concrete type satisfies every consumer's local interface because Go interfaces are structurally typed.

Applied across `internal/store` (the per-role store interfaces consumed at use sites), `internal/worker` (`Ops`, `CleanupOps`, `ScrubberOps` in `ops_runtime.go`), and the `internal/proxy/*` subpackages (`MultipartCore`/`MultipartCoordinator`, `ObjectCore`/`ObjectCoordinator`, `WritepathCore`).

**Rationale:**
- A consumer's dependency footprint is documented in its own source file.
- Adding a method to a producer type never bloats existing consumer mocks.
- Tests can mock at the granularity of what is used (typically 4–10 methods), not the full producer surface.
- Aligns with Rob Pike's "accept interfaces, return structs" guideline.

**Trade-offs:**
- Each consumer declares its own small interface (extra text, but localized).
- The composition layer (the root `proxy` package, `internal/di`) still holds concrete types — it owns construction and is the seam where interfaces meet implementations.

**Where the interfaces live:**

| Location | Holds |
|---|---|
| `internal/proxy/<consumer>/consumer_interfaces.go` | All narrow interfaces this consumer declares against other proxy subpackages and external clients |
| `internal/worker/ops_runtime.go` | Worker-side `Ops` / `CleanupOps` / `ScrubberOps` interfaces against the proxy infrastructure |
| `internal/store/core/interfaces.go` | Per-role narrow store interfaces (consumers compose them when they need to declare a minimal store dependency) |

**Naming convention:** `<Consumer><Provider>` — e.g. the multipart subpackage's view of `*infra.Core` is `MultipartCore`; the object subpackage's view of `*writepath.Coordinator` is `ObjectCoordinator`. The prefix names the consumer, the suffix names the producer concept.

**Constructor shape:** consumers take the interfaces, not concrete pointers. Composition-layer code (the root proxy package, DI providers) passes the concrete `*infra.Core`, `*writepath.Coordinator`, `*object.Manager`, etc., and the concrete types satisfy the interfaces implicitly.

```go
// internal/proxy/multipart/consumer_interfaces.go
type MultipartCore interface {
    GetBackend(name string) (backend.ObjectBackend, error)
    Usage() *counter.UsageTracker
    WithTimeout(ctx context.Context) (context.Context, context.CancelFunc)
    // ... only what multipart calls
}

// internal/proxy/multipart/manager.go
type Manager struct {
    core  MultipartCore        // not *infra.Core
    coord MultipartCoordinator // not *writepath.Coordinator
    // ...
}

func New(core MultipartCore, coord MultipartCoordinator, ...) *Manager { ... }
```

**Mocking:** generated mocks are not produced eagerly. When a test actually needs to mock a consumer-declared interface, add a `//go:generate mockgen -source=consumer_interfaces.go -destination=mock/<file>.go -package=<pkg>mock` directive at the top of the consumer interface file and run `make generate`. Until a mock is needed, the interface declaration alone documents the dependency surface — generating unused mocks is busywork.

**When NOT to declare a narrow interface (#918).** A consumer-side interface earns its keep when at least one of these is true:

1. **Multiple implementations actually exist** (a real polymorphism point, e.g. `keySource` over S3 iter + DB iter).
2. **A test fake genuinely benefits from the seam** — a hand-rolled fake or `gomock`-generated mock that lets tests exercise the consumer without standing up the real producer (e.g. `worker.Ops` mocked across ~60 test sites; `admin.ReplicatorOps`/`OverReplicationOps`/`ScrubberOps`/`Reconciler` whose fakes drive admin handler branches).
3. **An import cycle would otherwise form** (e.g. `readpath.LocationCache` — without the interface, `readpath` would have to import `object` which already imports `readpath`).
4. **The interface models a real domain boundary** between subsystems (`drain.Core`, `MultipartCore`, `ObjectCore`, `WritepathCore`).

If none apply — single impl, single consumer, no test fake, no cycle, no boundary — pass the concrete `*Type` directly. Examples cut under #918: `readpath.ObjectLocationLister`, `multipart.MultipartCoordinator`, `multipart.StaleCleaner`, `accounting.UsageTracker`, the `worker.RebalancerStore`/`ReplicatorStore`/`OverReplicationStore`/`CleanupWorkerStore`/`ScrubberStore`/`PendingReaperStore` store-role wrappers (workers now take `core.MetadataStore` directly).

**Producer-side interfaces are an anti-pattern.** `*infra.Core`, `*writepath.Coordinator`, `*object.Manager`, `*multipart.Manager` are exported as concrete pointer types with no sibling `Core`/`Coordinator`/`Manager` interface that mirrors their public surface. Producer-side interfaces force every consumer to mock the full producer API, which is exactly what this pattern is built to avoid.

**Logger is not a behavior dependency.** Never include `Log() *slog.Logger` in a consumer-declared interface. The logger is observability infrastructure: it has no return value the consumer depends on, and the per-component scope is a property of the consumer itself, not of the producer. Components build their own `log *slog.Logger` field in the constructor body via `slog.Default().With(logfmt.Component("<slug>"))` (see Logging and Audit), so each subsystem owns its component name and tests do not need to thread a logger through dependency interfaces.

**Single-method interface names follow the Go `-er` convention.** A single-method interface is named after its method, with the verb in agent-noun form: `Read` → `Reader`, `Close` → `Closer`, `Stringer` for `String()`. Names ending in `-Ops`, `-Store`, `-ing`, or other shapes that do not describe an actor get flagged by `golangci-lint` / sonar (rule S8196) and should be renamed:

| Method | Good | Bad |
|---|---|---|
| `Read(p []byte) (int, error)` | `Reader` | `IOOps` |
| `Close() error` | `Closer` | `Closeable` |
| `GetAllObjectLocations(...)` | `ObjectLocationLister` | `LocationStore` |
| `CleanupStaleMultipartUploads(...)` | `staleMultipartCleaner` | `multipartCleanupOps` |
| `UpdateUsageLimits(...)` | `usageLimitsApplier` | `usageLimitsHook` |
| `UpdateQuotaMetrics(...)` | `quotaMetricsRefresher` | `metricsHook` |

For interfaces that exist to *provide* a value (typical "Acct" / "Stores" / "Config" getters), name the interface after the returned type plus `Provider` or `Source` — `RecorderProvider` for `Acct() *Recorder`, `ConfigSource` for `Config() *Config`. The `Provider` / `Source` suffix is also an agent noun and satisfies the rule.

Multi-method interfaces are exempt: `core.MetadataStore`, `worker.Ops`, `ObjectCore`, `MultipartCoordinator` describe a role (or a composite of sub-roles), not a single action, so the `-er` form does not apply.

### Per-Backend Accounting: Use the Recorder

Per-backend usage and per-operation metric accounting flow through one shared helper: `internal/proxy/accounting.Recorder`. Every consumer holds the same `*accounting.Recorder` (resolved via `core.Acct()`) and calls the named methods rather than the underlying `Usage().Record(...)` or `RecordOperation(...)`:

| Intent | Use | Avoid |
|---|---|---|
| Backend call attempted, no bytes (success or failure) | `acct.APICall(backend)` | `Usage().Record(b, 1, 0, 0)` |
| N backend calls (paginated lists, multipart bulks) | `acct.APICalls(backend, n)` | `Usage().Record(b, n, 0, 0)` |
| Successful GET-like (bytes left the backend) | `acct.Egress(backend, size)` | `Usage().Record(b, 1, size, 0)` |
| Successful PUT-like (bytes arrived at the backend) | `acct.Ingress(backend, size)` | `Usage().Record(b, 1, 0, size)` |
| Per-operation Prometheus histogram | `acct.Operation(op, backend, start, err)` | `RecordOperation(op, b, start, err)` |
| Common combo: Operation + Ingress | `acct.PutSuccess(op, backend, size, start)` | both above, two lines |
| Common combo: Operation + Egress | `acct.GetSuccess(op, backend, size, start)` | both above, two lines |
| The call reached the backend and failed | `acct.OperationFailed(op, backend, start, err)` | Operation + APICall, two lines |

The cardinal rule "every backend call costs one API-call charge regardless of outcome" lives in the method bodies, not in repeated inline comments at every call site. The argument-order risk of the bare `Record(b, 1, 0, size)` vs `Record(b, 1, size, 0)` form is eliminated because ingress vs egress is named in the method.

Exceptions — *not* every Usage call goes through Recorder:
- `BackendManager.RecordUsage(b, apiCalls, egress, ingress)` is the admin escape hatch that takes an arbitrary tuple. It bypasses the per-attempt rule on purpose and uses `Usage().Record` directly.
- Tests that exercise the tracker's own semantics (`manager_usage_test.go`) call `Usage().Record` directly — they're verifying the tracker, not the accounting rule.

### Variable Naming

Avoid shadowing package imports with local variable names. When a function receives or creates a `backend.ObjectBackend`, name the variable `be`, not `backend`:

```go
// Good
be, err := mp.GetBackend(mu.BackendName)
etag, err := be.PutObject(ctx, key, body, size, contentType, nil)

// Bad — shadows the backend package import
backend, err := mp.GetBackend(mu.BackendName)
```

### Typed Constants

Use typed string constants for values compared in multiple places. Bare string comparisons (e.g., `routingStrategy == "spread"`) are error-prone:

```go
type RoutingStrategy string

const (
    RoutingPack   RoutingStrategy = "pack"
    RoutingSpread RoutingStrategy = "spread"
)
```

### No Empty Cleanup Funcs

Never return `func() {}` to satisfy a `(T, cleanup func(), error)` signature when the "no-cleanup" branch has nothing to do. SonarQube flags empty function literals (rule `go:S1186`) and the empty literal hides whether the branch *meant* to be empty or someone forgot to wire the cleanup.

**Why:** Past SonarQube findings on PRs that returned `func() {}` as the no-op cleanup arm of an in-memory-vs-tempfile sink. The repo has hit the same lint multiple times across different abstractions.

**How to apply:** Attach the cleanup as a method on the returned type so the caller writes `defer x.Cleanup()` once and the method internally branches on which underlying resource (if any) actually needs releasing. The lint never sees an empty literal, and the caller code stays identical across branches.

```go
// Bad — empty literal trips S1186 on the in-memory branch.
func newSink(size int64) (*sink, func(), error) {
    if size <= memoryLimit {
        return &sink{buf: &bytes.Buffer{}}, func() {}, nil
    }
    f, _ := os.CreateTemp("", "x-*")
    return &sink{file: f}, func() { _ = f.Close() }, nil
}

// Good — cleanup is a method that no-ops on the branch with no resource.
func newSink(size int64) (*sink, error) {
    if size <= memoryLimit {
        return &sink{buf: &bytes.Buffer{}}, nil
    }
    f, err := os.CreateTemp("", "x-*")
    if err != nil {
        return nil, err
    }
    return &sink{file: f}, nil
}

func (s *sink) Cleanup() {
    if s.file != nil {
        _ = s.file.Close()
    }
}
```

When the cleanup truly must be a callback (e.g., interface contract), wrap it in `sync.OnceFunc` and have it do something meaningful (clear a pointer, decrement a counter) so the body is never literally empty.

### Concurrency Patterns

- **`syncutil.AtomicConfig[T]`** for hot-reloadable config (wraps `atomic.Pointer[T]` with `Store`/`Load`)
- **`syncutil.TTLCache[K,V]`** for generic TTL-based caches with background eviction
- **Atomic counters** for usage tracking, flushed periodically to the database
- **Context-scoped timeouts** via helper methods (`m.WithTimeout(ctx)`) for backend calls
- **Graceful shutdown** via `context.WithCancel` + signal handling

---

## Project Structure and Layers

The codebase splits along strict layers. New code goes in the layer whose
responsibility matches; an outer layer must not import a more outer layer, and
inner layers must never import the transport packages.

```
cmd/s3-orchestrator/         # Binary entry point: flag parsing + os.Exit wrapper
internal/
  cli/                       # Subcommand dispatch (admin, init, sync, serve, validate)
    serve/                   # Server bootstrap: builds DI injector, starts HTTP listener
    adminctl/                # `s3-orchestrator admin ...` subcommands (HTTP client to admin API)
    initcmd/                 # `s3-orchestrator init` interactive config writer
    synccmd/                 # `s3-orchestrator sync` bucket reconcile entry point
  config/                    # YAML + env config: types, defaults, validators
  transport/                 # HTTP layer (no business logic)
    s3api/                   # S3-compatible XML/REST handlers
    admin/                   # Admin API handlers
    ui/                      # Dashboard handlers + templates
    auth/                    # SigV4 verification + bucket auth
    httputil/                # Cross-cutting HTTP helpers (trusted proxies, login throttle, cert reload)
  proxy/                     # Manager layer: routes requests, owns workers, glues store + backend
    manager/                 # BackendManager + role-narrow sub-managers
    metrics/                 # Manager-level metric collector
    drain/                   # Backend drain coordinator
    dashboard/               # Dashboard read aggregator
  worker/                    # Background services: replicator, rebalancer, cleanup, scrubber, reaper
  store/                     # Metadata persistence
    core/                    # Engine-agnostic orchestration (TxAdapter, Runner, business rules)
    postgres/                # Postgres engine adapter (sqlc-generated under sqlc/); CB protection lives in the DBTX wrapper
    sqlite/                  # SQLite engine adapter; CB protection lives in the DB wrapper
  backend/                   # S3-compatible client interface + per-provider adapters
  encryption/                # Envelope encryption: chunked AES-GCM, key providers (config/file/Vault)
  observe/
    audit/                   # Request-id context plumbing + structured audit log helper
    telemetry/               # Prometheus metrics + OTel span helpers
    event/                   # Notification event types (CloudEvents)
  notify/                    # Webhook delivery worker (consumes notification_outbox)
  counter/                   # Per-backend usage counters (local + Redis backends)
  cache/                     # In-memory read-side cache
  breaker/                   # Generic three-state CircuitBreaker (used for DB and backends)
  lifecycle/                 # Service supervisor: restart-on-panic, graceful Stop
  di/                        # samber/do/v2 wiring point (NewInjector + every Provide func)
  util/
    bufpool/                 # Pooled byte buffers for streaming I/O
    syncutil/                # AtomicConfig, TTLCache primitives
    workerpool/              # Bounded-concurrency worker pool
  integration/               # End-to-end integration tests against MinIO + Postgres testcontainers
```

### Layer Responsibilities

| Layer | Imports | Responsibility |
|---|---|---|
| `cmd/`, `cli/` | Everything | Wire flags, build the DI injector, invoke top-level services |
| `transport/` | `proxy`, `auth`, `observe` | Decode HTTP, authenticate, dispatch to manager, encode S3-XML response |
| `proxy/` | `worker`, `store`, `backend`, `observe`, `counter` | Routing strategy, broadcast reads, location cache, worker hosts |
| `worker/` | `store`, `backend`, `observe` | Background services driven by the lifecycle manager |
| `store/` | nothing app-specific | Metadata persistence; engine-agnostic orchestration in `core/`, engines under `postgres/` and `sqlite/` |
| `backend/` | nothing app-specific | Per-provider S3 client wrappers behind one `ObjectBackend` interface |
| `observe/` | nothing app-specific | Logging, tracing, metrics, audit |
| `di/` | All public surfaces | Single wiring point that registers every provider |

Inner layers must not import outer layers (no `store` importing `transport`).
The compiler does not enforce this, so reviewers must - it is the rule that
keeps the code testable and the dependency graph acyclic.

### Layered Read of a Single PUT

```
client -> transport/s3api  (parse, auth)
       -> proxy/manager    (routing, quota check, location cache)
       -> backend          (S3 PUT)
       -> store/core       (RecordObject in a tx)
       -> response back through the layers
```

The arrow direction is strict: `store/core` never calls back into `proxy`.

---

## Dependency Injection

DI uses [samber/do/v2](https://github.com/samber/do/v2) and is centralised
in `internal/di/di.go`. Every external dependency, store role, worker, and
top-level handler has a `Provide<Name>` function registered there. Callers
resolve dependencies via `do.Invoke[Foo](inj)` at the moment they are
needed; nothing is constructed until it is asked for.

### Two Foundational Rules

1. **Lazy providers**: a provider returns the dependency it builds; it
   never has side effects beyond construction. The injector calls it on
   the first `do.Invoke` and memoises the result for subsequent calls.
2. **Wide composite store, consumer-defined narrow interfaces at the
   boundary**: consumers receive the wide `core.MetadataStore` (a
   composite that embeds every per-role interface) and either typehint-
   narrow it at the field declaration or declare a local interface
   listing only the methods they call. Producer-side narrow roles
   (`ObjectStore`, `QuotaStore`, etc.) exist as embedding building
   blocks for `MetadataStore`; consumer code does not import them
   directly.

### Provider Pattern

Every provider follows the same shape:

```go
// ProvideRebalancer constructs the rebalancer worker.
func ProvideRebalancer(i do.Injector) (*worker.Rebalancer, error) {
    mgr, err := do.Invoke[*proxy.BackendManager](i)
    if err != nil {
        return nil, err
    }
    stores, err := do.Invoke[core.MetadataStore](i)
    if err != nil {
        return nil, err
    }
    rb := worker.NewRebalancer(mgr, stores)
    mgr.Rebalancer = rb
    return rb, nil
}
```

Providers take a `do.Injector`, resolve their own dependencies through it,
and return either the value or an error. They never panic, never log, and
never spawn goroutines. CB protection lives inside each driver's DBTX/DB
chokepoint, so providers do not wrap stores with per-role decorators.

### Wiring Point

The `internal/di` package is the single wiring point. It is the **only**
package that imports the concrete engine packages (`store/postgres`,
`store/sqlite`, each backend driver, etc.). Providers are split across
focused files by concern:

- `injector.go` - `NewInjector` and the top-level registration order
- `store.go`    - database, role aliases, instance ID, metrics deps
- `backend.go`  - S3 backends, breaker registry, backend manager, and the
                  optional providers feeding it (encryption, Redis counters,
                  object cache)
- `workers.go`  - background worker providers
- `lifecycle.go` - lifecycle manager and per-mode service registration
- `transport.go` - S3, admin, UI, notifier, rate limiter, login throttle
- `optional.go`  - `invokeOptional[T]` and `resolveOptional*` helpers
- `services.go`  - lifecycle Runner wrappers (locked-ticker background jobs)

New providers go in the file matching their concern, with `73-char`
section dividers when a single file hosts multiple groups.

### Consuming a Service

The transport layer and CLI commands resolve services from the injector:

```go
func (h *Handler) handleAdminReplicate(w http.ResponseWriter, r *http.Request) {
    repl, err := do.Invoke[*worker.Replicator](h.inj)
    if err != nil {
        writeError(w, http.StatusInternalServerError, err)
        return
    }
    n, err := repl.Replicate(r.Context(), repl.Config())
    // ...
}
```

The handler holds `do.Injector`, not the resolved service - that keeps
the handler decoupled from concrete provider behaviour and lets tests
substitute fakes by registering a different provider.

### Adding a New Provider

1. Add a `Provide<Name>` function in the appropriate section of
   `internal/di/di.go`.
2. Inside the function, call `do.Invoke[Dep](i)` for each dependency.
3. Construct the new component and return `(value, nil)`.
4. If the new component is consumed somewhere, the consumer should call
   `do.Invoke[Name](inj)` rather than holding the value as a field.

### Optional Providers

Some providers register only when a feature is enabled (encryption,
Redis counters, UI, notifications) or only in specific run modes
(reconciler is worker/all-only). Consumers that may run in either mode
use the `invokeOptional[T]` helper, which returns the zero value of `T`
when the service is not registered:

```go
return admin.New(&admin.Deps{
    // ... required deps from resolveAdminHandlerRequiredDeps
    Reconciler: invokeOptional[*worker.Reconciler](i),
})
```

Required providers should still bail with the `do.Invoke` error so a
genuinely missing dependency surfaces at boot, not at first use.

### Anti-Patterns

- Constructing a real dependency in a constructor that already has a
  provider. Always go through `do.Invoke`.
- Passing a `*slog.Logger` through the constructor argument list.
  Long-lived components hold a private `log *slog.Logger` field set
  inside the constructor body via
  `slog.Default().With(logfmt.Component("name"))`. Metrics use the
  `observe/telemetry` package-level vars directly. Free helper
  functions call `slog.XContext(ctx, ...)` directly. See
  [`docs/contributing/logging.md`](contributing/logging.md).
- Storing the injector as a field on a non-handler struct. Workers,
  managers, and stores receive their dependencies as constructor args
  resolved in the provider; only handlers (HTTP/CLI entry points) carry
  the injector itself.

### Constructor Patterns

Constructors at the DI/wiring boundary panic on missing required
dependencies via `internal/util/must`. The boundary is the set of
constructors a DI provider or a test fixture builds directly:
`proxy.NewBackendManager`, every `worker.New*`, every
`transport/{s3api,admin,ui}.New*`, and the `proxy/{drain,readpath,
multipart,object,writepath}` package constructors that take a `*Deps`
or interface bag. Internal helpers that are only called from one
already-validated site (`accounting.New`, `metrics.New`,
`dashboard.New`, `infra.New`) trust their caller and skip the panic.

```go
func New(d *Deps) *Handler {
    must.NotNil("d", d)
    must.NotNil("d.BackendOps", d.BackendOps)
    must.NotNil("d.Objects", d.Objects)
    ...
    return &Handler{...}
}
```

Why panic rather than return `(*T, error)`: a wiring bug is a programmer
error, not an operator-recoverable condition. The panic surfaces at boot
with a clear "required dependency X is nil" message; the alternative
defers to the NPE that fires several call frames deep on the first
request. Operator-facing config invariants (negative timeouts, missing
required strings) belong in `internal/config.SetDefaultsAndValidate`
which returns errors the loader can format and report.

**Fallible-constructor exemption.** Constructors that legitimately
fail at boot for I/O, parsing, or network reasons keep the
`(*T, error)` signature and do not use `must.NotNil`. Examples:
`httputil.NewCertReloader` (reads cert files),
`httpserver.New` (binds a port). The DI provider chains the error
upstream.

**Tests must satisfy the boundary.** When a test constructs one of
these types directly, it provides real or `gomock`-generated deps for
every required field. Don't loosen production validation to fit lazy
test wiring; widen the test fixture instead. The shared fixtures in
`internal/proxy/proxytest`, `internal/testutil`, and the per-package
`newTestHandler*` helpers exist for this purpose.

---

## Adding a New Component

Each component type has a fixed checklist; following it keeps the code
shape uniform across contributors.

### Adding a New Backend

1. Implement the `backend.ObjectBackend` interface in
   `internal/backend/<name>.go`. Constructor is `New<Name>(cfg config.BackendConfig)`.
2. Add the new backend type to the config validator in
   `internal/config/backends.go`.
3. Wire it into the backend factory in `internal/di/di.go` so existing
   `ProvideBackends` returns the new type when configured.
4. Add a unit test in `internal/backend/<name>_test.go`. Integration
   coverage comes from the existing MinIO testcontainer suite for any
   S3-compatible provider.
5. Update `README.md` and `docs/admin-guide.md` config sections.

### Adding a New Store Role

The narrow-role layering means new operations slot into one of the
existing role interfaces in `internal/store/core/interfaces.go`, or - if
they truly belong to a new bounded concern - into a new role.

1. Add the SQL in `internal/store/postgres/sqlc/queries/<role>.sql` and
   the matching method in `internal/store/sqlite/<role>.go`.
2. Run `make generate` to regenerate the sqlc-typed wrappers.
3. Surface the operation on the role interface in
   `internal/store/core/interfaces.go`. If business logic spans multiple
   tables or transactions, add the orchestration to
   `internal/store/core/<topic>.go` against `TxAdapter` so both engines
   share one implementation.
4. Add the CB-decorator method in `internal/store/cb_<role>.go`.
5. Update `MockStore` in `internal/testutil/mock_store.go` and any
   handwritten test mocks (`internal/proxy/mock_store_test.go` etc.).
6. Add a `Provide<Role>` provider in `internal/di/di.go` if it is a new
   role; otherwise the existing provider already covers it.
7. Schema changes go in a new `internal/store/postgres/migrations/000NN_*.sql`
   AND `internal/store/sqlite/schema.sql`. Bump
   `postgres.ExpectedSchemaVersion`.

### Adding a New Worker

1. Implement the worker in `internal/worker/<name>.go`. Define a narrow
   ops interface (e.g. `<Name>Ops`) and a narrow store interface
   (e.g. `<Name>WorkerStore`); the worker takes both as constructor args.
2. The worker exposes `Run(ctx) error` (long-lived) or `Process<X>(ctx)`
   (called per tick) - lifetime determines which.
3. Register a `Provide<Name>` provider in `internal/di/di.go`.
4. Register the worker with the lifecycle manager in `registerWorkerServices`
   inside `di.go`. Long-lived workers go through `lifecycle.Manager`;
   periodic workers wrap a ticker and an advisory lock.
5. Add metrics in a per-worker file under
   `internal/observe/telemetry/metrics_<worker>.go` if needed.

### Adding a New HTTP Handler

1. Define the handler in the appropriate `internal/transport/` sub-
   package. Handler signatures are `func (h *Handler) handle<X>(w http.ResponseWriter, r *http.Request)`.
2. The handler resolves its dependencies through the injector held on
   `*Handler`; it never constructs services itself.
3. Authentication middleware is applied at the `Server` level - do not
   reimplement auth inside individual handlers.
4. Audit-log every state-changing request via `audit.Log(ctx, "<event>", ...)`.
5. Update `docs/api-reference.md` for any new admin or UI endpoint.

### Adding a New Metric

1. Add the metric variable in the appropriate
   `internal/observe/telemetry/metrics_<domain>.go` file using
   `promauto.NewXxx`. Group by domain (cleanup, replication, breaker,
   etc.) - do not create a global metrics file.
2. Use the `s3o_<domain>_<noun>_<unit>` naming convention. Counters
   end in `_total`. Histograms end in `_seconds` or `_bytes`. Gauges
   take no suffix.
3. Add the metric to the dashboard JSON if it is operator-facing
   (`grafana/s3-orchestrator.json`).
4. Document the metric in `README.md` and `docs/admin-guide.md`.

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

All logs are structured `log/slog` JSON with the JSON handler set in
`internal/cli/serve/serve.go` `initLogging`. The full operational
logging conventions (helper package, attribute glossary, banned keys,
component scoping, outcome rollups) live in
[`docs/contributing/logging.md`](contributing/logging.md). This
section is the short summary; the contributing doc is the source of
truth.

### Structured operational logs

Every long-lived component holds a `*slog.Logger` field initialised
once in its constructor with the canonical `component` attribute:

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

r.log.WarnContext(ctx, "HEAD probe failed, leaving intent for next tick",
    "backend", p.BackendName,
    "key", p.ObjectKey,
    "intent_id", p.IntentID,
    logfmt.Err(err),
)
```

Three rules every call site must follow:

1. **Always render errors through `logfmt.Err`.** Passing `"error", err`
   directly serialises the error struct as `{}` in the JSON handler,
   which JS log viewers render as `[object Object]`, hiding the actual
   failure mode from operators.
2. **Component is an attribute, not a message prefix.** Messages are
   plain English ("HEAD probe failed"), not "Pending reaper: HEAD
   probe failed". The `component` attr added by the scoped logger
   makes every line filterable in Loki/Grafana without text matching.
3. **Use the canonical attribute glossary** in
   `docs/contributing/logging.md`. The CI lint rejects banned keys
   (`err`, `from_backend`, `remote_addr`, etc.).

`golangci-lint` enforces `context: all` on every `slog` call, snake-case
keys, and the banned-key list. See `.golangci.yml` for the active
sloglint configuration.

### Audit Logging

The `internal/observe/audit` package emits a separate stream of
structured entries marked `"audit": true` for security-relevant
operations. Audit logs are not subject to the operational-log
conventions above and continue to use `slog.LogAttrs` directly.

**S3 API requests** produce two correlated audit entries sharing the
same `request_id`:

- HTTP layer (`s3.PutObject`, `s3.GetObject`, etc.) — method, path,
  bucket, status, duration.
- Storage layer (`storage.PutObject`, `storage.GetObject`, etc.) —
  key, backend, size.

**Internal operations** generate their own correlation IDs:

```go
ctx = audit.WithRequestID(ctx, audit.NewID())
audit.Log(ctx, "rebalance.start",
    slog.String("strategy", cfg.Strategy),
    slog.Int("batch_size", cfg.BatchSize),
)
```

**Rules:**
- Use `audit.Log` for operations that change state or serve data (not
  for debug/health checks).
- Always pass context so the request ID propagates.
- Event names use dotted notation: `"s3.PutObject"`,
  `"storage.DeleteObject"`, `"rebalance.move"`.
- Include enough attributes to reconstruct the operation without
  reading other log lines.

### Request ID Propagation

Request IDs flow through context via `audit.WithRequestID` /
`audit.RequestID`. The `logfmt` package's init wires `audit.RequestID`
as the accessor `logfmt.RequestIDFromCtx` reads, so worker logs can
surface the inbound request ID without importing the audit package
directly.

- S3 API requests: extracted from `X-Request-Id` header or generated,
  set on context before auth.
- Internal operations: generated at the start of each background task
  tick or batch run.
- The ID is also set as a `s3o.request_id` attribute on OpenTelemetry
  spans.
- `trace_id` and `span_id` are automatically injected into JSON log
  output by `telemetry.TraceHandler` for any log call with an active
  span in context — use `*Context` slog variants
  (`InfoContext`/`WarnContext`/`ErrorContext`).

### Log Levels

| Level | Use |
|-------|-----|
| `slog.LevelDebug` | Verbose state; off by default. |
| `slog.LevelInfo`  | Lifecycle (startup/shutdown), terminal success of a notable operation, audit entries. |
| `slog.LevelWarn`  | Recoverable failure — caller proceeds, operator should know (failover, degraded mode, retry-able errors). |
| `slog.LevelError` | Unrecoverable failure of an operation — request fails, background tick aborts, integrity violation. |

---

## Tracing

OpenTelemetry tracing is wired in `internal/observe/telemetry`. Every
S3 request, manager call, backend call, and significant background tick
produces a span; spans inherit the request id from `audit.RequestID(ctx)`
so log lines and traces cross-link.

### Starting a Span

Use `telemetry.StartSpan` rather than the OTel API directly. The helper
guarantees the `s3o.request_id` attribute is set when the context carries
one and that the span is registered on the global tracer:

```go
ctx, span := telemetry.StartSpan(ctx, "Manager PutObject",
    telemetry.AttrOperation.String("PutObject"),
    telemetry.AttrBackend.String(backendName),
)
defer span.End()
```

Always defer `span.End()` on the line after the `StartSpan` call so an
early return cannot leave the span open.

### Span Naming

Span names are stable strings (no per-request data) so traces aggregate
cleanly:

| Layer | Prefix | Example |
|---|---|---|
| Manager | `Manager <Op>` | `Manager PutObject`, `Manager GetObject` |
| Backend | `Backend <Op>` | `Backend PutObject`, `Backend GetObject` |
| Background worker | `<Worker>.<Op>` | `Replicator.Replicate`, `CleanupWorker.ProcessCleanupQueue` |
| Store engine | `<Engine> <Op>` | `Postgres RecordObject`, `SQLite ListObjects` |

The `Manager `/`Backend ` prefixes are constants (`managerSpanPrefix`,
`spanPrefix`) so the layer attribution stays consistent if the prefix
ever changes.

### Attributes

Standard attribute keys live in `internal/observe/telemetry/attrs.go` so
every span uses the same string. Add new keys there rather than inlining
strings at call sites.

```go
span.SetAttributes(
    telemetry.AttrBackend.String(backendName),
    telemetry.AttrObjectSize.Int64(size),
)
```

High-cardinality values (object keys, request IDs as attributes) are
allowed only on leaf spans. Do not put object keys on long-running
worker spans because that explodes trace storage cardinality.

### Recording Errors

Span errors are recorded via the OTel `RecordError` + `SetStatus` pair
so trace UIs flag the span as failing. Do not log the error inside the
span block - the audit/slog log line already carries the error and
trace correlation links the two.

```go
if err != nil {
    span.RecordError(err)
    span.SetStatus(codes.Error, err.Error())
    return err
}
```

### Trace-to-Log Correlation

Every `slog.InfoContext`, `WarnContext`, `ErrorContext` call inside an
active span automatically attaches `trace_id` and `span_id` fields via
`telemetry.TraceHandler`. This is why context-aware slog must be used
(see Logging and Audit). Never use `slog.Info` (no context) when an
active span exists - that produces a log line with no trace correlation.

---

## Metrics

Prometheus metrics live in `internal/observe/telemetry/metrics_*.go`,
one file per domain. The `promauto` constructors register the metric
with the default registry on package init; nothing else needs to be
called to make a metric visible at `/metrics`.

### Naming

| Type | Format | Example |
|---|---|---|
| Counter | `s3o_<domain>_<noun>_total{labels}` | `s3o_cleanup_queue_enqueued_total{reason}` |
| Gauge | `s3o_<domain>_<noun>{labels}` | `s3o_cleanup_dlq_depth` |
| Histogram | `s3o_<domain>_<noun>_seconds` or `_bytes` | `s3o_request_duration_seconds` |

The `s3o_` prefix is mandatory so a multi-service Prometheus instance
can pick out orchestrator metrics with one label match. The unit suffix
goes at the end (`_seconds`, `_bytes`, `_total`) to match Prometheus
conventions.

### File Layout

Group metrics by domain in `internal/observe/telemetry/metrics_<domain>.go`.
Existing domains: `audit`, `breaker`, `cache`, `cleanup`, `encryption`,
`meta`, `quota`, `rebalance`, `replication`, `request`. Add a new file
when a new feature has its own logical domain - do not append to a
random existing file just to avoid creating one.

Each metric variable carries a 2-3 line godoc explaining what it
measures and which dashboard panel reads it.

### Label Cardinality

Labels multiply storage cost. Hard rules:

- **Allowed labels**: `backend`, `operation`, `status`, `reason`,
  enumerated states. These come from a small fixed set per dimension.
- **Forbidden labels**: `key`, `request_id`, `user`, IP, anything
  user-supplied or identity-bearing. These create unbounded label
  cardinality and will eventually crash Prometheus.
- Buckets for histograms must be explicit (`prometheus.LinearBuckets`
  or `ExponentialBuckets`); never default. Pick buckets that span the
  expected p50 to p99.9 range with at most 10-15 buckets total.

### Updating a Metric

Counters use `Inc` or `Add`. Gauges use `Set`. Histograms use `Observe`.
Always pass label values in the order they appear in the metric
declaration; Prometheus does not protect against argument-order swaps.

```go
telemetry.CleanupQueueEnqueuedTotal.WithLabelValues(reason).Inc()
telemetry.CleanupDLQDepth.Set(float64(depth))
telemetry.RequestDurationSeconds.WithLabelValues(operation, status).Observe(elapsed.Seconds())
```

### Tying Metrics, Tracing, and Audit Together

Every state-changing operation should produce all three signals at the
relevant scope:

| Signal | Where |
|---|---|
| Metric | At the layer where the count makes sense (manager for per-request counters, worker for per-tick counters) |
| Span | One per layer call (`Manager <Op>`, `Backend <Op>`, store engine) |
| Audit log | At the *outer* layer responsible for the operation, with the request id from `audit.RequestID(ctx)` |

Skipping one degrades observability: missing metric means dashboards
cannot alert; missing span means a slow request cannot be traced;
missing audit log means a customer dispute cannot be reconstructed.

---

## Testing

### Unit Tests

- Test files live alongside the code they test: `server_test.go`, `objects_test.go`
- Use table-driven tests for operations with multiple input/output combinations
- Use `go.uber.org/mock/mockgen` for generating mocks from interfaces. Add `//go:generate mockgen` directives and run `make generate`. Generated mocks live alongside the interface they mock.
- Legacy hand-written mocks exist in `internal/testutil/` and some packages; prefer generated mocks for new tests
- Test names follow `TestFunctionName_Scenario` convention

### Integration Tests

- Located in `internal/integration/`
- Gated behind the `integration` build tag
- Run in-process with real MinIO and PostgreSQL containers
- Cover end-to-end flows: CRUD, quota enforcement, multipart, replication, circuit breaker

### Benchmarks

- Benchmark files use the `_bench_test.go` suffix: `auth_bench_test.go`, `helpers_bench_test.go`
- Integration benchmarks live in `internal/integration/bench_test.go` (gated behind the `integration` build tag)
- Run benchmarks: `make bench`, `make bench-auth`, `make bench-crypto`, etc. Override duration with `BENCH_TIME=5s make bench`
- Use `b.Loop()` (Go 1.24+) for all benchmark loops — not `for i := 0; i < b.N; i++`
- Use `b.SetBytes()` for throughput benchmarks so Go reports MB/s
- Use `b.ResetTimer()` after setup/population steps
- Use `b.RunParallel` with `pb.Next()` for concurrent benchmarks (`b.Loop()` cannot replace `pb.Next()` inside `RunParallel` callbacks)
- Pre-compute keys and test data outside the measured loop — `fmt.Sprintf` inside a benchmark loop measures string formatting, not the code under test
- Benchmark names follow `BenchmarkFunctionName/variant` with sub-benchmarks for different input sizes

### Fuzz Tests

- Fuzz test files use the `_fuzz_test.go` suffix: `auth_fuzz_test.go`, `xml_fuzz_test.go`
- Run fuzz targets: `make fuzz` (default 30s per target). Override with `FUZZ_TIME=5m make fuzz` for deeper exploration
- CI runs a 10s smoke test per target to catch regressions
- Seed the corpus with valid inputs, edge cases, and adversarial inputs via `f.Add()`
- Fuzz callbacks must never panic — verify invariants with `t.Errorf` instead
- Match the production code path — use `xml.NewDecoder().Decode` not `xml.Unmarshal` when production uses streaming decoders
- **Differential oracles:** When two implementations exist for the same operation (e.g., `parseSigV4Fields` and `parseSigV4FieldsDirect`), fuzz both and assert they agree on every input
- **Structural invariants:** Assert properties of the output that must always hold (e.g., "canonical request has exactly N newlines", "bucket never contains a slash", "nonce is always 12 bytes")
- Do not assert application-level validation at the parsing layer — XML decoders accept negative integers; the handler layer validates range constraints

### Test Patterns

- **Generated mocks** (`mockgen`) are the preferred approach for new tests — they stay in sync with interfaces automatically
- **FailableStore** wraps a store to inject errors for circuit breaker testing
- Test assertions use standard `testing.T` methods, not external assertion libraries

### Coverage Exclusions

Go has no built-in coverage ignore directive. Use the Codecov `// codecov:ignore` inline comment to exclude untestable code (process entry points, `os.Exit` wrappers) from coverage reports. Always include a reason after the directive:

```go
func runValidate() { // codecov:ignore -- os.Exit wrapper, logic tested via validateConfig
    // ...
    os.Exit(1)
}
```

Use this sparingly and only for code that genuinely cannot be tested:
- `main()` and subcommand entry points that call `os.Exit`
- Signal handlers and process lifecycle glue

Do **not** use `codecov:ignore` for code that requires a database, S3 backend, or Redis — integration tests run with testcontainers and contribute coverage. Extract testable logic into separate functions that return errors instead of calling `os.Exit` directly.

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

## Versioning

The `.version` file in the repository root controls the version baked into binaries, Docker images, and Debian packages. Every PR that changes Go or SQL files must bump the version.

**Patch bump** (v0.X.Y -> v0.X.Y+1) for:
- Bug fixes
- Refactoring (no behavior change)
- Documentation updates
- Test improvements
- Dependency updates
- Cleanup and code style changes

**Minor bump** (v0.X.0 -> v0.X+1.0) for:
- New features (new config fields, new API endpoints, new CLI commands)
- Breaking changes (required config fields, changed defaults, removed options)
- Schema migrations that add tables or columns

---

## Documentation Updates

When a PR changes config fields, API behavior, or deployment requirements, update all affected documentation in the same PR:

- `packaging/config.yaml` — sample config
- `config.yaml` — local dev config
- `README.md` — config reference section
- `docs/admin-guide.md` — operational documentation
- `docs/security-hardening.md` — if security-relevant
- `docs/disaster-recovery.md` — if it affects failure modes
- `web/content/guides/*.md` — deployment guides
- `deploy/` — Nomad, Kubernetes, and Helm manifests

Search across all docs with `grep -rn 'field_name' README.md docs/ web/content/ packaging/ deploy/` before committing to catch every reference.

---

## Branch Naming

When a branch corresponds to a GitHub issue, use this format:

```
GH_ISSUE_<issue number>-<description of topic>
```

Examples:
- `GH_ISSUE_251-worker-pool-parallelism`
- `GH_ISSUE_42-multipart-upload-cleanup`

For branches without a linked issue, use a short kebab-case description of the topic.

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

package proxy

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

package proxy

// Let's create the replicate function
func (m *BackendManager) Replicate(ctx context.Context, cfg config.ReplicationConfig) (int, error) {
    // get the start time
    start := time.Now()
    // ...
}
```

---

**Remember:** Comments should explain *why* decisions were made, not *what* the code does. The code itself should be clear enough to understand *what* it does.
