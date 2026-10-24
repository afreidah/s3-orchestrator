---
description: "The project's Go conventions: comment and file-header format, package layout, error handling, logging, testing, and versioning rules."
---

**Author:** Alex Freidah

---

## Table of Contents

- [Core Principles](#core-principles)
- [Comment Types and Spacing](#comment-types-and-spacing)
- [File Headers](#file-headers)
- [Package Docs (doc.go)](#package-docs-docgo)
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
- **Box comment spacing** - ALL box comments (file headers with 79-dash dividers and sections with 73-dash dividers) ALWAYS have a blank line after
- **Professional tone** - No personal references, no numbered lists, no casual language
- **Self-documenting** - Code explains *why*, not just *what*
- **Streaming over buffering** - Use `io.Pipe` and streaming patterns for object data; never buffer entire objects in memory
- **Buffer pooling** - Use `bufpool.Copy` instead of `io.Copy` for all streaming I/O to reuse buffers and reduce GC pressure
- **Context propagation** - Pass `context.Context` through all function chains for cancellation, tracing, and audit correlation

---

## Comment Types and Spacing

### File Header (79-dash dividers)

The divider is `// ` followed by 79 dashes, 82 characters in all.

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

### Major Section Box (73-dash dividers)

The divider is `// ` followed by 73 dashes, 76 characters in all.

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
- The box goes above the first declaration's doc comment, never between a doc comment and its declaration

**When to use them:**

Every file with more than one logical group of declarations carries section boxes, so a reader lands in a familiar shape whichever file they open. A file under roughly 100 lines with a single concern does not - a box per file is noise, not structure.

**Section vocabulary.** Use these names, in this order, so the same concept is not called three things across the tree:

| Section | Holds |
|---------|-------|
| `CONSTANTS` | package-level constant blocks |
| `TYPES` | the types a file declares |
| `INTERFACE` | a consumer-declared or role interface |
| `CONSTRUCTOR` | `New*` and the wiring it needs |
| `PUBLIC API` | the exported methods and functions, when the file has one concern |
| *domain sections* | the file's work, named for it, when it has several: `ADMISSION`, `FLUSH`, `LISTINGS`, `SCRUB QUEUE`, `UNDER-REPLICATION` |
| `LIFECYCLE` | `Close`, `Shutdown`, and other teardown |
| `INTERNALS` | unexported helpers the sections above call |

`PUBLIC API` and `INTERNALS` are the most common pair in the tree. When a file's exported surface splits into distinct areas, name the areas instead: `LISTINGS` / `STATISTICS AND WRITES` / `INTERNALS` tells a reader more than one `PUBLIC API` box over all of it.

### Comments Inside Declaration Blocks

A comment line above a single entry inside a `struct`, `interface`, `const` or `var` block breaks the block's visual flow and makes a block that should read at a glance three times its height. Two forms are allowed instead:

- **End-of-line**, when the note is short:

```go
const (
	StateClosed   State = iota // healthy  -  all calls pass through
	StateOpen                  // down  -  return sentinel error
	StateHalfOpen              // probing  -  one call allowed through
)
```

- **In the block's doc comment**, when the note carries real reasoning:

```go
// ExistingCopy is the projection of an object_locations row that promotion
// and overwrite logic needs from a SELECT-for-update read.
//
// Encrypted and HasDEK say whether the row claims the bytes are an envelope and
// whether the key that reads them is still present. They are carried here so a
// decision about which copy to drop cannot destroy the last row able to decrypt
// the object.
type ExistingCopy struct {
	BackendName string
	SizeBytes   int64
	CreatedAt   time.Time
	Encrypted   bool
	HasDEK      bool
}
```

An entry whose name already says what it is gets no comment at all. `revive`'s `exported` rule is satisfied by the doc comment on the block, so moving the text up does not cost lint coverage.

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
  YES -> Use 79-dash dividers, blank line AFTER

Is this a major section (types, public API, internals)?
  YES -> Use a box with 73-dash dividers, blank line AFTER

Is this a minor division or label within a function?
  YES -> Use a standard single-line comment, NO blank line before code

Is this explaining a specific line?
  YES -> Use inline comment
```

**Key Rule:** ALL box comments (79-dash and 73-dash) have a blank line after. Single-line comments have no extra spacing.

---

## File Headers

Every hand-written `.go` file starts with a header block with 79-dash dividers. Generated files (sqlc output under `store/postgres/sqlc`, mockgen output) keep the header their generator writes.

```go
// -------------------------------------------------------------------------------
// Object Operations - PUT, GET, HEAD, DELETE, COPY
//
// Author: Alex Freidah
//
// Object-level CRUD operations. Handles backend selection via routing
// strategy, read failover across replicas, broadcast reads during degraded
// mode, and usage limit enforcement on reads and writes.
// -------------------------------------------------------------------------------

package object
```

**Rules:**
- Use `//` comments (not `/* */` blocks)
- Title line describes the file's scope, not the package
- Description covers purpose, key behaviors, and dependencies
- The `package` declaration follows immediately after the closing divider + blank line

The blank line between the header and `package` is load-bearing: it keeps the box header a file comment rather than the package comment, so the package doc lives in one place (see below).

---

## Package Docs (doc.go)

The package-level godoc lives in a dedicated `doc.go`, the idiomatic Go pattern (`net/http`, `encoding/json`). It is the one file exempt from the box header: it opens directly with a `// Package X ...` comment adjacent to the `package` clause, so it - and only it - is the package comment. This is what renders as the package overview on the godoc site.

```go
// Package backend defines the ObjectBackend abstraction over the S3-compatible
// providers the orchestrator stores objects on, plus the wrappers that harden
// it ...
package backend
```

**Rules:**
- One sentence is the floor; pin the package's invariants and link out to the implementation files for per-symbol detail
- Only `doc.go` carries a package comment; every other file keeps its box header + blank line before `package`
- Every hand-written package has one, including `main` and test-only helper packages. A new package without a `doc.go` is incomplete. The generated `store/postgres/sqlc` package is the exception

`go/doc` concatenates every package comment it finds, in file-name order, so a second one does not replace the first - it appends to it, and the rendered overview says the same thing twice. That is the reason for the one-file rule.

mockgen writes `// Package X is a generated GoMock package.` above the package clause of everything it emits. In a package whose mocks are `_test.go` files that never reaches godoc and is left alone; where they are not (`storetest`, `opstest`, `backendtest`), `make generate` strips it so `doc.go` stays the only package comment.

---

## Go Conventions

### Indentation

- **1 tab** - Go standard (`gofmt` enforced)

Formatting is a lint failure, not a convention: `make lint` runs `gofmt` and `goimports`, and `make fmt` applies both.

### Imports

Group imports in three blocks separated by blank lines:

```go
import (
    "context"
    "fmt"
    "time"

    "github.com/prometheus/client_golang/prometheus"
    "go.opentelemetry.io/otel/attribute"

    "github.com/afreidah/s3-orchestrator/internal/observe/audit"
    "github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)
```

Order: stdlib, external packages, this module's packages. `goimports` enforces it through `local-prefixes: github.com/afreidah/s3-orchestrator` in `.golangci.yml`. When two imports share a name, alias the less central one by role (`s3be`, `objcache`, `pobserve`) rather than by abbreviation.

### Naming

- **Exported types** get standard Go doc comments placed directly above the declaration
- **Constants** grouped by concern with `const` blocks, named in `CamelCase`
- **Sentinel errors** use `Err` prefix: `ErrObjectNotFound`, `ErrDBUnavailable`
- **S3Error type** (`store/core/errors.go`) wraps HTTP status + S3 error code for typed error responses
- **Operation names** are the typed constants of `s3op.Operation`, named for the S3 API call: `s3op.PutObject`, `s3op.GetObject`. The set is closed: accounting, usage limits and config validation all take an `s3op.Operation`, never a bare string

### Struct Organization

Group related fields, with end-of-line comments on the ones whose role is not obvious from the name and type (`internal/proxy/object/manager.go`, abridged):

```go
type Manager struct {
    core        Runtime                // infrastructure subset: backends, usage, timeout, eligibility, error classification, metrics
    coord       *writepath.Coordinator // write-path helpers shared with the multipart manager
    stores      Stores                 // direct store access for read paths and quota inspection
    encryptor   *encryption.Encryptor
    objectCache objcache.ObjectCache   // nil when object data caching is disabled
    failover    *readpath.Failover     // per-key read failover + degraded-mode broadcast orchestrator
    log         *slog.Logger
}
```

### Interface Design: Consumer-Declared Interfaces

This codebase follows the Go-idiomatic "accept interfaces, return structs" pattern: **producer packages export concrete `*Type` values with no producer-side interface**, and **each consumer declares its own narrow interface** listing only the methods it actually calls. The concrete type satisfies every consumer's local interface because Go interfaces are structurally typed.

Applied across `internal/store` (the per-role store interfaces consumed at use sites), `internal/worker` (`Ops` in `ops_runtime.go`), and the `internal/proxy/*` subpackages (`multipart.Runtime`, `object.Runtime`, `writepath.WriteRuntime`).

**Rationale:**
- A consumer's dependency footprint is documented in its own source file.
- Adding a method to a producer type never bloats existing consumer mocks.
- Tests can mock at the granularity of what is used (typically 4-10 methods), not the full producer surface.
- Aligns with Rob Pike's "accept interfaces, return structs" guideline.

**Trade-offs:**
- Each consumer declares its own small interface (extra text, but localized).
- The composition layer (the root `proxy` package, `internal/di`) still holds concrete types - it owns construction and is the seam where interfaces meet implementations.

**Where the interfaces live:**

| Location | Holds |
|---|---|
| `internal/proxy/<consumer>/consumer_interfaces.go` | The narrow interfaces `readpath`, `multipart`, `writepath` and `object` declare against other proxy subpackages |
| Beside the constructor that takes it | Everywhere else: `backendstate.Runtime` in `manager.go`, `writepath.CoordinatorStores` in `coordinator.go`, the `Stores` composites of `object`, `multipart`, `reconcile` and `usage` |
| `internal/worker/ops_runtime.go` | The worker-side `Ops`, `FleetOps` and `Placement` interfaces against the proxy infrastructure |
| `internal/ops/interfaces.go` | What the operations layer needs from workers and the runtime (`ReplicatorOps`, `ScrubberOps`, ...) |
| `internal/store/core/interfaces.go` | Per-role narrow store interfaces (consumers compose them when they need to declare a minimal store dependency) |

**Naming convention:** name the interface for the producer concept and let the package name the consumer - `multipart.Runtime` and `object.Runtime` are each package's view of `*infra.BackendRuntime`.

**Constructor shape:** consumers take the interfaces, not concrete pointers. Composition-layer code (the root proxy package, DI providers) passes the concrete `*infra.BackendRuntime`, `*writepath.Coordinator`, `*object.Manager`, etc., and the concrete types satisfy the interfaces implicitly.

```go
// internal/proxy/multipart/consumer_interfaces.go
type Runtime interface {
    GetBackend(name string) (backend.ObjectBackend, error)
    Usage() *counter.UsageTracker
    WithTimeout(ctx context.Context) (context.Context, context.CancelFunc)
    // ... only what multipart calls
}

// internal/proxy/multipart/manager.go
type Manager struct {
    core  Runtime                // not *infra.BackendRuntime
    coord *writepath.Coordinator // concrete: none of the reasons below applies
    // ...
}
```

**Mocking:** generated mocks are not produced eagerly. When a test actually needs to mock a consumer-declared interface, put a directive directly above it that writes a `_test.go` mock into the same package, and run `make generate`:

```go
//go:generate mockgen -destination=mock_stores_test.go -package=writepath github.com/afreidah/s3-orchestrator/internal/proxy/writepath CoordinatorStores
```

Until a mock is needed, the interface declaration alone documents the dependency surface - generating unused mocks is busywork.

**When NOT to declare a narrow interface (#918).** A consumer-side interface earns its keep when at least one of these is true:

1. **Multiple implementations actually exist** (a real polymorphism point, e.g. `keySource` over S3 iter + DB iter).
2. **A test fake genuinely benefits from the seam** - a hand-rolled fake or `gomock`-generated mock that lets tests exercise the consumer without standing up the real producer (e.g. `worker.Ops` mocked across ~60 test sites; `ops.ReplicatorOps`/`OverReplicationOps`/`ScrubberOps` and `admin.Reconciler`, whose fakes drive the operations layer and the admin handlers).
3. **An import cycle would otherwise form** (e.g. `readpath.LocationCache` - without the interface, `readpath` would have to import `object` which already imports `readpath`).
4. **The interface models a real domain boundary** between subsystems (`backendstate.Runtime`, `multipart.Runtime`, `object.Runtime`, `writepath.WriteRuntime`).

If none apply - single impl, single consumer, no test fake, no cycle, no boundary - pass the concrete `*Type` directly. Examples cut: `readpath.ObjectLocationLister`, `multipart.MultipartCoordinator`, `multipart.StaleCleaner`, `accounting.UsageTracker`, `object.Coordinator`. Splitting one dependency into role interfaces that nothing uses on its own fails the same test.

**There is no exported union of the store roles.** A type that unions every per-role interface is a god interface, and an exported one in the package every consumer already imports is an invitation to take it as a dependency. So the union exists in exactly two places, neither of them reachable from a feature package:

- `internal/di.metadataStore`, unexported, because the composition root genuinely has to hold one opened engine before aliasing it into roles. `openStore` returns either engine as it, which is also the compile-time check that each implements everything.
- `storetest.MetadataStore`, the mockgen target, so one generated mock stands in wherever a test needs a fully-populated store.

The union is not a fact about the domain: nothing in persistence requires one object to implement all nineteen roles, and a design that split persistence across several objects would satisfy every role interface and violate nothing. What it encodes is that there happens to be one store type per engine, which is wiring rather than domain.

Every feature package under `internal/proxy/*`, `internal/worker/`, `internal/transport/*` declares its own role-composite naming exactly the per-role interfaces it needs, plus any single methods it calls from a role it otherwise does not need:

  ```go
  // internal/worker/scrubber.go
  type ScrubberStore interface {
      core.IntegrityStore
      LedgerStats(ctx context.Context) (core.LedgerStats, error)
      DeleteObjectLocation(ctx context.Context, loc *core.ObjectLocation, keepAtLeast int) (core.DeletedCopy, error)
      // ...
  }

  // internal/proxy/writepath/coordinator.go
  type CoordinatorStores interface {
      core.ObjectStore
      core.QuotaStore
      core.PendingStore
      core.CleanupStore
      CreateMultipartUpload(ctx context.Context, params *core.CreateMultipartUploadParams) (bool, error)
  }
  ```

  Each concrete store satisfies every such composite implicitly, so DI wiring stays one line per provider while each consumer's dependency footprint is documented in its own source file. A package that took the whole surface would give the misleading impression it might call any DB role, and auditing what it actually touches would then mean reading the implementation.

**Producer-side interfaces are an anti-pattern.** `*infra.BackendRuntime`, `*writepath.Coordinator`, `*object.Manager`, `*multipart.Manager` are exported as concrete pointer types with no sibling `Core`/`Coordinator`/`Manager` interface that mirrors their public surface. Producer-side interfaces force every consumer to mock the full producer API, which is exactly what this pattern is built to avoid.

**Logger is not a behavior dependency.** Never include `Log() *slog.Logger` in a consumer-declared interface. The logger is observability infrastructure: it has no return value the consumer depends on, and the per-component scope is a property of the consumer itself, not of the producer. Components build their own `log *slog.Logger` field in the constructor body via `slog.Default().With(logfmt.Component("<slug>"))` (see Logging and Audit), so each subsystem owns its component name and tests do not need to thread a logger through dependency interfaces.

**Single-method interface names follow the Go `-er` convention.** A single-method interface is named after its method, with the verb in agent-noun form: `Read` -> `Reader`, `Close` -> `Closer`, `Stringer` for `String()`. Names ending in `-Ops`, `-Store`, `-ing`, or other shapes that do not describe an actor get flagged by `golangci-lint` / sonar (rule S8196) and should be renamed:

| Method | Good | Bad |
|---|---|---|
| `Read(p []byte) (int, error)` | `Reader` | `IOOps` |
| `Close() error` | `Closer` | `Closeable` |
| `GetAllObjectLocations(...)` | `ObjectLocationLister` | `LocationStore` |
| `CleanupStaleMultipartUploads(...)` | `staleMultipartCleaner` | `multipartCleanupOps` |
| `UpdateUsageLimits(...)` | `usageLimitsApplier` | `usageLimitsHook` |
| `UpdateQuotaMetrics(...)` | `quotaMetricsRefresher` | `metricsHook` |

For interfaces that exist to *provide* a value (typical "Acct" / "Stores" / "Config" getters), name the interface after the returned type plus `Provider` or `Source` - `KeyProvider`, `ConfigSource` for `Config() *Config`. The `Provider` / `Source` suffix is also an agent noun and satisfies the rule.

Multi-method interfaces are exempt: `worker.Ops`, `object.Runtime`, `backendstate.Runtime` describe a role, not a single action, so the `-er` form does not apply.

### Where new methods live

The split is by **persistence coupling**:

- **`infra.BackendRuntime`** owns methods that touch only backend objects,
  in-memory usage counters, admission, timeouts, error classification, and
  metrics - no `store.X` calls. Keeping them store-free is what lets every
  worker reuse the runtime through the role interfaces without dragging the
  metadata-store dependency along.
- **A collaborator under `internal/proxy/*`** owns methods that read or write
  the metadata store, open transactions, or coordinate more than one of the
  others. `object`, `multipart`, `writepath`, `backendstate` and `usage` each own a
  slice of that, and each takes the store roles it needs directly.

When in doubt: if the method needs the store, it belongs to whichever
collaborator owns that concern, not to the runtime. There is no root object
to fall back on, which is the point - a method with no obvious owner is a sign
the concern has no home yet, rather than a reason to make one type hold it.

### Per-Backend Accounting: Use the Recorder

Per-backend usage and per-operation metric accounting flow through one shared helper: `internal/proxy/accounting.Recorder`. Every consumer holds the same `*accounting.Recorder` (resolved via `core.Acct()`) and calls the named methods rather than the underlying `Usage().Record(...)` or `RecordOperation(...)`:

| Intent | Use | Avoid |
|---|---|---|
| May this backend take the call (usage limits) | `acct.Allow(backend, op, egress, ingress)` | `Usage().WithinLimits(...)` |
| Backend call attempted, no bytes (success or failure) | `acct.APICall(op, backend)` | `Usage().Record(b, op, 0, 0)` |
| Successful GET-like (bytes left the backend) | `acct.Egress(op, backend, size)` | `Usage().Record(b, op, size, 0)` |
| Successful PUT-like (bytes arrived at the backend) | `acct.Ingress(op, backend, size)` | `Usage().Record(b, op, 0, size)` |
| Per-operation Prometheus histogram | `acct.Operation(op, backend, start, err)` | `RecordOperation(op, b, start, err)` |
| Common combo: Operation + Ingress | `acct.PutSuccess(op, backend, size, start)` | both above, two lines |

The cardinal rule "every backend call costs one API-call charge regardless of outcome" lives in the method bodies, not in repeated inline comments at every call site. The argument-order risk of the bare `Record(b, op, 0, size)` vs `Record(b, op, size, 0)` form is eliminated because ingress vs egress is named in the method.

Backend-to-backend copies (replication, rebalance, drain and the bulk rewrite passes) are charged inside `StreamCopy`, which admits both legs and records what each one moved, so their callers charge nothing.

Exception - the usage tracker's own tests in `internal/counter` call `Record` directly - they're verifying the tracker, not the accounting rule.

### Variable Naming

Avoid shadowing package imports with local variable names. When a function receives or creates a `backend.ObjectBackend`, name the variable `be`, not `backend`:

```go
// Good
be, err := mp.GetBackend(mu.BackendName)
etag, err := be.PutObject(ctx, key, body, size, contentType, nil)

// Bad - shadows the backend package import
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

### Bit-Flag Sets

A set of independent yes/no properties is an integer of bits, not a struct of bools or one string state. The three in the tree are `core.PermissionSet` (what a grant allows), `core.BackendFlags` (an operator-set backend state: cordoned, draining, drained, drain-failed) and `core.BackendCalls` (the backend calls whose circuit breaker is closed).

- **Declare the bits and one vocabulary.** The type declares its bits with `1 << iota` and one `bitset.Vocabulary` pairing each bit with its name, in render order. It renders through the vocabulary (`Names`, `Join`), looks names up through it (`Lookup`, `AllNames`), and its test calls `Vocabulary.Validate`, which fails on a shared bit or a repeated name.
- **Test through `bitset`.** `bitset.Has` (carries every bit), `bitset.Any` (carries at least one) and `bitset.Within` (carries nothing outside) are the only places the bit expressions are written. A type's methods (`PermissionSet.Has`, `BackendFlags.AcceptedBy`) call them. `internal/util/bitset` holds only what more than one flag type uses; anything one type needs on its own (the `all` shorthands, parsing operator input, `DrainState`) stays with that type.
- **Masks live beside the type, named by purpose.** Consumers import `core.AcceptForWrite` or `core.RequiredForRead` and never assemble a mask of their own, so two consumers that want the same backends share one definition, and a deliberate difference between them is two names in one file.
- **The set's meaning picks the mask.** A set of capabilities (`PermissionSet`, `BackendCalls`) is tested with a required mask: the consumer states what it needs and tests `Has(required)`, and a bit added later matters only to the consumers that come to need it. A set of reasons to hold something back (`BackendFlags`) is tested with an accept mask: the consumer states the flags it tolerates and tests `AcceptedBy(accept)`, so a flag added later is refused everywhere until a mask accepts it, and a forgotten update holds a backend back instead of sending it work.
- **Say how the set is stored, because it decides whether bits may move.** `PermissionSet` is stored as names in a text column and `BackendCalls` is never stored, so their bits may be renumbered. `BackendFlags` is stored as an integer because SQL tests it, so its bit positions are part of the schema: bits are only ever added. The type's doc comment states which rule applies, and a set stored as an integer is signed (`int64`) to match the column.
- **No bit numbers in SQL.** A mask or a bit reaches a query only as a parameter the store binding passes from `core`: `(c.flags & ~@accepts::bigint) = 0`, never `(c.flags & 15) = 0`. sqlc does not recognize a parameter directly after `~`, so wrap it: `~(@accepts::bigint)`.
- **Parenthesize the test.** Write `(flags & mask) != 0`, not `flags&mask != 0`, so the grouping reads at a glance. gofmt keeps the spaces inside parentheses.

### No Empty Cleanup Funcs

Never return `func() {}` to satisfy a `(T, cleanup func(), error)` signature when the "no-cleanup" branch has nothing to do. SonarQube flags empty function literals (rule `go:S1186`) and the empty literal hides whether the branch *meant* to be empty or someone forgot to wire the cleanup.

**Why:** Past SonarQube findings on PRs that returned `func() {}` as the no-op cleanup arm of an in-memory-vs-tempfile sink. The repo has hit the same lint multiple times across different abstractions.

**How to apply:** Attach the cleanup as a method on the returned type so the caller writes `defer x.Cleanup()` once and the method internally branches on which underlying resource (if any) actually needs releasing. The lint never sees an empty literal, and the caller code stays identical across branches.

```go
// Bad - empty literal trips S1186 on the in-memory branch.
func newSink(size int64) (*sink, func(), error) {
    if size <= memoryLimit {
        return &sink{buf: &bytes.Buffer{}}, func() {}, nil
    }
    f, _ := os.CreateTemp("", "x-*")
    return &sink{file: f}, func() { _ = f.Close() }, nil
}

// Good - cleanup is a method that no-ops on the branch with no resource.
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
- **`workerpool.Run`** for bounded fan-out over a slice; never hand-roll a semaphore and `WaitGroup`
- **`batch`** (`Pager`, `Runner`, `Summary`) for any pass that pages through rows and acts on each: it owns the cursor walk, per-item outcome counting and the zero-progress guard
- **Graceful shutdown** via `signal.NotifyContext` in `main`; the cancelled context drives every service's shutdown

---

## Project Structure and Layers

The codebase splits along strict layers. New code goes in the layer whose
responsibility matches; an outer layer must not import a more outer layer, and
inner layers must never import the transport packages.

```
cmd/s3-orchestrator/         # Binary entry point: one subcommand table, dispatch + os.Exit
internal/
  cli/                       # Subcommand implementations
    serve/                   # `serve`: load config, build a runtime.Runtime, run it
    adminctl/                # `admin ...` subcommands over the admin API
    adminclient/             # Shared HTTP transport for the admin API (adminctl, tui)
    admintarget/             # Resolves the admin address and credentials from flags/env/config
    output/                  # Terminal rendering of admin API responses
    tui/                     # `tui`: the terminal dashboard
    initcmd/                 # `init`: interactive config writer
    synccmd/                 # `sync`: import a bucket's existing objects
    validatecmd/             # `validate`: config check
  runtime/                   # Daemon composition root: observability, DI, listener, workers, reload, shutdown order
  reload/                    # SIGHUP-driven config reload
  config/                    # YAML + env config: types, defaults, validators
  transport/                 # HTTP layer (no business logic)
    httpserver/              # The listener and route mux for S3, admin, UI and metrics
    s3api/                   # S3-compatible XML/REST handlers
    admin/                   # Admin API handlers and the route table
    ui/                      # Dashboard handlers + templates
    auth/                    # SigV4 verification, users, grants, bucket registry
    cors/                    # Browser preflight for the S3 surface
    httputil/                # Cross-cutting HTTP helpers (trusted proxies, login throttle, cert reload)
  ops/                       # Operations layer: what the orchestrator can be told to do, shared by admin and UI
  proxy/                     # Orchestration layer: a namespace over the collaborators below, with no root object
    infra/                   # BackendRuntime: fleet, usage counters, admission, timeouts, error classification
    accounting/              # Recorder: per-backend usage and per-operation metrics
    object/                  # Object CRUD, read failover, broadcast reads
    multipart/               # Multipart upload lifecycle
    writepath/               # Shared write helpers: routing, pending intents, recovery
    readpath/                # Read failover orchestrator
    backendstate/            # Operator-set backend flags: cordon, drain
    reconcile/               # Folding a backend's real contents into the ledger
    expiry/                  # Lifecycle expiration rules
    usage/                   # Counter flush to the store + the drift reconcile
    metrics/                 # Per-backend metric collector
    dashboard/               # Dashboard read aggregator
    etag/                    # S3 entity tags
    observe/                 # Per-operation completion observability
    proxytest/               # Shared store fixtures for proxy tests
  worker/                    # Background services: replicator, rebalancer, over-replication, cleanup, pending reaper, scrubber, drainer, reconciler
  store/                     # Metadata persistence; store/circuitbreaker.go wraps both engines
    core/                    # Engine-agnostic types, role interfaces, business rules
    postgres/                # Postgres engine (goose migrations, sqlc-generated queries under sqlc/)
    sqlite/                  # SQLite engine
    storetest/               # Generated store mocks + the hand-written permissive store
  backend/                   # The ObjectBackend interface, the S3 implementation, breaker and recovery wrappers
  encryption/                # Envelope encryption: chunked AES-GCM, key providers (config/file/Vault)
  compression/               # Chunked seekable zstd
  provisioning/              # Merging config-declared and stored buckets, users and grants
  observe/                   # Span envelope helpers (span.go)
    audit/                   # Request-id context plumbing + structured audit log helper
    logfmt/                  # Shared slog attribute helpers
    telemetry/               # Prometheus metrics + OTel span helpers
    event/                   # Notification event types (CloudEvents)
  notify/                    # Webhook delivery worker (consumes notification_outbox)
  counter/                   # Per-backend usage counters (local + Redis backends)
  cache/                     # In-memory read-side cache
  breaker/                   # Generic three-state CircuitBreaker (used for DB and backends)
  lifecycle/                 # Service supervisor: restart-on-panic, graceful Stop
  di/                        # samber/do/v2 wiring point (NewInjector + every Provide func)
  debug/                     # Opt-in diagnostic services (flight recorder)
  s3op/                      # The closed set of backend operation names
  internalkey/               # The "bucket/userkey" namespacing convention
  instanceid/                # Stable per-process identifier
  progress/                  # Transport-agnostic progress observer for long passes
  util/
    bitset/                  # Shared bit-flag vocabulary (see Bit-Flag Sets)
    batch/                   # Paged walks, item runners, outcome summaries
    bufpool/                 # Pooled byte buffers for streaming I/O
    humanize/                # Operator-facing units
    ioutilx/                 # io.Reader composition helpers
    materialize/             # One-shot stream to seekable reader
    must/                    # Required-dependency panics for constructors
    syncutil/                # AtomicConfig, TTLCache primitives
    workerpool/              # Bounded-concurrency worker pool
  testutil/testx/            # Generic test helpers
  integration/               # End-to-end tests (build tag `integration`) against MinIO + Postgres testcontainers
```

### Layer Responsibilities

| Layer | Responsibility |
|---|---|
| `cmd/`, `cli/`, `runtime/` | Parse flags, build the DI injector, run and stop top-level services |
| `transport/` | Decode HTTP, authenticate, authorize, dispatch, encode the response. The S3 handlers call the object and multipart managers; admin and UI call `ops` |
| `ops/` | Operations the control plane can request, expressed once for every transport. Returns domain values; knows nothing about HTTP |
| `proxy/` | Routing, admission, failover, write coordination, backend state |
| `worker/` | Background services driven by the lifecycle manager |
| `store/` | Metadata persistence; engine-agnostic rules in `core/`, engines under `postgres/` and `sqlite/` |
| `backend/` | S3 client behind one `ObjectBackend` interface |
| `observe/` | Logging, tracing, metrics, audit |
| `di/` | Single wiring point that registers every provider |

Inner layers must not import outer layers (no `store` importing `transport`,
no `proxy` importing `ops`). One part of this is enforced: the `store-boundary`
depguard rule in `.golangci.yml` refuses an import of `store/postgres` or
`store/sqlite` outside `store`, `di`, `cli` and `cmd`, so feature code reaches
the store only through the role interfaces in `store/core`. The rest is held
by review, and it is the rule that keeps the code testable and the dependency
graph acyclic.

### Layered Read of a Single PUT

```
client -> transport/s3api      (parse, authenticate, authorize)
       -> proxy/object         (admission, encryption/compression, choose a target)
       -> proxy/writepath      (pending intent, claim the target)
       -> backend              (S3 PUT)
       -> store/core           (record the object and promote the intent in one tx)
       -> response back through the layers
```

The arrow direction is strict: `store/core` never calls back into `proxy`.

---

## Dependency Injection

DI uses [samber/do/v2](https://github.com/samber/do/v2) and is centralised
in `internal/di`. Every external dependency, collaborator, worker, and
top-level handler has a `Provide<Name>` function, and `NewInjector` in
`injector.go` registers them all. Store roles are not providers: the opened
engine is registered once as `metadataStore` and aliased into the roles a
consumer resolves directly with `do.MustAs`. Nothing is constructed until it
is asked for.

### Two Foundational Rules

1. **Lazy providers**: a provider returns the dependency it builds; it
   never has side effects beyond construction. The injector calls it on
   the first `do.Invoke` and memoises the result for subsequent calls.
2. **Role interfaces at the boundary**: `di` holds the opened engine as
   the unexported `metadataStore` and passes it into each constructor,
   where it lands in the narrow role composite that consumer declared.
   Nothing outside `di` can name the wide type, so a consumer's
   dependency footprint is whatever roles it wrote down.

### Provider Pattern

Every provider follows the same shape. A provider with more than one
dependency resolves them through the batch `resolver` (`resolve.go`) and
checks the error once:

```go
// ProvideS3Server creates the S3 API server.
func ProvideS3Server(i do.Injector) (*s3api.Server, error) {
    r := newResolver(i)
    cfg := r.Resolve[*config.Config]()
    objects := r.Resolve[*object.Manager]()
    multipartManager := r.Resolve[*multipart.Manager]()
    bucketAuth := r.Resolve[*auth.BucketRegistry]()
    if r.err != nil {
        return nil, r.err
    }
    srv := s3api.NewServer(objects, multipartManager, cfg.Server.MaxObjectSize)
    srv.SetBucketAuth(bucketAuth)
    return srv, nil
}
```

Providers take a `do.Injector`, resolve their own dependencies through it,
and return either the value or an error. They never panic and never spawn
goroutines, and they log only to report a degraded optional dependency. CB
protection lives inside each driver's DBTX/DB chokepoint, so providers do not
wrap stores with per-role decorators.

### Wiring Point

The `internal/di` package is the single wiring point. It is the **only**
package that imports the concrete engine packages (`store/postgres`,
`store/sqlite`, each backend driver, etc.). Providers are split across
focused files by concern:

- `injector.go`  - `NewInjector`, the top-level registration order, and the
                   `do.MustAs` role aliases
- `store.go`     - database, the `metadataStore` union, instance ID
- `backend.go`   - S3 backends, breaker registry, backend runtime, the
                   proxy collaborators, and the optional providers feeding
                   them (encryption, Redis counters, object cache)
- `workers.go`   - background worker providers and the backend state manager
- `lifecycle.go` - lifecycle manager and per-mode service registration
- `transport.go` - S3, ops, admin, UI, CORS, notifier, rate limiter, login throttle
- `provisioning.go` - the provisioning registry and its cross-instance refresh
- `debug.go`     - opt-in diagnostic services
- `services.go`  - lifecycle Runner wrappers (locked-ticker background jobs),
                   with their consumer interfaces in `service_interfaces.go`
- `resolve.go`   - the batch `resolver`
- `optional.go`  - `Optional[T]` and `IsRegistered[T]`
- `wiring.go`    - `WireManager`, the boot-time smoke check that every
                   required worker resolves

New providers go in the file matching their concern, with section boxes
when a single file hosts multiple groups.

### Consuming a Service

Components never hold the injector. A provider resolves what a component
needs and passes it to the constructor, usually as a `Deps` struct:

```go
func ProvideAdminHandler(i do.Injector) (*admin.Handler, error) {
    d, err := resolveAdminHandlerRequiredDeps(i)
    if err != nil {
        return nil, err
    }
    // ... optional deps
    return admin.New(deps), nil
}
```

The only code that holds `do.Injector` is the composition layer around the
injector itself: `runtime`, `reload` and `httpserver`, which resolves the
top-level handlers once when it builds the route mux. Tests build a
component with fakes in its `Deps` rather than registering a fake provider.

### Adding a New Provider

1. Add a `Provide<Name>` function to the `internal/di` file matching its
   concern.
2. Resolve its dependencies with `newResolver(i)` and check `r.err` once.
3. Construct the component, passing what it resolved, and return
   `(value, nil)`.
4. Register it in `NewInjector`. A worker the lifecycle manager runs is
   also added to `registerWorkerServices` in `lifecycle.go`.

### Optional Providers

Some providers register only when a feature is enabled (encryption,
Redis counters, UI, notifications) or only in specific run modes
(the reconciler runs in worker/all mode only). A consumer that may run
without one resolves it with `Optional[T]`, which tells apart "not
registered" (`ResolutionDisabled`) from "registered but failed to build"
(`Failed()`):

```go
recRes := Optional[*worker.Reconciler](i)
if recRes.Failed() {
    slog.WarnContext(ctx, "reconciler resolution failed; admin reconcile endpoint will be inert",
        logfmt.Component("di"), "error", recRes.Err)
}
deps := &admin.Deps{
    // ...
    Reconciler: recRes.Value,
}
```

`IsRegistered[T]` checks presence without building the service. Required
providers still return the resolution error so a genuinely missing
dependency surfaces at boot, not at first use.

### Anti-Patterns

- Constructing a real dependency in a constructor that already has a
  provider. Take it as a parameter and let the provider resolve it.
- Passing a `*slog.Logger` through the constructor argument list.
  Long-lived components hold a private `log *slog.Logger` field set
  inside the constructor body via
  `slog.Default().With(logfmt.Component("name"))`. Metrics use the
  `observe/telemetry` package-level vars directly. Free helper
  functions call `slog.XContext(ctx, ...)` directly. See
  [`docs/contributing/logging.md`](contributing/logging.md).
- Storing the injector as a field. Workers, managers, handlers and
  stores receive their dependencies as constructor args resolved in the
  provider; only the composition layer (`runtime`, `reload`,
  `httpserver`) holds the injector itself.

### Constructor Patterns

**Parameter threshold.** A constructor with **four or more parameters**, or
with two or more same-typed primitives (bool/int/string) whose call-site
order is ambiguous, takes a single `Deps` (dependency bag) or `Config`
(value config) struct instead of a positional list - named fields document
each argument and make transposition impossible. `context.Context` stays
the first positional argument, never inside the struct; three or fewer
distinct-typed parameters stay positional. Pass the struct by pointer when
it is large (`gocritic` flags structs over ~80 bytes), by value otherwise.
A flat `Deps` is the default. Capability sub-structs, grouping fields by
where they come from, are reserved for genuinely large configs - and are
worth a second look before reaching for: the last one grew four sub-structs
that nothing read, which stayed invisible because a config nobody reads still
compiles.

Constructors at the DI/wiring boundary panic on missing required
dependencies via `internal/util/must`. The boundary is the set of
constructors a DI provider or a test fixture builds directly:
every `worker.New*`, every `transport/{s3api,admin,ui}.New*`, and the
`proxy/{backendstate,readpath,multipart,object,writepath,usage,reconcile}` package
constructors that take a `*Deps` or interface bag. Internal helpers that are only called from one
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

**When NOT to use `must.NotNil`.** It is for already-resolved internal
dependencies whose absence is a programmer wiring bug - nothing else. Do
not reach for it on:

- values derived from config or user input (validate in `internal/config.SetDefaultsAndValidate` and return an error);
- network clients, file paths, or external services that can fail at runtime;
- optional or feature-gated dependencies - use `Optional[T]` or an explicit nil-check rather than panicking.

The test: if a nil here could result from anything other than a developer
mis-wiring the program, return an error instead of panicking.

**Tests must satisfy the boundary.** When a test constructs one of
these types directly, it provides real or `gomock`-generated deps for
every required field. Don't loosen production validation to fit lazy
test wiring; widen the test fixture instead. The shared fixtures in
`internal/proxy/proxytest`, `internal/store/storetest`, and the per-package
`newTestHandler*` helpers exist for this purpose. Integration tests build
some `Deps` literals by hand (`admin.Deps` among them), and they sit behind
the `integration` build tag, so after adding a required field run
`go vet -tags integration ./...` - a plain `go vet ./...` never compiles them.

---

## Adding a New Component

Each component type has a fixed checklist; following it keeps the code
shape uniform across contributors.

### Supporting a New Storage Provider

There is one backend implementation, `backend.S3Backend`, built by
`NewS3Backend(ctx, *config.BackendConfig)`; a provider is a configuration
of it, not a new type. Supporting a provider whose S3 dialect differs means
a `BackendConfig` option (`ForcePathStyle`, `DisableChecksum` and
`StripSDKHeaders` exist for GCS and friends), validated in
`internal/config/backends.go` and applied in `internal/backend/s3.go`.

1. Add the option to `BackendConfig` with its default and validation.
2. Apply it where `NewS3Backend` builds the client, with a unit test in
   `internal/backend/s3_test.go`. Integration coverage comes from the
   MinIO testcontainer suite.
3. Document it in `docs/backends.md` (the provider quick-reference) and
   `docs/configuration.md`.

### Adding a Store Method or Role

The mechanical checklist - which interfaces to touch, the compile-time
check, the mocks - is in [`docs/contributing/store.md`](contributing/store.md),
the source of truth for the store. In short: a method goes on a role
interface in `internal/store/core/interfaces.go`, is implemented in both
`internal/store/postgres` and `internal/store/sqlite`, and the mocks are
regenerated with `make generate`. A new role is also embedded in
`di.metadataStore` and in `storetest.MetadataStore`. Three things are easy
to miss:

- `internal/store/storetest/permissive.go` is hand-written. A new method
  that a fixture may pass through needs a stanza there; mockgen does not
  add one.
- `storetest/role_mocks.go` generates per-role mocks from the list in the
  `go:generate` directive in `storetest/interface.go`. A test that needs a
  mock of one role adds the role to that list.
- Postgres queries live in `internal/store/postgres/sqlc/queries/*.sql`.
  `make generate` runs `sqlc generate` against them, using the Postgres
  migrations as the schema. sqlc does not recognize a named parameter
  directly after an operator such as `~` or inside arithmetic: wrap it,
  `~(@mask::bigint)`, or compute the value in Go and pass it whole.

If business logic spans multiple tables or transactions, put the
orchestration in `internal/store/core/<topic>.go` against `TxAdapter` so
both engines share one implementation.

### Changing the Schema

Schema changes are written twice, once per engine, and both engines pin the
version they expect. The engines number their migrations independently:

**PostgreSQL** - add `internal/store/postgres/migrations/000NN_*.sql` (goose
format, `-- +goose Up` / `-- +goose Down`) and bump
`postgres.ExpectedSchemaVersion` to the new number.

**SQLite** - add `internal/store/sqlite/migrations/00NN_*.sql`, apply the
same change to `internal/store/sqlite/schema.sql`, and bump both
`sqlite.expectedSchemaVersion` and the `INSERT INTO schema_version` value at
the bottom of `schema.sql`. Add a step undoing the migration to
`schemaRewindSteps` in `sqlite/store_test.go`, so the upgrade tests can
rewind a fresh database to the previous version and migrate it forward.

Both SQLite files are needed because they serve different databases.
`schema.sql` is the baseline a fresh database is created from; the numbered
migration is what an existing database is upgraded through. A change made in
only one of them means a new install and an upgraded install disagree, which
is the kind of drift nothing notices until an operator hits it.

Data repair belongs in a migration too, not in a startup pass. A one-off
fixup that runs on every boot is unversioned, unrecorded, and invisible to
the operator whose data it rewrites.

A missed version bump is caught by unit tests on both engines
(`TestMigrations_AreNumberedAndReachExpectedVersion`). Whether the migration
works against a real database is only covered by the integration suite,
which is behind `//go:build integration`: verify schema work with
`make integration-test`.

### Adding a New Worker

1. Implement the worker in `internal/worker/<name>.go`. It takes the
   shared worker interfaces it needs from `ops_runtime.go` (`Ops`,
   `FleetOps`, `Placement`) plus a `<Name>Store` role composite naming
   the store roles it uses (`RebalancerStore`, `ScrubberStore`).
2. The worker exposes `Run(ctx)` (a long-lived loop) or a per-tick
   `Process<X>(ctx) batch.Summary` - lifetime determines which.
3. Add a `Provide<Name>` provider in `internal/di/workers.go` and
   register it in `NewInjector`.
4. Register it with the lifecycle manager in `registerWorkerServices`
   (`internal/di/lifecycle.go`). Periodic workers run on a ticker under
   an advisory lock.
5. Add metrics to the domain's file under `internal/observe/telemetry/`
   (`metrics_workers.go` for the shared per-worker families).

### Adding an Admin API Endpoint

1. Add the route to the table in
   `internal/transport/admin/handler_routes.go`, declaring its `Kind`
   (the resource the permission is checked against) and `Perm`.
   Authentication and authorization run from the table; handlers never
   check permissions themselves.
2. Put the work in `internal/ops` when it is an operation the dashboard
   could also request, and keep the handler to decoding, calling, and
   encoding.
3. Audit-log every state-changing request through `audit.Log`.
4. Run `make openapi` to regenerate `docs/openapi.yaml`.
   `TestOpenAPISpec_MatchesRouteTable` fails on a stale spec, and the
   `openapi-breaking` CI job flags a breaking change to an existing route.
5. A new admin permission is a new bit on `core.PermissionSet`: add it to
   the bit list and the vocabulary, include it in `PermAdminAll`, and
   update the permission table in `docs/admin-api.md` and the bit table in
   `web/content/diagrams/access-control.md`.
6. Add the CLI verb in `internal/cli/adminctl` and, if the TUI offers it,
   the client method in `internal/cli/adminclient`.

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
4. Document the metric in `docs/monitoring.md`.

---

## Error Handling

### S3 Errors

Use the `S3Error` type, declared with its sentinels in `internal/store/core/errors.go`, for errors that map to S3 HTTP responses:

```go
ErrObjectNotFound = &S3Error{
    StatusCode: http.StatusNotFound,
    Code:       "NoSuchKey",
    Message:    "object not found",
}
```

The S3 handlers use `writeStorageError(w, err, fallbackMsg)` to turn an error into an XML response. A streaming-signature failure takes precedence (`403 SignatureDoesNotMatch`), then any `*S3Error` in the chain; anything else falls back to `502 InternalError`.

### Error Classification

`(*infra.BackendRuntime).ClassifyWriteError(span, op, err)` translates a store error on the write path into an S3 error and marks the span:

- `ErrDBUnavailable` (circuit breaker open) -> `ErrServiceUnavailable`, `503`, and a degraded-write rejection is counted
- `ErrNoSpaceAvailable` -> `ErrInsufficientStorage`, `507`
- Anything else is recorded on the span and returned unchanged

### Background Operation Errors

Background workers (rebalancer, replicator, cleanup) log errors and continue rather than crashing. Individual item failures are logged with `slog.Warn` and skipped; the batch proceeds with remaining items.

---

## Logging and Audit

All logs are structured `log/slog` JSON. The handler chain (JSON, then
the error handler, then `telemetry.TraceHandler`) is built in
`startObservability` in `internal/runtime/observability.go`. The full operational
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
    deps      Ops
    placement Placement
    store     core.PendingStore
    log       *slog.Logger
    // ...
}

func NewPendingReaper(deps PendingReaperDeps) *PendingReaper {
    must.NotNil("Ops", deps.Ops)
    // ...
    return &PendingReaper{
        deps:      deps.Ops,
        placement: deps.Placement,
        store:     deps.Store,
        log:       slog.Default().With(logfmt.Component("pending_reaper")),
        // ...
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

- HTTP layer (`s3.PutObject`, `s3.GetObject`, etc.) - method, path,
  bucket, status, duration.
- Storage layer (`storage.PutObject`, `storage.GetObject`, etc.) -
  key, backend, size.

Both also carry `user`, the identity the request authenticated as.
`audit.Log` reads it and the request id from context, so no call site
passes either and the storage-layer entry carries the same pair as the
HTTP-layer one without being handed an identity. An entry with no
`user` authenticated no caller: a rejected request, or a background
worker acting on its own.

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
`audit.RequestID`, and the acting user via `audit.WithUser` /
`audit.User`. `audit.Log` reads both from context.

- S3 API requests: extracted from `X-Request-Id` header or generated,
  set on context before auth. The S3 server also sets it as the
  `s3o.request_id` attribute on the request's server span.
- Operator-driven worker cycles (`runOpsCycle` in `worker/cycle.go`)
  mint a fresh ID with `audit.NewID()`. Periodic ticks do not: a tick is
  not an operator action.
- `trace_id` and `span_id` are automatically injected into JSON log
  output by `telemetry.TraceHandler` for any log call with an active
  span in context - use `*Context` slog variants
  (`InfoContext`/`WarnContext`/`ErrorContext`).

### Log Levels

| Level | Use |
|-------|-----|
| `slog.LevelDebug` | Verbose state; off by default. |
| `slog.LevelInfo`  | Lifecycle (startup/shutdown), terminal success of a notable operation, audit entries. |
| `slog.LevelWarn`  | Recoverable failure - caller proceeds, operator should know (failover, degraded mode, retry-able errors). |
| `slog.LevelError` | Unrecoverable failure of an operation - request fails, background tick aborts, integrity violation. |

---

## Tracing

OpenTelemetry tracing is wired in `internal/observe/telemetry`, with the
span helpers in `internal/observe`. Every S3 request, manager call,
backend call, and worker cycle produces a span. Postgres queries are
traced by `otelpgx` at the driver; SQLite is not traced.

### Wrapping a Unit of Work

Prefer the `observe.Run` envelope over a hand-opened span. It ends the
span, sets the status on success and failure, records the error, and
hands the elapsed time to an optional metrics recorder, so none of that
is repeated at the call site:

```go
return observe.Run(ctx,
    observe.Client(spanPrefix+operation, attrs, b.recordOperation),
    func(ctx context.Context) (string, error) {
        // ...
    })
```

`observe.Client` opens a client span (a call leaving the process, such as
a backend request) and `observe.Internal` an internal one. `RunErr` is the
form for work that returns only an error, and `RunValue` for work that
cannot fail. Worker cycles go through `runOpsCycle` / `runTickCycle` in
`worker/cycle.go`, which wrap the same envelope.

### Starting a Span by Hand

When the span has to stay open across code that does not fit one closure,
use `telemetry.StartSpan` (or `StartServerSpan` at an HTTP entry point)
rather than the OTel API directly, and defer `span.End()` on the next line
so an early return cannot leave it open:

```go
ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation.String(),
    telemetry.AttrObjectKey.String(key),
)
defer span.End()
```

### Span Naming

Span names are stable strings (no per-request data) so traces aggregate
cleanly:

| Layer | Form | Example |
|---|---|---|
| Manager | `Manager <Op>` | `Manager PutObject`, `Manager GetObject` |
| Backend | `Backend <Op>` | `Backend PutObject`, `Backend GetObject` |
| Worker cycle | `<Op>` | `ProcessCleanupQueue`, `ProcessPendingQueue` |

The `Manager ` and `Backend ` prefixes are package constants
(`managerSpanPrefix` in `object`, `spanPrefix` in `backend`, `readpath`
and `multipart`), so a call site never spells the prefix itself.

### Attributes

Standard attribute keys are the `Attr*` variables in
`internal/observe/telemetry/tracing.go`, all under the `s3o.` namespace,
so every span uses the same string. Add new keys there rather than
inlining strings at call sites.

```go
span.SetAttributes(
    telemetry.AttrBackendName.String(backendName),
    telemetry.AttrObjectSize.Int64(size),
)
```

High-cardinality values (object keys, request IDs as attributes) are
allowed only on leaf spans. Do not put object keys on long-running
worker spans because that explodes trace storage cardinality.

### Recording Errors

Inside `observe.Run` errors are recorded for you. On a hand-opened span,
use `observe.RecordSpanError(span, err)` for an error, or
`observe.MarkSpanError(span, msg)` to fail the span with a message when
the error itself is translated away (as `ClassifyWriteError` does). Both
set the error status so trace UIs flag the span. Do not log the error
inside the span block - the audit/slog log line already carries the
error and trace correlation links the two.

```go
if err != nil {
    observe.RecordSpanError(span, err)
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
Existing domains: `audit`, `auth`, `breaker`, `cache`, `cleanup`,
`compression`, `cors`, `encryption`, `meta`, `quota`, `rebalance`,
`replication`, `request`, `workers`; the per-worker gauge publisher is in
`worker_gauges.go`. Add a new file
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
- Histograms on the request path or a backend call declare explicit
  buckets (a literal slice, `LinearBuckets` or `ExponentialBuckets`) that
  span the expected p50 to p99.9 range with at most 10-15 buckets.
  `prometheus.DefBuckets` is acceptable only for internal timings whose
  range it already fits (the breaker and metadata-query histograms).

### Updating a Metric

Counters use `Inc` or `Add`. Gauges use `Set`. Histograms use `Observe`.
Always pass label values in the order they appear in the metric
declaration; Prometheus does not protect against argument-order swaps.

```go
telemetry.CleanupQueueEnqueuedTotal.WithLabelValues(reason).Inc()
telemetry.CleanupDLQDepth.Set(float64(depth))
telemetry.RequestDuration.WithLabelValues(method).Observe(elapsed.Seconds())
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
- Shared test doubles live in `-test` suffixed packages beside what they stand in for: `store/storetest` (store mocks and the permissive store), `backend/backendtest`, `ops/opstest`, `proxy/proxytest`
- Test names follow `TestFunctionName_Scenario` convention
- `make test` is the gate: `go test -race -cover ./...`. A bare `go test ./...` (`make test-fast`) skips the race detector
- `make lint` runs golangci-lint, the same check CI runs; `make fmt` applies the formatting it enforces

### Integration Tests

- Located in `internal/integration/`, plus `*_integration_test.go` files beside the engine code in `internal/store/postgres`
- Gated behind the `integration` build tag; run with `make integration-test`
- Run in-process with real MinIO and PostgreSQL containers. The Postgres image is glibc-based on purpose: musl collates as byte order, which would hide collation bugs
- Cover end-to-end flows: CRUD, quota enforcement, multipart, replication, circuit breaker
- Because `go vet ./...` and `make test` never compile them, run `go vet -tags integration ./...` after changing an exported API

### Benchmarks

- Benchmark files use the `_bench_test.go` suffix: `auth_bench_test.go`, `helpers_bench_test.go`
- Integration benchmarks live in `internal/integration/bench_test.go` (gated behind the `integration` build tag)
- Run benchmarks: `make bench`, `make bench-auth`, `make bench-crypto`, etc. Override duration with `BENCH_TIME=5s make bench`
- Use `b.Loop()` (Go 1.24+) for all benchmark loops - not `for i := 0; i < b.N; i++`
- Use `b.SetBytes()` for throughput benchmarks so Go reports MB/s
- Use `b.ResetTimer()` after setup/population steps
- Use `b.RunParallel` with `pb.Next()` for concurrent benchmarks (`b.Loop()` cannot replace `pb.Next()` inside `RunParallel` callbacks)
- Pre-compute keys and test data outside the measured loop - `fmt.Sprintf` inside a benchmark loop measures string formatting, not the code under test
- Benchmark names follow `BenchmarkFunctionName/variant` with sub-benchmarks for different input sizes

### Fuzz Tests

- Fuzz test files use the `_fuzz_test.go` suffix: `auth_fuzz_test.go`, `xml_fuzz_test.go`
- Run fuzz targets: `make fuzz` (default 30s per target). Override with `FUZZ_TIME=5m make fuzz` for deeper exploration
- The `fuzz` workflow runs every target nightly for 2 minutes each; pull-request CI does not fuzz
- Seed the corpus with valid inputs, edge cases, and adversarial inputs via `f.Add()`
- Fuzz callbacks must never panic - verify invariants with `t.Errorf` instead
- Match the production code path - use `xml.NewDecoder().Decode` not `xml.Unmarshal` when production uses streaming decoders
- **Differential oracles:** When two implementations exist for the same operation (e.g., `parseSigV4Fields` and `parseSigV4FieldsDirect`), fuzz both and assert they agree on every input
- **Structural invariants:** Assert properties of the output that must always hold (e.g., "canonical request has exactly N newlines", "bucket never contains a slash", "nonce is always 12 bytes")
- Do not assert application-level validation at the parsing layer - XML decoders accept negative integers; the handler layer validates range constraints

### Test Patterns

- **Generated mocks** (`mockgen`) are the preferred approach for new tests - they stay in sync with interfaces automatically
- **`backendtest.FailableBackend`** wraps a backend to inject errors for failover and circuit breaker testing
- Test assertions use standard `testing.T` methods, not external assertion libraries

### Coverage

Coverage is reported to SonarCloud from the unit, integration and Terraform
provider runs. New and changed code in a pull request should land above 80%;
measure it before pushing, since the Postgres bindings and transport wiring
are where gaps usually hide.

Exclusions are path-based, in `sonar.coverage.exclusions` in
`sonar-project.properties`: `cmd/**`, generated code (`sqlc`), test-double
packages, and a few files that need an external service (`encryption/vault.go`).
There is no inline ignore directive. Keep `cmd/` to dispatch and `os.Exit`,
and put anything worth testing in a function under `internal/` that returns
an error. Do not add a path to the exclusions for code that needs a
database, S3 backend, or Redis - integration tests run with testcontainers
and contribute coverage.

---

## Code Style

### Character Rules

**ALWAYS USE:**
- ASCII dash: `-` (hyphen-minus, U+002D)
- Standard ASCII characters only

**NEVER USE:**
- Unicode em-dash (U+2014)
- Unicode en-dash (U+2013)
- Unicode arrows (U+2192); write `->`
- Unicode box-drawing (U+2500)
- Equals signs for dividers

The rule covers everything the project publishes, not only Go source:
docs, workflow files, commit messages, PR descriptions and issues. No
lint check enforces it, so it falls to review.

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

The `.version` file in the repository root controls the version baked into binaries, Docker images, and Debian packages. CI's `version-check` job fails a PR that changes a `.go` or `.sql` file, or `sqlc.yaml`, without bumping it. Docs-only and workflow-only changes need no bump. Bump from the version on `main`, once per PR.

**Patch bump** (v0.X.Y -> v0.X.Y+1) for:
- Bug fixes
- Refactoring (no behavior change)
- Test improvements
- Dependency updates
- Cleanup and code style changes

**Minor bump** (v0.X.0 -> v0.X+1.0) for:
- New features (new config fields, new API endpoints, new CLI commands)
- Breaking changes (required config fields, changed defaults, removed options)
- Schema migrations that add tables or columns

`CHANGELOG.md` is generated from commit messages by git-cliff at release
(`cliff.toml`); never edit it by hand. That is why commit subjects follow
[Conventional Commits](https://www.conventionalcommits.org/) (`feat:`,
`fix:`, `docs:`, `refactor:`, `test:`). Each PR is one commit and is
squash-merged, so the PR's single commit is the changelog entry.

---

## Documentation Updates

When a PR changes config fields, API behavior, or deployment requirements, update all affected documentation in the same PR:

- `packaging/config.yaml` - sample config
- `config.yaml` - local dev config
- `README.md` - config reference section
- `docs/*.md` - per-topic operational documentation (see `docs/admin-guide.md` for the index)
- `docs/security-hardening.md` - if security-relevant
- `docs/disaster-recovery.md` - if it affects failure modes
- `docs/version-migration.md` - an entry under Version History for any change an operator upgrading has to know about
- `docs/openapi.yaml` - regenerated with `make openapi`, never edited by hand
- `web/content/guides/*.md` - deployment guides
- `web/content/diagrams/*.md` - the interactive diagrams. They restate behavior (permission bits, write routing, state machines) and rot silently, so when behavior changes, read the whole affected diagram against the code
- `deploy/` - Nomad, Kubernetes, and Helm manifests

`web/content/godoc/` is generated by `make web-godoc` from the package
docs, so it follows the code rather than being edited.

Search across all docs with `grep -rn 'field_name' README.md docs/ web/content/ packaging/ deploy/` before committing to catch every reference.

Doc comments carry information or are left out: `make doc-stub-check`
(run in CI) fails on a tautological stub such as `// Foo foo.`.

---

## Branch Naming

When a branch corresponds to a GitHub issue, use this format:

```
GH_ISSUE_<issue number>-<description of topic>
```

Examples:
- `GH_ISSUE_251-worker-pool-parallelism`
- `GH_ISSUE_42-multipart-upload-cleanup`

For branches without a linked issue, use a short kebab-case description of the topic, prefixed with the change type where it helps (`fix-<description>`), and link or file an issue in the PR body.

---

## Quick Reference

| Comment Type | Divider | Spacing After | Use Case |
|-------------|--------|---------------|----------|
| File header | 79 dashes | 1 blank line | Top of every hand-written `.go` file |
| Major section | 73 dashes | 1 blank line | Major divisions (types, API, internals) |
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

package worker

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Replicate finds under-replicated objects and creates additional copies to
// reach the target replication factor. Returns the number of copies created.
func (r *Replicator) Replicate(ctx context.Context, cfg config.ReplicationConfig) (int, error) {
    start := time.Now()
    ctx = audit.WithRequestID(ctx, audit.NewID())

    // Find under-replicated objects
    locations, err := r.store.GetUnderReplicatedObjects(ctx, cfg.Factor, cfg.BatchSize)
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

package worker

// Let's create the replicate function
func (r *Replicator) Replicate(ctx context.Context, cfg config.ReplicationConfig) (int, error) {
    // get the start time
    start := time.Now()
    // ...
}
```

---

**Remember:** Comments should explain *why* decisions were made, not *what* the code does. The code itself should be clear enough to understand *what* it does.
