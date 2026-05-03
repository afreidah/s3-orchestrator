// -------------------------------------------------------------------------------
// Core TxAdapter and Reader - Per-Engine Seam
//
// Author: Alex Freidah
//
// Declares the per-feature transactional adapters and the read-only Reader.
// Engine packages (postgres, sqlite) provide concrete implementations that
// translate between sqlc-generated row types and the canonical core domain
// types. Engine-agnostic business logic in this package never touches a
// driver-typed value - it operates exclusively on these interfaces.
//
// Method names are engine-neutral so a Postgres-flavored mechanism (FOR
// UPDATE row locks) and a SQLite-flavored equivalent (single-writer
// existence probe) can satisfy the same contract without leaking either
// dialect into core.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"time"
)

// -------------------------------------------------------------------------
// PARENT TX ADAPTER
// -------------------------------------------------------------------------

// TxAdapter is the per-engine transactional seam. A core operation
// receives one of these from Runner.WithTx, runs business logic against
// it, and never touches a driver-specific transaction directly. The
// parent embeds the per-feature adapters so callers depend only on the
// narrowest interface that fits their needs.
type TxAdapter interface {
	PendingTxAdapter
	ObjectsTxAdapter
	CleanupTxAdapter
	QuotaTxAdapter

	// AcquireKeyLock takes a transaction-scoped lock keyed by the
	// object key. Postgres uses pg_advisory_xact_lock derived from a
	// hash of the key; SQLite no-ops because the engine serializes
	// writers and the in-tx existence probe provides the same
	// guarantee.
	AcquireKeyLock(ctx context.Context, objectKey string) error
}

// -------------------------------------------------------------------------
// PER-FEATURE TX ADAPTERS
// -------------------------------------------------------------------------

// PendingTxAdapter exposes the transactional operations on the
// pending_objects table.
type PendingTxAdapter interface {
	// ClaimPending returns true if the pending row was successfully
	// claimed for promotion, false if it has already been resolved by
	// another worker. Postgres uses SELECT FOR UPDATE; SQLite uses an
	// existence probe inside the writer-serialized txn - same
	// guarantee.
	ClaimPending(ctx context.Context, intentID string) (claimed bool, err error)

	InsertPending(ctx context.Context, p *PendingObject) error
	DeletePending(ctx context.Context, intentID string) error
	DeletePendingByBackend(ctx context.Context, backendName string) error
}

// KeyedExistingCopy is an ExistingCopy that also carries the object_key
// so batch operations can group rows by key.
type KeyedExistingCopy struct {
	ObjectKey   string
	BackendName string
	SizeBytes   int64
}

// ObjectsTxAdapter exposes the transactional operations on the
// object_locations table.
type ObjectsTxAdapter interface {
	GetExistingCopiesForUpdate(ctx context.Context, objectKey string) ([]ExistingCopy, error)
	InsertObjectLocation(ctx context.Context, loc *ObjectLocation) error
	DeleteObjectCopies(ctx context.Context, objectKey string) error

	// GetCopiesForKeysForUpdate returns every (key, backend, size) row
	// matching any key in the supplied list, locked FOR UPDATE so the
	// same transaction can delete the rows and decrement quotas
	// atomically. Used by the batch-delete path.
	GetCopiesForKeysForUpdate(ctx context.Context, keys []string) ([]KeyedExistingCopy, error)

	// DeleteObjectsByKeys removes every object_locations row whose key
	// is in the supplied list. Caller must have already locked the
	// rows via GetCopiesForKeysForUpdate.
	DeleteObjectsByKeys(ctx context.Context, keys []string) error

	// CheckObjectExistsOnBackend reports whether the object_locations
	// table holds a row for (key, backend).
	CheckObjectExistsOnBackend(ctx context.Context, objectKey, backend string) (bool, error)

	// LockObjectOnBackend takes a FOR UPDATE lock on the
	// (key, backend) row and returns its full payload. Returns
	// (nil, false, nil) when the row is gone - benign race the caller
	// treats as nothing to act on.
	LockObjectOnBackend(ctx context.Context, objectKey, backend string) (loc *ObjectLocation, ok bool, err error)

	// DeleteObjectFromBackend removes the single (key, backend)
	// object_locations row.
	DeleteObjectFromBackend(ctx context.Context, objectKey, backend string) error

	// InsertObjectLocationIfNotExists is the import-side INSERT that
	// preserves an existing row. Returns true if a row was inserted.
	InsertObjectLocationIfNotExists(ctx context.Context, loc *ObjectLocation) (inserted bool, err error)

	// InsertReplicaConditional inserts a replica row only if the
	// source copy still exists. Returns the size_bytes the row was
	// inserted with (read from the source row inside the same
	// statement) and inserted=true on success, or (0, false, nil)
	// when the source copy is gone or the target already has a copy.
	// Callers use the returned size for IncrementBackendQuota so
	// object_locations.size_bytes and backend_quotas.bytes_used always
	// agree.
	InsertReplicaConditional(ctx context.Context, objectKey, targetBackend, sourceBackend string) (size int64, inserted bool, err error)
}

// CleanupTxAdapter exposes the transactional operations on the
// cleanup_queue table needed by core orchestration. Background-worker
// helpers that already live entirely on a single transaction (Enqueue,
// Retry, Complete) stay on the read/write path through CleanupStore.
type CleanupTxAdapter interface {
	// SumAndDeleteCleanupQueueRows deletes every cleanup_queue row for
	// the given (objectKey, backend) pair and returns the count and
	// total size of the deleted rows.
	SumAndDeleteCleanupQueueRows(ctx context.Context, objectKey, backend string) (deleted int64, totalBytes int64, err error)
}

// QuotaTxAdapter exposes the transactional operations on the
// backend_quotas table.
type QuotaTxAdapter interface {
	// IncrementBackendQuota credits delta bytes to the backend's
	// quota and returns ErrNoSpaceAvailable when the row reports zero
	// rows updated (quota would be exceeded).
	IncrementBackendQuota(ctx context.Context, backendName string, delta int64) error

	// DecrementBackendQuota debits delta bytes from the backend's
	// quota.
	DecrementBackendQuota(ctx context.Context, backendName string, delta int64) error

	// DecrementOrphanBytes debits delta bytes from the backend's
	// orphan_bytes counter, clamped at zero.
	DecrementOrphanBytes(ctx context.Context, backendName string, delta int64) error
}

// -------------------------------------------------------------------------
// READ-ONLY ADAPTER
// -------------------------------------------------------------------------

// Reader exposes the engine's read-only operations - those that do not
// require a transaction or row-level lock. Engine packages return one
// alongside the Runner so background services can issue queries
// without paying the cost of a transaction.
type Reader interface {
	// Pending reads
	GetStalePending(ctx context.Context, olderThan time.Time, limit int) ([]PendingObject, error)
	CountPending(ctx context.Context) (int64, error)

	// Object listing reads
	GetAllObjectLocations(ctx context.Context, key string) ([]ObjectLocation, error)
	ListObjectsByBackend(ctx context.Context, backendName string, limit int) ([]ObjectLocation, error)
	ListObjectsByBackendKeyAsc(ctx context.Context, backendName, afterKey string, limit int) ([]ObjectLocation, error)
	ListObjectsByPrefix(ctx context.Context, prefix, startAfter string, maxKeys int) ([]ObjectLocation, error)
	ListExpiredObjects(ctx context.Context, prefix string, cutoff time.Time, limit int) ([]ObjectLocation, error)

	// Cleanup queue reads
	GetPendingCleanups(ctx context.Context, limit int) ([]CleanupItem, error)
	CleanupQueueDepth(ctx context.Context) (int64, error)
}
