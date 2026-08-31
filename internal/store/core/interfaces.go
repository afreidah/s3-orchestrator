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
type ObjectStore interface {
	GetAllObjectLocations(ctx context.Context, key string) ([]ObjectLocation, error)
	GetObjectBackendsForKeys(ctx context.Context, keys []string) (map[string][]string, error)
	RecordObject(ctx context.Context, req *RecordObjectRequest) ([]DeletedCopy, error)
	DeleteObject(ctx context.Context, key string) ([]DeletedCopy, error)
	DeleteObjectsBatch(ctx context.Context, keys []string) (map[string][]DeletedCopy, error)
	ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*ListObjectsResult, error)
	ListObjectsDelimited(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*ListDelimitedResult, error)
	ListObjectsByBackend(ctx context.Context, backendName string, limit int) ([]ObjectLocation, error)
	ListObjectsByBackendKeyAsc(ctx context.Context, backendName, afterKey string, limit int) ([]ObjectLocation, error)
	MoveObjectLocation(ctx context.Context, key, fromBackend, toBackend string) (int64, error)
	ImportObject(ctx context.Context, key, backend string, size int64, unmanaged bool, form *StoredForm) (ImportOutcome, error)
	DeleteObjectLocation(ctx context.Context, key, backendName string) error

	// RecordObjectIdentity fills in the identity columns a read had to ask a
	// backend for, on every copy of the key so the answer stops depending on
	// which one replies. Columns already set are left alone: what a write
	// computed over the client's own bytes outranks what a backend reports
	// about the bytes as stored.
	RecordObjectIdentity(ctx context.Context, key string, id *ObjectIdentity) error
}

// TagStore defines object tag read and write operations. Writes are
// transactional and promoted from TxOps, so both engines share one
// implementation; only the reads are per-engine.
//
// GetObjectTags returns the set ordered by key, so the Tagging XML response is
// byte-identical run to run. CountObjectTags returns its size for the read
// path's tagging-count header, which needs how many rather than which. Neither
// errors on an untagged object: an untagged object has an empty TagSet, not a
// missing one.
type TagStore interface {
	GetObjectTags(ctx context.Context, key string) ([]Tag, error)
	CountObjectTags(ctx context.Context, key string) (int, error)
	ReplaceObjectTags(ctx context.Context, key string, tags []Tag) error
	DeleteObjectTags(ctx context.Context, key string) error
}

// QuotaStore defines quota routing queries.
type QuotaStore interface {
	GetBackendWithSpace(ctx context.Context, size int64, backendOrder []string) (string, error)
	GetLeastUtilizedBackend(ctx context.Context, size int64, eligible []string) (string, error)
	GetQuotaStats(ctx context.Context) (map[string]QuotaStat, error)

	// ReconcileUsage recomputes bytes_used from SUM(object_locations.size_bytes)
	// per backend, correcting drift in the incrementally maintained counter.
	// Returns the per-backend delta applied (truth - previous); only drifted
	// backends are present.
	ReconcileUsage(ctx context.Context) (map[string]int64, error)
}

// MultipartStore defines multipart upload lifecycle operations.
type MultipartStore interface {
	CreateMultipartUpload(ctx context.Context, params *CreateMultipartUploadParams) error
	GetMultipartUpload(ctx context.Context, uploadID string) (*MultipartUpload, error)
	RecordPart(ctx context.Context, p *RecordPartParams) error
	GetParts(ctx context.Context, uploadID string) ([]MultipartPart, error)
	DeleteMultipartUpload(ctx context.Context, uploadID string) error
	ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]MultipartUpload, error)
	CountActiveMultipartUploads(ctx context.Context, bucketPrefix string) (int64, error)
	GetStaleMultipartUploads(ctx context.Context, olderThan time.Duration) ([]MultipartUpload, error)
	GetMultipartUploadsByBackend(ctx context.Context, backendName string) ([]MultipartUpload, error)
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
	RecordReplica(ctx context.Context, key, targetBackend, sourceBackend string) (size int64, inserted bool, err error)
	GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error)
	CountOverReplicatedObjects(ctx context.Context, factor int) (int64, error)
	RemoveExcessCopy(ctx context.Context, key, backendName string, factor int) (removed bool, err error)
}

// CleanupStore defines cleanup queue and orphan byte tracking operations.
type CleanupStore interface {
	EnqueueCleanup(ctx context.Context, backendName, objectKey, reason string, sizeBytes int64) error

	// GetPendingCleanups returns a read-only snapshot of pending rows for
	// admin / dashboard display. It does not stamp claim columns; the
	// cleanup worker uses ClaimPendingCleanups instead.
	GetPendingCleanups(ctx context.Context, limit int) ([]CleanupItem, error)

	// ClaimPendingCleanups atomically reserves a batch of cleanup rows
	// for the calling instance. Postgres uses FOR UPDATE SKIP LOCKED so
	// concurrent claim transactions across instances return disjoint row
	// sets; SQLite serialises writes intrinsically. A row is eligible
	// when claimed_at IS NULL or older than graceCutoff (a stale claim
	// from a worker that died mid-process). Returned rows have Reclaimed
	// set true when their previous claim was reclaimed by this call.
	ClaimPendingCleanups(ctx context.Context, limit int, instanceID string, graceCutoff time.Time) ([]CleanupItem, error)

	// CompleteCleanupItem atomically deletes a successfully-processed row
	// and decrements the backing backend's orphan_bytes by the row's
	// size_bytes. Idempotent against re-claim retries: if the row was
	// already deleted by a previous worker, orphan_bytes is not
	// double-decremented.
	CompleteCleanupItem(ctx context.Context, id int64) error

	// RetryCleanupItem advances next_retry, records the error, and
	// clears the claim so the row is immediately re-eligible for the
	// next worker tick.
	RetryCleanupItem(ctx context.Context, id int64, backoff time.Duration, lastError string) error
	CleanupQueueDepth(ctx context.Context) (int64, error)
	IncrementOrphanBytes(ctx context.Context, backendName string, amount int64) error
	DecrementOrphanBytes(ctx context.Context, backendName string, amount int64) error
	SweepStaleCleanupQueueRows(ctx context.Context, key, backend string) (int64, error)

	// MoveCleanupToDLQ atomically graduates an exhausted cleanup_queue
	// row to cleanup_dlq. orphan_bytes is intentionally left untouched
	// because the underlying backend object is still on disk; the DLQ
	// entry exists so an operator can investigate, retry, or write off
	// each unrecoverable orphan deliberately. Returns true if a row was
	// moved, false if no row existed (benign concurrent-finaliser race).
	MoveCleanupToDLQ(ctx context.Context, id int64, lastError string) (bool, error)

	// CleanupDLQDepth returns the number of rows currently in
	// cleanup_dlq. Updates the cleanup_dlq_depth gauge so dashboards can
	// surface the count of unrecoverable orphans needing attention.
	CleanupDLQDepth(ctx context.Context) (int64, error)

	// ListCleanupDLQ returns dead-lettered cleanup rows for operator
	// inspection, newest graduation first. An empty backend lists every
	// backend; a non-empty one scopes the listing. limit bounds the page.
	ListCleanupDLQ(ctx context.Context, backend string, limit int) ([]CleanupDLQItem, error)

	// RequeueCleanupDLQ atomically moves dead-lettered rows back into
	// cleanup_queue (fresh attempts) so the cleanup worker retries them
	// against a recovered backend; orphan_bytes then decrements naturally
	// as those retries complete. An empty backend requeues every backend.
	// Returns the number of rows requeued.
	RequeueCleanupDLQ(ctx context.Context, backend string) (int64, error)
}

// PendingStore defines in-flight PutObject intent tracking. The write
// path inserts an intent before the backend PUT and removes it on a
// successful commit; the pending reaper resolves intents left behind by
// a failed commit so a DB outage between PUT and RecordObject cannot
// silently destroy the prior copy of an overwritten key.
type PendingStore interface {
	InsertPending(ctx context.Context, p *PendingObject) error
	DeletePending(ctx context.Context, intentID string) error
	GetStalePending(ctx context.Context, olderThan time.Time, limit int) ([]PendingObject, error)
	PromotePending(ctx context.Context, p *PendingObject) (PendingPromoteResult, []DeletedCopy, error)
	PendingDepth(ctx context.Context) (int64, error)
	DeletePendingByBackend(ctx context.Context, backendName string) error
}

// IntegrityStore defines content hash verification operations.
type IntegrityStore interface {
	// GetLeastRecentlyScrubbedObjects returns the copies most overdue for
	// verification on the named backends. The backend filter belongs in the
	// query rather than in the caller: a copy the scrubber cannot afford to
	// read must never occupy a batch slot, or it is either stamped as examined
	// without being read or left at the head of the queue forever.
	GetLeastRecentlyScrubbedObjects(ctx context.Context, limit int, backends []string) ([]ObjectLocation, error)
	CountScrubCandidatesOnBackends(ctx context.Context, backends []string) (int64, error)
	GetObjectsWithoutHash(ctx context.Context, limit, offset int) ([]ObjectLocation, error)
	UpdateContentHash(ctx context.Context, key, backendName, hash string) error
	MarkObjectScrubbed(ctx context.Context, key, backendName string) error
	OldestUnverifiedAge(ctx context.Context) (age time.Duration, neverVerified int64, err error)
}

// ExpiredObjectsQuery selects the objects one lifecycle rule expires. Prefix
// and Tags are both optional filters and every one set must match, so a query
// carrying each selects their intersection; a query with neither matches the
// whole namespace, which config validation refuses to express.
//
// A struct rather than positional parameters because the filter grows: size
// and absolute-date conditions belong here too, and adding one should not
// re-break every implementation and caller.
type ExpiredObjectsQuery struct {
	Prefix string
	Tags   map[string]string
	Cutoff time.Time
	Limit  int
}

// ExpiredObjectsLister defines object lifecycle expiration operations.
type ExpiredObjectsLister interface {
	ListExpiredObjects(ctx context.Context, q ExpiredObjectsQuery) ([]ObjectLocation, error)
}

// BackendLifecycleStore defines backend-level admin operations.
type BackendLifecycleStore interface {
	BackendObjectStats(ctx context.Context, backendName string) (int64, int64, error)
	DeleteBackendData(ctx context.Context, backendName string) error
}

// UsageFlusher defines the store method used by UsageTracker.FlushUsage.
type UsageFlusher interface {
	FlushUsageDeltas(ctx context.Context, backendName, period string, apiRequests, egressBytes, ingressBytes int64) error
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
	GetObjectCounts(ctx context.Context) (map[string]int64, error)
	GetUnverifiedObjectCounts(ctx context.Context) (map[string]int64, error)
	GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error)
	GetUsageForPeriod(ctx context.Context, period string) (map[string]UsageStat, error)
	ListDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*DirectoryListResult, error)
	OldestUnverifiedAge(ctx context.Context) (age time.Duration, neverVerified int64, err error)
	CountUnencryptedLocations(ctx context.Context) (int64, error)
	CompressionStats(ctx context.Context) (map[string]CompressionStat, error)
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

// EncryptionAdmin defines the admin-only encryption key rotation and
// encrypt/decrypt batch operations used by the admin HTTP handler. These
// are not on the request hot path, so they bypass the circuit breaker.
type EncryptionAdmin interface {
	ListEncryptedLocations(ctx context.Context, keyID string, limit, offset int) ([]EncryptedLocation, error)
	UpdateEncryptionKey(ctx context.Context, objectKey, backendName string, newEncryptionKey []byte, newKeyID string) error
	ListUnencryptedLocations(ctx context.Context, limit int, after Cursor) ([]UnencryptedLocation, error)
	CountUnencryptedLocations(ctx context.Context) (int64, error)
	MarkObjectEncrypted(ctx context.Context, u *EncryptedUpdate) error
	ListAllEncryptedLocations(ctx context.Context, limit int, after Cursor) ([]DecryptableLocation, error)
	MarkObjectDecrypted(ctx context.Context, objectKey, backendName string, plaintextSize int64) error
}

// CompressionAdmin defines the admin-only bulk compression operations used by
// the admin HTTP handler, the web UI and adminctl. Like EncryptionAdmin these
// are off the request hot path, so they bypass the circuit breaker.
//
// The two listings are complements: one selects copies with no encoding, the
// other copies that have one.
//
// Both page by cursor rather than offset, and that is load-bearing. A pass
// rewrites the rows it is walking, so each one it processes leaves the
// predicate its listing selects on; an offset advanced against a set that just
// shrank underneath it steps straight over the rows that moved up, and the run
// reports success having never looked at them. The cursor is the last row seen,
// so a row leaving the set moves nothing.
// The uncompressed listing takes the thresholds that decide candidacy, because
// both of its exclusions are answers that outlive the pass: a copy under the
// size floor is never a candidate, and one already measured as unable to reach
// the ratio stays declined until a setting changes. Selecting on them is what
// keeps a second pass from paying to rediscover what the first one learned.
type CompressionAdmin interface {
	ListUncompressedLocations(ctx context.Context, limit int, after Cursor, t CompressionThresholds) ([]RewritableLocation, error)
	ListCompressedLocations(ctx context.Context, limit int, after Cursor) ([]RewritableLocation, error)
	MarkObjectCompressed(ctx context.Context, u *CompressedUpdate, previousSize int64) error
	RecordCompressionProbe(ctx context.Context, probe *CompressionProbe) error
}

// NotificationOutbox defines the durable notification outbox operations
// the notifier worker uses to deliver webhook events with retry/backoff
// semantics. Leader election around the drain loop comes from a separate
// AdvisoryLocker dependency.
type NotificationOutbox interface {
	InsertNotification(ctx context.Context, eventType, payload, endpointURL string) error
	GetPendingNotifications(ctx context.Context, limit int) ([]NotificationRow, error)
	CompleteNotification(ctx context.Context, id int64) error
	RetryNotification(ctx context.Context, id int64, backoff time.Duration, lastError string) error
}
