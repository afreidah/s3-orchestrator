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
	"time"
)

// -------------------------------------------------------------------------
// PARENT ADAPTER
// -------------------------------------------------------------------------

// TxAdapter is the per-engine transactional seam a core operation receives from
// Runner.WithTx. AcquireKeyLock takes a Postgres advisory lock on a hash of each
// key. AcquireBackendLock takes one on each backend, in LockSpaceBackendClaim,
// only when the backend has a byte ceiling, since a claim on an unlimited
// backend cannot overrun it. Both lock their names in one statement and in byte
// order, so overlapping sets cannot deadlock. SQLite no-ops both because the
// engine already serializes writers.
type TxAdapter interface {
	PendingTxAdapter
	ObjectsTxAdapter
	CleanupTxAdapter
	QuotaTxAdapter
	TagsTxAdapter
	MultipartTxAdapter

	AcquireKeyLock(ctx context.Context, objectKeys ...string) error
	AcquireBackendLock(ctx context.Context, backendNames ...string) error
}

// -------------------------------------------------------------------------
// PENDING
// -------------------------------------------------------------------------

// PendingTxAdapter exposes the transactional operations on the pending_objects
// table. ClaimPending reports false when another worker already resolved the
// intent. ClearPendingForKey deletes every intent for the keys except keep and
// returns what it removed so the bytes can be cleaned up, including intents on
// a backend the caller is writing to; it takes one key or a batch.
// InsertPendingIfFits records the intent
// only while the backend has room for it and carries no flag outside accepts,
// judged in the statement.
type PendingTxAdapter interface {
	InsertPendingIfFits(ctx context.Context, p *PendingObject, accepts BackendFlags) (bool, error)
	ClaimPending(ctx context.Context, intentID string) (claimed bool, err error)
	DeletePending(ctx context.Context, intentID string) error
	ClearPendingForKey(ctx context.Context, keep []string, objectKeys ...string) ([]SupersededIntent, error)
}

// -------------------------------------------------------------------------
// OBJECTS
// -------------------------------------------------------------------------

// ObjectsTxAdapter exposes the transactional operations on the object_locations
// table. GetExistingCopiesForUpdate locks every copy of the keys so the same
// transaction can delete them with DeleteObjectCopies and move the quota; both
// take one key or a batch. InsertObjectLocation writes every column loc carries
// and stamps the current time only when loc has no CreatedAt.
// UpdateCopyMetadata writes a patch to the copy loc names and returns
// ErrCopyChanged when no row has its storage key.
// ListedPathStates reports, for paths a backend listed, which hold a recorded
// copy and which a queued or dead-lettered delete names; a pending delete
// outranks a recorded copy.
type ObjectsTxAdapter interface {
	GetExistingCopiesForUpdate(ctx context.Context, objectKeys ...string) ([]ExistingCopy, error)
	InsertObjectLocation(ctx context.Context, loc *ObjectLocation) error
	DeleteObjectCopies(ctx context.Context, objectKeys ...string) error // rows must already be locked

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
// table that core orchestration needs; the settle operations live on
// CleanupStore. InsertCleanup queues the rows claimed by this instance and
// returns them, claims included, in no particular order.
type CleanupTxAdapter interface {
	InsertCleanup(ctx context.Context, requests ...*CleanupRequest) ([]CleanupItem, error)
}

// -------------------------------------------------------------------------
// MULTIPART
// -------------------------------------------------------------------------

// MultipartTxAdapter exposes the transactional writes on multipart parts.
// LockMultipartUpload locks the upload row for the rest of the transaction and
// returns its backend and when a running completion last renewed its mark,
// zero when none is running; an upload already gone returns an empty backend.
// UpsertPart records a
// part at the path its intent names and returns the part of that number it
// replaced, or nil. DeleteMultipartUpload removes the upload and its parts and
// returns the parts it removed.
type MultipartTxAdapter interface {
	LockMultipartUpload(ctx context.Context, uploadID string) (backendName string, completionHeartbeat time.Time, err error)
	UpsertPart(ctx context.Context, p *RecordPartParams) (replaced *MultipartPart, err error)
	DeleteMultipartUpload(ctx context.Context, uploadID string) (parts []MultipartPart, err error)
}

// -------------------------------------------------------------------------
// TAGS
// -------------------------------------------------------------------------

// TagsTxAdapter exposes the transactional writes on the object_tags table;
// reads go through TagStore. Callers clear the set before inserting, so a
// primary-key conflict from InsertObjectTag is a duplicate that survived
// validation and is returned as an error. DeleteObjectTags clears one key or a
// batch in one statement.
type TagsTxAdapter interface {
	InsertObjectTag(ctx context.Context, objectKey, tagKey, tagValue string) error
	DeleteObjectTags(ctx context.Context, objectKeys ...string) error
}

// -------------------------------------------------------------------------
// QUOTA
// -------------------------------------------------------------------------

// QuotaTxAdapter exposes the transactional operations on the quota tables.
// Byte movements carry no limit guard because they record bytes already moved;
// the ceiling is enforced when a write is admitted. AdjustQuotaStripes writes
// the charges in the order given, each on its own row.
type QuotaTxAdapter interface {
	AdjustQuotaStripes(ctx context.Context, charges ...StripeCharge) error
}
