// -------------------------------------------------------------------------------
// Core TxAdapter - Per-Engine Seam
//
// Author: Alex Freidah
//
// Declares the per-feature transactional adapters.
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

	DeletePending(ctx context.Context, intentID string) error
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

	// GetCleanupQueueRow returns the full payload of a single
	// cleanup_queue row by id. Used inside MoveCleanupToDLQ so the row
	// contents (key, backend, size, attempts, created_at, last_error)
	// can be copied into the DLQ insert without a separate round trip.
	GetCleanupQueueRow(ctx context.Context, id int64) (CleanupQueueRow, error)

	// InsertCleanupDLQ inserts the supplied row into cleanup_dlq. The
	// original_id retains the queue row's id for forensic correlation;
	// first_enqueued_at carries the original created_at so the DLQ
	// entry remembers how long the cleanup was outstanding. Takes a
	// pointer so the 112-byte row payload travels by reference.
	InsertCleanupDLQ(ctx context.Context, row *CleanupQueueRow) error

	// DeleteCleanupItem removes a cleanup_queue row by id. Used inside
	// MoveCleanupToDLQ so the queue->DLQ move is atomic.
	DeleteCleanupItem(ctx context.Context, id int64) error
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

	// AllBackendBytesUsed returns the current bytes_used counter for every
	// backend_quotas row, keyed by backend name. Used by usage
	// reconciliation to diff the stored counter against the ledger truth.
	AllBackendBytesUsed(ctx context.Context) (map[string]int64, error)

	// SumObjectSizesByBackend returns the authoritative byte total per
	// backend from object_locations (the ledger), keyed by backend name.
	// Backends with no rows are absent from the map (treated as zero).
	SumObjectSizesByBackend(ctx context.Context) (map[string]int64, error)

	// SetBackendBytesUsed overwrites bytes_used with an authoritative
	// value. Unlike Increment/Decrement it carries no bytes_limit guard:
	// the recomputed ledger total is reality and the counter must follow.
	SetBackendBytesUsed(ctx context.Context, backendName string, value int64) error
}
