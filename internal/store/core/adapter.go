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
	MultipartTxAdapter

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
// them and move the quota. InsertObjectLocation writes every column loc carries
// and stamps the current time only when loc has no CreatedAt.
// UpdateCopyMetadata writes a patch to the copy loc names and returns
// ErrCopyChanged when no row has its storage key.
// ListedPathStates reports, for paths a backend listed, which hold a recorded
// copy and which a queued or dead-lettered delete names; a pending delete
// outranks a recorded copy.
type ObjectsTxAdapter interface {
	GetExistingCopiesForUpdate(ctx context.Context, objectKey string) ([]ExistingCopy, error)
	InsertObjectLocation(ctx context.Context, loc *ObjectLocation) error
	DeleteObjectCopies(ctx context.Context, objectKey string) error

	GetCopiesForKeysForUpdate(ctx context.Context, keys []string) ([]KeyedExistingCopy, error)
	DeleteObjectsByKeys(ctx context.Context, keys []string) error // rows must already be locked

	ListedPathStates(ctx context.Context, backend string, paths []string) (map[string]PathState, error)           // untracked paths are absent
	LockObjectOnBackend(ctx context.Context, objectKey, backend string) (loc *ObjectLocation, ok bool, err error) // ok=false: row gone, a benign race
	DeleteObjectFromBackend(ctx context.Context, objectKey, backend string) error

	UpdateCopyMetadata(ctx context.Context, loc *ObjectLocation, p *CopyPatch) error
	InsertObjectLocationIfNotExists(ctx context.Context, loc *ObjectLocation) (inserted bool, err error) // import-side, preserves an existing row
}

// -------------------------------------------------------------------------
// CLEANUP
// -------------------------------------------------------------------------

// CleanupTxAdapter exposes the transactional operations on the cleanup_queue
// table that core orchestration needs; single-statement operations live on
// CleanupStore. InsertCleanup queues a row claimed by this instance and
// returns its id. InsertCleanupDLQ keeps the queue row's id and created_at so
// an operator can tell how long the cleanup was outstanding.
type CleanupTxAdapter interface {
	InsertCleanup(ctx context.Context, c *CleanupRequest) (int64, error)
	GetCleanupQueueRow(ctx context.Context, id int64) (CleanupQueueRow, error)
	InsertCleanupDLQ(ctx context.Context, row *CleanupQueueRow) error // pointer: the row payload is 112 bytes
	DeleteCleanupItem(ctx context.Context, id int64) error
}

// -------------------------------------------------------------------------
// MULTIPART
// -------------------------------------------------------------------------

// MultipartTxAdapter exposes the transactional writes on multipart parts.
// UpsertPart records a part at the path its intent names and returns the part
// of that number it replaced, or nil. DeleteMultipartUpload removes the upload
// and its parts and returns the upload's backend with the parts it removed. A
// part recorded concurrently cannot slip between the read and the delete, and
// an upload already gone returns no parts.
type MultipartTxAdapter interface {
	UpsertPart(ctx context.Context, p *RecordPartParams) (replaced *MultipartPart, err error)
	DeleteMultipartUpload(ctx context.Context, uploadID string) (backendName string, parts []MultipartPart, err error)
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

	AllBackendBytesUsed(ctx context.Context) (map[string]int64, error)     // the striped total per backend
	SumObjectSizesByBackend(ctx context.Context) (map[string]int64, error) // the ledger truth it is diffed against

	SetBackendBytesUsed(ctx context.Context, backendName string, value int64) error
}
