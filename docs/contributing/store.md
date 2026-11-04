---
description: "Metadata store conventions: the composite interface, the narrow role interfaces consumers depend on, and the checks keeping them aligned."
title: "Metadata Store Conventions"
---

The metadata store is split into a composite interface plus a set of
narrow role interfaces. Consumers depend on the narrowest role they need;
the producer-side composite exists to wire one implementation through
DI and to back a single generated mock for tests. This page documents
the mechanical checks that keep that architecture consistent so adding
a store method does not require touching a long checklist.

---

## Layering

| Package | Role |
|---|---|
| `internal/store/core` | Role interfaces (`ObjectStore`, `QuotaStore`, `ReplicationStore`, ...). Shared error sentinels and value types live here. There is deliberately no composite of them: see below. |
| `internal/store/engine` | `engine.Store`, the one composite of every role. Only the sites that hold a whole engine import it. |
| `internal/store/sqlite` | Concrete `*Store` for SQLite. |
| `internal/store/postgres` | Concrete `*Store` for PostgreSQL. |
| `internal/store/storetest` | Names the mock target `storetest.MetadataStore` and hosts the mockgen-generated `MockMetadataStore`, plus per-role mocks (`MockObjectStore`, `MockCleanupStore`, ...) in `role_mocks.go`. |
| `internal/di` | Declares the unexported `metadataStore`, which holds an opened engine before splitting it into roles. |
| `internal/store/open.go` | `Open` returns either engine as `engine.Store`, which is the compile-time check that both implement every role. `Prepare` runs the boot steps the server and the sync command share. |
| `internal/store/circuitbreaker.go` | `NewDatabaseBreaker` and the error filter; the breaker is installed at the SQL driver chokepoint so every store call passes through it. |

Worker / handler / proxy packages **never** import `internal/store/sqlite`
or `internal/store/postgres` directly. They take a narrow role interface
through DI and resolve to the concrete implementation only via
`internal/di/store.go`. The `depguard` rule
`store-boundary` in `.golangci.yml` enforces this mechanically: a file
outside `internal/store`, `internal/di`, `internal/cli` and `cmd` that is
not a test and imports a concrete store driver or `internal/store/engine`
fails CI with a pointer back to this page.

---

## Adding a method to an existing role interface

1. Add the method to the role interface in `internal/store/core/interfaces.go`.
2. Implement it on both `internal/store/sqlite/*.go` and
   `internal/store/postgres/*.go`. `Open` in `internal/store/open.go`
   returns both engines as `engine.Store`, so the build fails if either
   implementation is missing, naming the method that is absent.
3. From the repo root run `go generate ./internal/store/storetest/...`
   to regenerate the gomock mocks: `MockMetadataStore` for the composite
   `storetest.MetadataStore`, and the per-role mocks in `role_mocks.go`. A
   new role that needs its own mock must be added to that second
   `go:generate` list. `storetest/permissive.go` is
   hand-written: if a fixture may pass through the new method, add a stanza
   for it there, because mockgen does not.
4. The driver-level circuit breaker installed by `NewDatabaseBreaker`
   wraps the `database/sql` DBTX surface, so the new method picks up
   `core.ErrDBUnavailable` semantics automatically. No per-method
   decorator to update.

---

## Adding a new role interface

1. Declare the interface in `internal/store/core/interfaces.go`. Keep it
   to the operations one consumer actually performs; resist the urge
   to bundle unrelated methods.
2. Embed the role in `engine.Store` in `internal/store/engine/engine.go`,
   which both engines are returned as, so the build fails until both
   implement it. DI's `metadataStore` and the mock target embed that
   union, so mockgen picks up the new methods.
3. Hand the role interface to consumers as a constructor argument. There
   is no wide type to pass by accident: depguard refuses `engine.Store`
   outside the store, DI and CLI packages.

---

## What is *not* needed

- **Per-method circuit-breaker decorators**: the breaker lives at the
  SQL driver chokepoint, so adding a method needs no decorator update.
  See `internal/store/circuitbreaker.go` for which errors count as the
  database being unavailable, and each driver's `RequestError` for the
  errors it answers about one request.
- **Per-role compile-time assertions**: `Open` returning each
  engine as `engine.Store` covers every role. Adding
  `var _ core.ObjectStore = (*Store)(nil)` would be redundant.
- **Hand-written mocks**: the gomock-generated `MockMetadataStore` and
  per-role mocks are what the test suite uses. New methods appear there
  automatically after `go generate`.

---

## Mechanical checks summary

| Check | Where | What it catches |
|---|---|---|
| `Open` returning `engine.Store` | `internal/store/open.go` | Either store implementation missing a method the role set requires. |
| Compile-time `var _ core.TxAdapter = ...` | `sqlite/adapter.go`, `postgres/adapter.go` | Tx-scoped role implementations missing methods. |
| `go generate ./internal/store/storetest/...` | `storetest/interface.go` | Out-of-date mock after an interface change. |
| Driver-level circuit breaker | `internal/store/circuitbreaker.go` | New store methods automatically get CB semantics. |
| `depguard` rule `store-boundary` | `.golangci.yml` | Non-DI / non-test code importing concrete store packages. |
| Unit + integration tests | `internal/store/{sqlite,postgres}` | Behavioural regressions and SQL drift. |
