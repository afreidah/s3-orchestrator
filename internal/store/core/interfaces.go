// -------------------------------------------------------------------------------
// Core Role Interfaces - Narrow Per-Worker Store Contracts
//
// Author: Alex Freidah
//
// Declares the narrow role interfaces consumers request from DI. The Postgres
// and SQLite engines each return a *Store from the core package that satisfies
// every role - this package exposes no composed god interface; consumers
// depend only on the role they actually use.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// The roles below are the whole persistence contract. Each consumer declares
// its own dependency in terms of the ones it calls, and both *postgres.Store
// and *sqlite.Store satisfy every role, asserted through AssertEngine.
//
// There is deliberately no interface unioning them. The union is not a fact
// about the domain - it is a fact about there being one store type per engine
// - so it exists only as the unexported engineRoles constraint in engine.go,
// which no consumer can hold, and unexported in internal/di, which is the only
// place that holds an opened engine before splitting it into these roles.
//
// CB protection lives in each driver's DBTX chokepoint, so every role carries
// the breaker semantics transparently.

// -------------------------------------------------------------------------
// REQUEST-TIME ROLE INTERFACES
// -------------------------------------------------------------------------

// ObjectStore defines object location CRUD and listing operations.
//
// RecordObjectIdentity fills in the identity columns a read had to ask a
// backend for, on every copy of the key, while the copy that was read is still
// recorded. Columns already set are left alone, because a value the write
// computed outranks what a backend reports. UpdateCopyMetadata writes a patch
// to the copy loc names, or returns ErrCopyChanged when a newer write replaced
// it.
//
// Every method that stops recording a copy queues the deletion of its bytes in
// the same transaction and returns the queued rows, claimed by this instance,
// for the caller to settle.
type ObjectStore interface {
	GetAllObjectLocations(ctx context.Context, key string) ([]ObjectLocation, error)
	GetObjectBackendsForKeys(ctx context.Context, keys []string) (map[string][]string, error)
	RecordObject(ctx context.Context, req *RecordObjectRequest) ([]CleanupItem, QuotaDeltas, error)
	DeleteObject(ctx context.Context, key string) ([]CleanupItem, QuotaDeltas, error)
	DeleteObjectsBatch(ctx context.Context, keys []string) ([]CleanupItem, QuotaDeltas, error)
	ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*ListObjectsResult, error)
	CountObjectsByPrefix(ctx context.Context, prefix string) (int64, error)
	ListObjectsDelimited(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*ListDelimitedResult, error)
	ListObjectsByBackend(ctx context.Context, backendName string, limit int, after SizeCursor) ([]ObjectLocation, error)
	ListObjectsByBackendKeyAsc(ctx context.Context, backendName, afterKey string, limit int) ([]ObjectLocation, error)
	PlaceCopy(ctx context.Context, p *Placement) (int64, []CleanupItem, error)
	ImportObject(ctx context.Context, req *ImportObjectRequest) (ImportOutcome, error)
	ListedPathStates(ctx context.Context, backend string, paths []string) (map[string]PathState, error)
	DeleteObjectLocation(ctx context.Context, loc *ObjectLocation, keepAtLeast int, reason string) ([]CleanupItem, error)
	RecordObjectIdentity(ctx context.Context, loc *ObjectLocation, id *ObjectIdentity) error
	UpdateCopyMetadata(ctx context.Context, loc *ObjectLocation, p *CopyPatch) error
}

// TagStore defines object tag read and write operations. Writes come from TxOps
// and are shared by both engines; reads are per-engine. GetObjectTags returns
// the set ordered by key and does not error on an untagged object. The tag
// count the read path needs comes from GetAllObjectLocations.
type TagStore interface {
	GetObjectTags(ctx context.Context, key string) ([]Tag, error)
	ReplaceObjectTags(ctx context.Context, key string, tags []Tag) error
	DeleteObjectTags(ctx context.Context, key string) error
}

// QuotaStore defines the byte-counter reads and writes. Placement decisions use
// the in-memory tracker, not these rows. ReconcileUsage brings each backend's
// striped total to SUM(object_locations.size_bytes), read from one snapshot,
// by adding the difference rather than overwriting the total, so a write that
// commits alongside keeps its own adjustment. It returns the nonzero
// per-backend deltas (truth - previous) it applied.
type QuotaStore interface {
	GetQuotaStats(ctx context.Context) (map[string]QuotaStat, error)
	ListBackendQuotaUsage(ctx context.Context) ([]BackendQuotaUsage, error)
	ReconcileUsage(ctx context.Context) (map[string]int64, error)
}

// MultipartStore defines multipart upload lifecycle operations. RecordPart
// clears the part's intent and queues the bytes of a part it replaced, and
// DeleteMultipartUpload queues every part's bytes under reason in the
// transaction that removes the upload; both return the queued rows.
type MultipartStore interface {
	CreateMultipartUpload(ctx context.Context, params *CreateMultipartUploadParams) (bool, error)
	GetMultipartUpload(ctx context.Context, uploadID string) (*MultipartUpload, error)
	RecordPart(ctx context.Context, p *RecordPartParams) ([]CleanupItem, error)
	GetParts(ctx context.Context, uploadID string) ([]MultipartPart, error)
	ListParts(ctx context.Context, uploadID string, afterPart, limit int) ([]MultipartPart, error)
	DeleteMultipartUpload(ctx context.Context, uploadID, reason string) ([]CleanupItem, error)
	ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]MultipartUpload, error)
	CountActiveMultipartUploads(ctx context.Context, bucketPrefix string) (int64, error)
	ScanMultipartUploads(ctx context.Context, filter MultipartUploadFilter, limit int, afterUploadID string) ([]MultipartUpload, error)
}

// MultipartUploadFilter selects uploads for an abort scan. Backend, when set,
// keeps one backend's uploads; CreatedBefore, when non-zero, keeps uploads
// started before it.
type MultipartUploadFilter struct {
	Backend       string
	CreatedBefore time.Time
}

// CreateMultipartUploadParams bundles the fields a CreateMultipartUpload
// row needs at insert time. The optional upload-level encryption fields
// stay nil for non-encrypted uploads so call sites can omit them.
type CreateMultipartUploadParams struct {
	UploadID      string
	ObjectKey     string
	BackendName   string
	ContentType   string
	Metadata      map[string]string
	EncryptionKey []byte // empty for unencrypted uploads
	KeyID         string // empty for unencrypted uploads
	Tags          []Tag  // applied to the object CompleteMultipartUpload produces
}

// ReplicationStore defines replication management operations.
type ReplicationStore interface {
	GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error)
	GetUnderReplicatedObjectsExcluding(ctx context.Context, factor, limit int, excludedBackends []string) ([]ObjectLocation, error)
	GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error)
	CountReplicationBacklog(ctx context.Context, factor int) (ReplicationBacklog, error)
}

// CleanupStore defines the cleanup queue lifecycle. Rows are queued by the
// transactions that stop recording bytes; a backend's orphan bytes are the sum
// of its queued and dead-lettered rows.
//
// GetPendingCleanups is a read-only snapshot for display; the worker uses
// ClaimPendingCleanups, which reserves a disjoint batch per instance and
// returns each row with the claim it stamped. A row is eligible when its claim
// is absent or older than graceCutoff, and such reclaimed rows come back with
// Reclaimed set.
//
// The settle writes act only on a row still held under the item's claim, so an
// instance whose claim aged out and was taken over cannot settle the row again.
// CompleteCleanupItems retires the rows whose bytes are gone in one statement
// and returns the ids it retired. RetryCleanupItem schedules the next attempt
// and releases the claim, and MoveCleanupToDLQ graduates an exhausted row; both
// report false for a lost claim.
//
// RequeueCleanupDLQ sends dead-lettered rows back with fresh attempts. An empty
// backend in the DLQ calls means every backend.
type CleanupStore interface {
	GetPendingCleanups(ctx context.Context, limit int) ([]CleanupItem, error)
	ClaimPendingCleanups(ctx context.Context, limit int, instanceID string, graceCutoff time.Time) ([]CleanupItem, error)
	CompleteCleanupItems(ctx context.Context, items []CleanupItem) ([]int64, error)
	RetryCleanupItem(ctx context.Context, item *CleanupItem, backoff time.Duration, lastError string) (bool, error)
	CleanupQueueDepth(ctx context.Context) (int64, error)
	SweepStaleCleanupQueueRows(ctx context.Context, storageKey, backend string) (int64, error)

	MoveCleanupToDLQ(ctx context.Context, item *CleanupItem, lastError string) (bool, error)
	CleanupDLQDepth(ctx context.Context) (int64, error)                                      // also refreshes the cleanup_dlq_depth gauge
	ListCleanupDLQ(ctx context.Context, backend string, limit int) ([]CleanupDLQItem, error) // newest graduation first
	RequeueCleanupDLQ(ctx context.Context, backend string) (int64, error)
}

// PendingStore defines in-flight PutObject intent tracking. The write
// path inserts an intent before the backend PUT and removes it on a
// successful commit; the pending reaper resolves intents left behind by
// a failed commit so a DB outage between PUT and RecordObject cannot
// silently destroy the prior copy of an overwritten key.
//
// InsertPendingIfFits admits the intent on a backend whose flags all fall
// within accepts. DiscardPending drops an intent whose upload will not be
// recorded and queues its bytes under reason.
type PendingStore interface {
	InsertPendingIfFits(ctx context.Context, p *PendingObject, accepts BackendFlags) (bool, error)
	DeletePending(ctx context.Context, intentID string) error
	DiscardPending(ctx context.Context, p *PendingObject, reason string) ([]CleanupItem, error)
	GetStalePending(ctx context.Context, olderThan time.Time, limit int) ([]PendingObject, error)
	PromotePending(ctx context.Context, p *PendingObject) (PendingPromoteResult, []CleanupItem, QuotaDeltas, error)
	CommitCompanionCopy(ctx context.Context, p *PendingObject) (CompanionCommitResult, []CleanupItem, QuotaDeltas, error)
	PendingDepth(ctx context.Context) (int64, error)
	DeletePendingByBackend(ctx context.Context, backendName string) error
}

// IntegrityStore defines content hash verification operations. The scrub
// queries filter by backend in SQL so a copy the scrubber cannot read never
// occupies a batch slot. Both take scrubbedBefore so the count and the batch
// describe the same population.
type IntegrityStore interface {
	CopyLister
	GetLeastRecentlyScrubbedObjects(ctx context.Context, limit int, backends []string, scrubbedBefore time.Time) ([]ObjectLocation, error)
	CountScrubCandidatesOnBackends(ctx context.Context, backends []string, scrubbedBefore time.Time) (int64, error)
}

// CopyLister selects copies by the layers their bytes carry, for the passes
// that work through them: hash backfill, the unreadable purge, and the
// encryption and compression rewrites. The listing pages by cursor, not
// offset: a pass usually takes the copies it acts on out of its own filter,
// and an offset would skip rows.
type CopyLister interface {
	ListCopiesByForm(ctx context.Context, f *FormFilter, limit int, after Cursor) ([]ObjectLocation, error)
	CountCopiesByForm(ctx context.Context, with, without StoredFlags) (int64, error)
}

// ExpiredObjectsQuery selects the objects one lifecycle rule expires. Prefix
// and Tags are optional and every one set must match; a query with neither
// matches the whole namespace, which config validation refuses. After is the
// last key of the previous page, in byte order; empty starts at the first key.
type ExpiredObjectsQuery struct {
	Prefix string
	Tags   map[string]string
	Cutoff time.Time
	After  string
	Limit  int
}

// ExpiredObjectsLister defines object lifecycle expiration operations.
type ExpiredObjectsLister interface {
	ListExpiredObjects(ctx context.Context, q ExpiredObjectsQuery) ([]ObjectLocation, error)
}

// BackendStateStore persists each backend's state flags and drain progress,
// shared across instances and restarts, and the backend-wide operations that
// act on them. Admission refuses a backend carrying a flag writes do not
// accept.
//
// SetBackendFlag reports false when the flag was already set, and
// ClearBackendFlags when none of the given flags was. StartDrain reports false
// when the backend is already draining or drained; a failed drain is
// restarted, and other flags such as a cordon are kept. AddDrainedObjects and
// MarkDrainFailed act only on a drain in progress. CompleteDrain reports false
// while managed rows, intents or multipart uploads remain on the backend.
// ClearBackendState deletes the record when the backend is removed.
// BackendObjectStats counts the backend's objects and bytes, and
// DeleteBackendData drops every row it holds.
type BackendStateStore interface {
	ListBackendStates(ctx context.Context) ([]BackendStateRecord, error)
	SetBackendFlag(ctx context.Context, backendName string, flag BackendFlags) (bool, error)
	ClearBackendFlags(ctx context.Context, backendName string, flags BackendFlags) (bool, error)
	ClearBackendState(ctx context.Context, backendName string) (bool, error)
	StartDrain(ctx context.Context, backendName string) (bool, error)
	AddDrainedObjects(ctx context.Context, backendName string, moved int64) error
	MarkDrainFailed(ctx context.Context, backendName, reason string) error
	CompleteDrain(ctx context.Context, backendName string) (bool, error)
	BackendObjectStats(ctx context.Context, backendName string) (int64, int64, error)
	DeleteBackendData(ctx context.Context, backendName string) error
}

// UsageFlusher defines the store methods used by UsageTracker.FlushUsage.
type UsageFlusher interface {
	FlushUsageDeltas(ctx context.Context, backendName, period string, apiRequests, egressBytes, ingressBytes int64) error
	FlushPoolDeltas(ctx context.Context, backendName, period string, deltas PoolUsage) error
}

// AdvisoryLocker defines the leader-election helper used by background
// services. SQLite implementations no-op since the engine serializes
// writers; on Postgres this is pg_try_advisory_lock with the supplied
// lockID.
type AdvisoryLocker interface {
	WithAdvisoryLock(ctx context.Context, lockID int64, fn func(ctx context.Context) error) (bool, error)
}

// DashboardStore defines the methods used by the web UI dashboard
// aggregator.
type DashboardStore interface {
	GetQuotaStats(ctx context.Context) (map[string]QuotaStat, error)
	LedgerStats(ctx context.Context) (LedgerStats, error)
	GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error)
	GetUsageForPeriod(ctx context.Context, period string) (map[string]UsageStat, error)
	GetPoolUsageForPeriod(ctx context.Context, period string) (map[string]PoolUsage, error)
	ListDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*DirectoryListResult, error)
}

// -------------------------------------------------------------------------
// ADMIN ROLE INTERFACES
// -------------------------------------------------------------------------

// LifecycleAdmin defines startup, shutdown, and schema-management
// operations on the concrete store. These methods are not wrapped by
// CircuitBreakerStore - they run during boot before the breaker is wired
// up, and Close() releases pool resources directly.
type LifecycleAdmin interface {
	RunMigrations(ctx context.Context) error
	VerifySchemaVersion(ctx context.Context) error
	SyncQuotaLimits(ctx context.Context, backends []config.BackendConfig) error
	Close()
}

// ProvisioningStore defines the store half of the bucket registry: the buckets,
// users, credentials and grants held as rows rather than config entries.
// Registry assembly reads the listings at startup and on every reload. The
// schema refuses deleting a user that still holds credentials or grants.
type ProvisioningStore interface {
	ListBuckets(ctx context.Context) ([]Bucket, error)
	ListUsers(ctx context.Context) ([]User, error)
	ListCredentials(ctx context.Context) ([]Credential, error)
	ListGrants(ctx context.Context) ([]Grant, error)
	CreateBucket(ctx context.Context, b *Bucket) error
	CreateUser(ctx context.Context, u *User) error
	CreateCredential(ctx context.Context, c *Credential) error
	CreateGrant(ctx context.Context, g *Grant) error
	UpdateBucket(ctx context.Context, b *Bucket) error
	RenameUser(ctx context.Context, id, name string) error
	SetGrant(ctx context.Context, g *Grant) error
	DeleteBucket(ctx context.Context, name string) error
	DeleteUser(ctx context.Context, id string) error
	DeleteCredential(ctx context.Context, accessKeyID string) error
	DeleteGrant(ctx context.Context, userID string, r Resource) error
}

// NotificationOutbox defines the durable notification outbox operations
// the notifier worker uses to deliver webhook events with retry/backoff
// semantics. Leader election around the drain loop comes from a separate
// AdvisoryLocker dependency.
type NotificationOutbox interface {
	InsertNotification(ctx context.Context, eventType, payload, endpointURL string) error
	GetPendingNotifications(ctx context.Context, limit int) ([]NotificationRow, error)
	NotificationQueueDepth(ctx context.Context) (int64, error)
	CompleteNotification(ctx context.Context, id int64) error
	RetryNotification(ctx context.Context, id int64, backoff time.Duration, lastError string) error
}
