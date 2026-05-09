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

// -------------------------------------------------------------------------
// REQUEST-TIME ROLE INTERFACES
// -------------------------------------------------------------------------

// ObjectStore defines object location CRUD and listing operations.
type ObjectStore interface {
	GetAllObjectLocations(ctx context.Context, key string) ([]ObjectLocation, error)
	GetObjectBackendsForKeys(ctx context.Context, keys []string) (map[string][]string, error)
	RecordObject(ctx context.Context, key, backend string, size int64, enc *EncryptionMeta) ([]DeletedCopy, error)
	RecordObjectAndClearPending(ctx context.Context, key, backend string, size int64, enc *EncryptionMeta, intentID string) ([]DeletedCopy, error)
	DeleteObject(ctx context.Context, key string) ([]DeletedCopy, error)
	DeleteObjectsBatch(ctx context.Context, keys []string) (map[string][]DeletedCopy, error)
	ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*ListObjectsResult, error)
	ListObjectsByBackend(ctx context.Context, backendName string, limit int) ([]ObjectLocation, error)
	ListObjectsByBackendKeyAsc(ctx context.Context, backendName, afterKey string, limit int) ([]ObjectLocation, error)
	MoveObjectLocation(ctx context.Context, key, fromBackend, toBackend string) (int64, error)
	ImportObject(ctx context.Context, key, backend string, size int64) (bool, error)
	DeleteObjectLocation(ctx context.Context, key, backendName string) error
}

// QuotaStore defines quota routing queries.
type QuotaStore interface {
	GetBackendWithSpace(ctx context.Context, size int64, backendOrder []string) (string, error)
	GetLeastUtilizedBackend(ctx context.Context, size int64, eligible []string) (string, error)
	GetQuotaStats(ctx context.Context) (map[string]QuotaStat, error)
}

// MultipartStore defines multipart upload lifecycle operations.
type MultipartStore interface {
	CreateMultipartUpload(ctx context.Context, params *CreateMultipartUploadParams) error
	GetMultipartUpload(ctx context.Context, uploadID string) (*MultipartUpload, error)
	RecordPart(ctx context.Context, uploadID string, partNumber int, etag string, size int64, enc *EncryptionMeta) error
	GetParts(ctx context.Context, uploadID string) ([]MultipartPart, error)
	DeleteMultipartUpload(ctx context.Context, uploadID string) error
	ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]MultipartUpload, error)
	CountActiveMultipartUploads(ctx context.Context, bucketPrefix string) (int64, error)
	GetStaleMultipartUploads(ctx context.Context, olderThan time.Duration) ([]MultipartUpload, error)
	GetMultipartUploadsByBackend(ctx context.Context, backendName string) ([]MultipartUpload, error)

	// ListLegacyMultipartUploads returns uploads whose encryption_key
	// column is NULL - rows created before migration 00010 that the
	// multipart_dek_backfill worker still has to migrate to the
	// shared-DEK format. limit caps the page size so the backfill can
	// chunk through a backlog without blowing past statement memory.
	ListLegacyMultipartUploads(ctx context.Context, limit int) ([]MultipartUpload, error)

	// UpdateUploadEncryption persists the upload-level wrapped DEK and
	// key ID once the backfill worker has re-encrypted every part of
	// an upload. Returns ErrObjectNotFound when the row no longer
	// exists (e.g., the upload was aborted while the backfill was in
	// flight) so the caller treats the row as already-resolved.
	UpdateUploadEncryption(ctx context.Context, uploadID string, encryptionKey []byte, keyID string) error

	// UpdatePartEncryption replaces a part row's encryption metadata
	// and recorded ciphertext size after the backfill rewrites the
	// part on the backend under the new upload-level DEK. Used only
	// by the backfill path; the regular UploadPart write goes through
	// RecordPart.
	UpdatePartEncryption(ctx context.Context, uploadID string, partNumber int, sizeBytes int64, enc *EncryptionMeta) error
}

// CreateMultipartUploadParams bundles the fields a CreateMultipartUpload
// row needs at insert time. Pulled into a struct because the optional
// upload-level encryption fields would otherwise push the call signature
// past gocritic's parameter-count limit and force every call site to
// pass empty values for non-encrypted uploads.
type CreateMultipartUploadParams struct {
	UploadID      string
	ObjectKey     string
	BackendName   string
	ContentType   string
	Metadata      map[string]string
	EncryptionKey []byte // empty for unencrypted uploads
	KeyID         string // empty for unencrypted uploads
}

// ReplicationStore defines replication management operations.
type ReplicationStore interface {
	GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error)
	GetUnderReplicatedObjectsExcluding(ctx context.Context, factor, limit int, excludedBackends []string) ([]ObjectLocation, error)
	RecordReplica(ctx context.Context, key, targetBackend, sourceBackend string) (size int64, inserted bool, err error)
	GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error)
	CountOverReplicatedObjects(ctx context.Context, factor int) (int64, error)
	RemoveExcessCopy(ctx context.Context, key, backendName string, size int64) error
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
	GetRandomHashedObjects(ctx context.Context, limit int) ([]ObjectLocation, error)
	GetObjectsWithoutHash(ctx context.Context, limit, offset int) ([]ObjectLocation, error)
	UpdateContentHash(ctx context.Context, key, backendName, hash string) error
}

// ExpiredObjectsLister defines object lifecycle expiration operations.
type ExpiredObjectsLister interface {
	ListExpiredObjects(ctx context.Context, prefix string, cutoff time.Time, limit int) ([]ObjectLocation, error)
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
	GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error)
	GetUsageForPeriod(ctx context.Context, period string) (map[string]UsageStat, error)
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

// EncryptionAdmin defines the admin-only encryption key rotation and
// encrypt/decrypt batch operations used by the admin HTTP handler. These
// are not on the request hot path, so they bypass the circuit breaker.
type EncryptionAdmin interface {
	ListEncryptedLocations(ctx context.Context, keyID string, limit, offset int) ([]EncryptedLocation, error)
	UpdateEncryptionKey(ctx context.Context, objectKey, backendName string, newEncryptionKey []byte, newKeyID string) error
	ListUnencryptedLocations(ctx context.Context, limit, offset int) ([]UnencryptedLocation, error)
	MarkObjectEncrypted(ctx context.Context, objectKey, backendName string, encryptionKey []byte, keyID string, plaintextSize, ciphertextSize int64) error
	ListAllEncryptedLocations(ctx context.Context, limit, offset int) ([]DecryptableLocation, error)
	MarkObjectDecrypted(ctx context.Context, objectKey, backendName string, plaintextSize int64) error
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
