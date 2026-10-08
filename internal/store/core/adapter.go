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

// TxAdapter is the per-engine transactional seam a core operation receives from
// Runner.WithTx. AcquireKeyLock takes a Postgres advisory lock on a hash of the
// key; SQLite no-ops it because the engine already serializes writers.
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
// table. ClaimPending reports false when another worker already resolved the
// intent. ClearPendingForKey deletes every intent for the key except keep and
// returns what it removed so the bytes can be cleaned up, including intents on
// a backend the caller is writing to.
type PendingTxAdapter interface {
	ClaimPending(ctx context.Context, intentID string) (claimed bool, err error)
	DeletePending(ctx context.Context, intentID string) error
	ClearPendingForKey(ctx context.Context, objectKey string, keep []string) ([]SupersededIntent, error)
}

// -------------------------------------------------------------------------
// OBJECTS
// -------------------------------------------------------------------------

// KeyedExistingCopy is an ExistingCopy that also carries the object_key so
// batch operations can group rows by key.
type KeyedExistingCopy struct {
	ObjectKey   string
	BackendName string
	StorageKey  string
	SizeBytes   int64
}

// ObjectsTxAdapter exposes the transactional operations on the object_locations
// table. The ForUpdate reads lock their rows so the same transaction can delete
// them and move the quota. The stored-form writes touch only the row; the
// caller makes the matching quota adjustment in the same transaction.
// InsertReplicaConditional returns the source row's size as read inside the
// insert, for the caller to credit. RecordCompressionProbe stores what the
// encoder measured for a copy it declined to store compressed.
type ObjectsTxAdapter interface {
	GetExistingCopiesForUpdate(ctx context.Context, objectKey string) ([]ExistingCopy, error)
	InsertObjectLocation(ctx context.Context, loc *ObjectLocation) error
	DeleteObjectCopies(ctx context.Context, objectKey string) error

	GetCopiesForKeysForUpdate(ctx context.Context, keys []string) ([]KeyedExistingCopy, error)
	DeleteObjectsByKeys(ctx context.Context, keys []string) error // rows must already be locked

	CheckObjectExistsOnBackend(ctx context.Context, objectKey, backend string) (bool, error)
	CopyExistsAtPath(ctx context.Context, backend, storageKey string) (bool, error)                               // whatever object it belongs to
	LockObjectOnBackend(ctx context.Context, objectKey, backend string) (loc *ObjectLocation, ok bool, err error) // ok=false: row gone, a benign race
	DeleteObjectFromBackend(ctx context.Context, objectKey, backend string) error
	GetCopySizeBytes(ctx context.Context, objectKey, backendName string) (int64, error)

	RecordCompressionProbe(ctx context.Context, probe *CompressionProbe) error
	InsertObjectLocationIfNotExists(ctx context.Context, loc *ObjectLocation) (inserted bool, err error) // import-side, preserves an existing row
	InsertReplicaConditional(ctx context.Context, p *ReplicaInsert) (size int64, inserted bool, err error)

	UpdateCompressedForm(ctx context.Context, u *CompressedUpdate) error
	MarkCopyEncrypted(ctx context.Context, u *EncryptedUpdate) error
	MarkCopyDecrypted(ctx context.Context, u *DecryptedUpdate) error
}

// -------------------------------------------------------------------------
// CLEANUP
// -------------------------------------------------------------------------

// CleanupTxAdapter exposes the transactional operations on the cleanup_queue
// table that core orchestration needs; single-statement operations live on
// CleanupStore. InsertCleanupDLQ keeps the queue row's id and created_at so an
// operator can tell how long the cleanup was outstanding. HasPendingCleanup
// matches on the path, since a queued deletion names particular bytes.
type CleanupTxAdapter interface {
	SumAndDeleteCleanupQueueRows(ctx context.Context, storageKey, backend string) (deleted int64, totalBytes int64, err error)
	GetCleanupQueueRow(ctx context.Context, id int64) (CleanupQueueRow, error)
	InsertCleanupDLQ(ctx context.Context, row *CleanupQueueRow) error // pointer: the row payload is 112 bytes
	DeleteCleanupItem(ctx context.Context, id int64) error
	HasPendingCleanup(ctx context.Context, storageKey, backend string) (bool, error)
}

// -------------------------------------------------------------------------
// TAGS
// -------------------------------------------------------------------------

// TagsTxAdapter exposes the transactional writes on the object_tags table;
// reads go through TagStore. Callers clear the set before inserting, so a
// primary-key conflict from InsertObjectTag is a duplicate that survived
// validation and is returned as an error.
type TagsTxAdapter interface {
	InsertObjectTag(ctx context.Context, objectKey, tagKey, tagValue string) error
	DeleteObjectTags(ctx context.Context, objectKey string) error
	DeleteObjectTagsForKeys(ctx context.Context, objectKeys []string) error // one statement, for batch delete
}

// -------------------------------------------------------------------------
// QUOTA
// -------------------------------------------------------------------------

// QuotaTxAdapter exposes the transactional operations on the quota tables.
// Byte movements carry no limit guard because they record bytes already moved;
// the ceiling is enforced when a write is admitted. Callers pick the stripe
// for AdjustQuotaStripe with StripeFor.
type QuotaTxAdapter interface {
	AdjustQuotaStripe(ctx context.Context, backendName string, stripe int16, delta int64) error
	DecrementOrphanBytes(ctx context.Context, backendName string, delta int64) error // clamped at zero

	AllBackendBytesUsed(ctx context.Context) (map[string]int64, error)     // the striped total per backend
	SumObjectSizesByBackend(ctx context.Context) (map[string]int64, error) // the ledger truth it is diffed against

	SetBackendBytesUsed(ctx context.Context, backendName string, value int64) error
}
