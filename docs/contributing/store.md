# Metadata Store Conventions

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
| `internal/store/core` | Role interfaces (`ObjectStore`, `QuotaStore`, `ReplicationStore`, ...) and the composite `MetadataStore` that embeds them. Shared error sentinels and value types live here. |
| `internal/store/sqlite` | Concrete `*Store` for SQLite. Asserts `core.MetadataStore` and `core.Runner` at compile time. |
| `internal/store/postgres` | Concrete `*Store` for PostgreSQL. Same compile-time assertions. |
| `internal/store/storetest` | Re-exports the composite as `storetest.MetadataStore` and hosts the mockgen-generated `MockMetadataStore`. |
| `internal/store/circuitbreaker.go` | `NewDatabaseBreaker` and the error filter; the breaker is installed at the SQL driver chokepoint so every store call passes through it. |

Worker / handler / proxy packages **never** import `internal/store/sqlite`
or `internal/store/postgres` directly. They take a narrow role interface
through DI and resolve to the concrete implementation only via
`internal/di/store.go`. The `depguard` rule
`store-boundary` in `.golangci.yml` enforces this mechanically: a
non-`internal/di`, non-`internal/cli`, non-test file that imports a
concrete store driver fails CI with a pointer back to this page.

---

## Adding a method to an existing role interface

1. Add the method to the role interface in `internal/store/core/interfaces.go`.
2. Implement it on both `internal/store/sqlite/*.go` and
   `internal/store/postgres/*.go`. The compile-time assertions
   `var _ core.MetadataStore = (*Store)(nil)` in
   `sqlite/store.go` and `postgres/store.go` fail the build if either
   implementation is missing.
3. From the repo root run `go generate ./internal/store/storetest/...`
   to regenerate the gomock mock. The composite `MetadataStore`
   re-exported in `storetest/interface.go` is the single mock source of
   truth; tests that need imperative control wrap it via
   `testutil.NewMockStore`.
4. The driver-level circuit breaker installed by `NewDatabaseBreaker`
   wraps the `database/sql` DBTX surface, so the new method picks up
   `core.ErrDBUnavailable` semantics automatically. No per-method
   decorator to update.

---

## Adding a new role interface

1. Declare the interface in `internal/store/core/interfaces.go`. Keep it
   to the operations one consumer actually performs; resist the urge
   to bundle unrelated methods.
2. Embed the new interface in the `MetadataStore` composite in the same
   file so both concrete stores transitively implement it.
3. Re-export the same interface in
   `internal/store/storetest/interface.go` so mockgen picks up the new
   methods (the regenerated mock satisfies both the storetest and core
   composites).
4. Hand the role interface to consumers as a constructor argument; do
   not pass the wide `core.MetadataStore` through to a worker that only
   needs three methods.

---

## What is *not* needed

- **Per-method circuit-breaker decorators**: the breaker lives at the
  SQL driver chokepoint, so adding a method needs no decorator update.
  See `internal/store/circuitbreaker.go` and the `isDBError` filter for
  the application-vs-DB error classification.
- **Per-role compile-time assertions**: the composite assertion on
  `*Store` covers every embedded role transitively. Adding
  `var _ core.ObjectStore = (*Store)(nil)` would be redundant.
- **Hand-written mocks**: the gomock-generated `MockMetadataStore` is
  the only mock the test suite uses. New methods appear there
  automatically after `go generate`.

---

## Mechanical checks summary

| Check | Where | What it catches |
|---|---|---|
| Compile-time `var _ core.MetadataStore = (*Store)(nil)` | `sqlite/store.go`, `postgres/store.go` | Either store implementation missing a method declared on the composite. |
| Compile-time `var _ core.TxAdapter = ...` | `sqlite/adapter.go`, `postgres/adapter.go` | Tx-scoped role implementations missing methods. |
| `go generate ./internal/store/storetest/...` | `storetest/interface.go` | Out-of-date mock after an interface change. |
| Driver-level circuit breaker | `internal/store/circuitbreaker.go` | New store methods automatically get CB semantics. |
| `depguard` rule `store-boundary` | `.golangci.yml` | Non-DI / non-test code importing concrete store packages. |
| Unit + integration tests | `internal/store/{sqlite,postgres}` | Behavioural regressions and SQL drift. |
