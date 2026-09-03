// -------------------------------------------------------------------------------
// Core TxAdapter - Per-Engine Seam
//
// Author: Alex Freidah
//
// Declares the per-feature transactional adapters. Engine packages (postgres,
// sqlite) provide concrete implementations that translate between sqlc-generated
// row types and the canonical core domain types. Engine-agnostic business logic
// in this package never touches a driver-typed value - it operates exclusively
// on these interfaces.
//
// Method names are engine-neutral so a Postgres-flavored mechanism (FOR UPDATE
// row locks) and a SQLite-flavored equivalent (single-writer existence probe)
// can satisfy the same contract without leaking either dialect into core.
// -------------------------------------------------------------------------------

package core

import (
	"context"
)

// -------------------------------------------------------------------------
// PARENT ADAPTER
// -------------------------------------------------------------------------

// TxAdapter is the per-engine transactional seam. A core operation receives one
// of these from Runner.WithTx, runs business logic against it, and never
// touches a driver-specific transaction directly. The parent embeds the
// per-feature adapters so callers depend only on the narrowest interface that
// fits their needs.
//
// AcquireKeyLock is where the two engines differ most: Postgres derives
// pg_advisory_xact_lock from a hash of the key, while SQLite no-ops because the
// engine serializes writers and the in-transaction existence probe gives the
// same guarantee.
type TxAdapter interface {
	PendingTxAdapter
	ObjectsTxAdapter
	CleanupTxAdapter
	QuotaTxAdapter
	TagsTxAdapter

	AcquireKeyLock(ctx context.Context, objectKey string) error
}

// -------------------------------------------------------------------------
// PENDING
// -------------------------------------------------------------------------

// PendingTxAdapter exposes the transactional operations on the pending_objects
// table.
//
// ClaimPending reports false when another worker has already resolved the
// intent, which is how two reapers racing on one row settle it once. Postgres
// claims with SELECT FOR UPDATE and SQLite with an existence probe inside the
// writer-serialized transaction, which are the same guarantee.
type PendingTxAdapter interface {
	ClaimPending(ctx context.Context, intentID string) (claimed bool, err error)
	DeletePending(ctx context.Context, intentID string) error
}

// -------------------------------------------------------------------------
// OBJECTS
// -------------------------------------------------------------------------

// KeyedExistingCopy is an ExistingCopy that also carries the object_key so
// batch operations can group rows by key.
type KeyedExistingCopy struct {
	ObjectKey   string
	BackendName string
	SizeBytes   int64
}

// ObjectsTxAdapter exposes the transactional operations on the object_locations
// table.
//
// The ForUpdate reads lock the rows they return so the same transaction can
// delete them and move the quota that follows them; splitting those halves
// across transactions is what makes the counter drift from the ledger. For the
// same reason the stored-form writes - UpdateCompressedForm, MarkCopyEncrypted,
// MarkCopyDecrypted - only touch the row, leaving the matching quota adjustment
// to the caller that already holds the transaction.
//
// InsertReplicaConditional reads the source row's size inside the insert and
// returns it, so the caller credits the destination quota with the size the
// ledger actually recorded rather than one measured separately.
//
// RecordCompressionProbe stores what the encoder measured for a copy it
// declined to store compressed, so a verbatim move can carry the measurement
// onto the destination row rather than re-deriving it from bytes it did not
// change.
type ObjectsTxAdapter interface {
	GetExistingCopiesForUpdate(ctx context.Context, objectKey string) ([]ExistingCopy, error)
	InsertObjectLocation(ctx context.Context, loc *ObjectLocation) error
	DeleteObjectCopies(ctx context.Context, objectKey string) error

	GetCopiesForKeysForUpdate(ctx context.Context, keys []string) ([]KeyedExistingCopy, error)
	DeleteObjectsByKeys(ctx context.Context, keys []string) error // rows must already be locked

	CheckObjectExistsOnBackend(ctx context.Context, objectKey, backend string) (bool, error)
	LockObjectOnBackend(ctx context.Context, objectKey, backend string) (loc *ObjectLocation, ok bool, err error) // ok=false: row gone, a benign race
	DeleteObjectFromBackend(ctx context.Context, objectKey, backend string) error
	GetCopySizeBytes(ctx context.Context, objectKey, backendName string) (int64, error)

	RecordCompressionProbe(ctx context.Context, probe *CompressionProbe) error
	InsertObjectLocationIfNotExists(ctx context.Context, loc *ObjectLocation) (inserted bool, err error) // import-side, preserves an existing row
	InsertReplicaConditional(ctx context.Context, objectKey, targetBackend, sourceBackend string) (size int64, inserted bool, err error)

	UpdateCompressedForm(ctx context.Context, u *CompressedUpdate) error
	MarkCopyEncrypted(ctx context.Context, u *EncryptedUpdate) error
	MarkCopyDecrypted(ctx context.Context, objectKey, backendName string, plaintextSize int64) error
}

// -------------------------------------------------------------------------
// CLEANUP
// -------------------------------------------------------------------------

// CleanupTxAdapter exposes the transactional operations on the cleanup_queue
// table needed by core orchestration. Background-worker helpers that already
// live entirely on a single transaction (Enqueue, Retry, Complete) stay on the
// read/write path through CleanupStore.
//
// The queue-to-DLQ move is three of these in one transaction - read the row,
// insert it, delete it - so a cleanup cannot be lost between the two tables.
// The DLQ insert keeps the queue row's id and created_at, which is how an
// operator later tells how long the cleanup was outstanding.
//
// HasPendingCleanup is read inside the import transaction so a cleanup
// finishing concurrently cannot slip between the check and the insert.
type CleanupTxAdapter interface {
	SumAndDeleteCleanupQueueRows(ctx context.Context, objectKey, backend string) (deleted int64, totalBytes int64, err error)
	GetCleanupQueueRow(ctx context.Context, id int64) (CleanupQueueRow, error)
	InsertCleanupDLQ(ctx context.Context, row *CleanupQueueRow) error // pointer: the row payload is 112 bytes
	DeleteCleanupItem(ctx context.Context, id int64) error
	HasPendingCleanup(ctx context.Context, objectKey, backend string) (bool, error)
}

// -------------------------------------------------------------------------
// TAGS
// -------------------------------------------------------------------------

// TagsTxAdapter exposes the transactional operations on the object_tags table.
// Reads are absent by design: a tag set is read outside a transaction through
// TagStore, and the write paths here replace or clear a whole set rather than
// deriving it from what is already stored.
//
// Callers delete the existing set before inserting, so a primary-key conflict
// from InsertObjectTag means a duplicate key survived validation and is
// surfaced rather than absorbed. Clearing a set that is already empty is a
// no-op.
type TagsTxAdapter interface {
	InsertObjectTag(ctx context.Context, objectKey, tagKey, tagValue string) error
	DeleteObjectTags(ctx context.Context, objectKey string) error
	DeleteObjectTagsForKeys(ctx context.Context, objectKeys []string) error // one statement, for batch delete
}

// -------------------------------------------------------------------------
// QUOTA
// -------------------------------------------------------------------------

// QuotaTxAdapter exposes the transactional operations on the backend_quotas
// table.
//
// Increment enforces the limit and Decrement does not, because refusing to
// give bytes back would strand a backend above its ceiling. The two
// authoritative writers, SetBackendBytesUsed and AdjustBackendBytesUsed, carry
// no limit guard at all: usage reconciliation and the stored-form rewrites
// describe bytes that already moved on the backend, and the counter has to
// follow them in either direction.
type QuotaTxAdapter interface {
	IncrementBackendQuota(ctx context.Context, backendName string, delta int64) error // ErrNoSpaceAvailable when the limit would be exceeded
	DecrementBackendQuota(ctx context.Context, backendName string, delta int64) error
	DecrementOrphanBytes(ctx context.Context, backendName string, delta int64) error // clamped at zero

	AllBackendBytesUsed(ctx context.Context) (map[string]int64, error)     // the stored counter
	SumObjectSizesByBackend(ctx context.Context) (map[string]int64, error) // the ledger truth it is diffed against

	SetBackendBytesUsed(ctx context.Context, backendName string, value int64) error
	AdjustBackendBytesUsed(ctx context.Context, backendName string, delta int64) error // signed, clamped at zero
}
