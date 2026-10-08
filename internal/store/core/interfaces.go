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
// backend for, on every copy of the key. Columns already set are left alone,
// because a value the write computed outranks what a backend reports.
type ObjectStore interface {
	GetAllObjectLocations(ctx context.Context, key string) ([]ObjectLocation, error)
	GetObjectBackendsForKeys(ctx context.Context, keys []string) (map[string][]string, error)
	RecordObject(ctx context.Context, req *RecordObjectRequest) ([]DeletedCopy, QuotaDeltas, error)
	DeleteObject(ctx context.Context, key string) ([]DeletedCopy, QuotaDeltas, error)
	DeleteObjectsBatch(ctx context.Context, keys []string) (map[string][]DeletedCopy, QuotaDeltas, error)
	ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*ListObjectsResult, error)
	CountObjectsByPrefix(ctx context.Context, prefix string) (int64, error)
	ListObjectsDelimited(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*ListDelimitedResult, error)
	ListObjectsByBackend(ctx context.Context, backendName string, limit int) ([]ObjectLocation, error)
	ListObjectsByBackendKeyAsc(ctx context.Context, backendName, afterKey string, limit int) ([]ObjectLocation, error)
	MoveObjectLocation(ctx context.Context, m *MoveLocation) (int64, error)
	ImportObject(ctx context.Context, req *ImportObjectRequest) (ImportOutcome, error)
	DeleteObjectLocation(ctx context.Context, key, backendName string) (int64, error)
	RecordObjectIdentity(ctx context.Context, key string, id *ObjectIdentity) error
}

// TagStore defines object tag read and write operations. Writes come from TxOps
// and are shared by both engines; reads are per-engine. GetObjectTags returns
// the set ordered by key. Neither read errors on an untagged object.
type TagStore interface {
	GetObjectTags(ctx context.Context, key string) ([]Tag, error)
	CountObjectTags(ctx context.Context, key string) (int, error)
	ReplaceObjectTags(ctx context.Context, key string, tags []Tag) error
	DeleteObjectTags(ctx context.Context, key string) error
}

// QuotaStore defines the byte-counter reads and writes. Placement decisions use
// the in-memory tracker, not these rows. ReconcileUsage recomputes the counter
// from SUM(object_locations.size_bytes) and returns the nonzero per-backend
// deltas (truth - previous) it applied.
type QuotaStore interface {
	GetQuotaStats(ctx context.Context) (map[string]QuotaStat, error)
	ListBackendQuotaUsage(ctx context.Context) ([]BackendQuotaUsage, error)
	ReconcileUsage(ctx context.Context) (map[string]int64, error)
}

// MultipartStore defines multipart upload lifecycle operations.
type MultipartStore interface {
	CreateMultipartUpload(ctx context.Context, params *CreateMultipartUploadParams) (bool, error)
	GetMultipartUpload(ctx context.Context, uploadID string) (*MultipartUpload, error)
	RecordPart(ctx context.Context, p *RecordPartParams) error
	GetParts(ctx context.Context, uploadID string) ([]MultipartPart, error)
	ListParts(ctx context.Context, uploadID string, afterPart, limit int) ([]MultipartPart, error)
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
	RecordReplica(ctx context.Context, r *ReplicaInsert) (size int64, inserted bool, err error)
	GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error)
	CountOverReplicatedObjects(ctx context.Context, factor int) (int64, error)
	RemoveExcessCopy(ctx context.Context, key, backendName string, factor int) (RemovedCopy, error)
}

// CleanupStore defines cleanup queue and orphan byte tracking operations.
//
// GetPendingCleanups is a read-only snapshot for display; the worker uses
// ClaimPendingCleanups, which reserves a disjoint batch per instance. A row is
// eligible when its claim is absent or older than graceCutoff, and such
// reclaimed rows come back with Reclaimed set. CompleteCleanupItem deletes the
// row and decrements orphan_bytes, and never decrements twice for one row.
// RetryCleanupItem clears the claim so the row is eligible again after backoff.
//
// MoveCleanupToDLQ leaves orphan_bytes alone because the bytes are still on the
// backend; RequeueCleanupDLQ sends rows back with fresh attempts. An empty
// backend in the DLQ calls means every backend.
type CleanupStore interface {
	EnqueueCleanup(ctx context.Context, c *CleanupRequest) error
	GetPendingCleanups(ctx context.Context, limit int) ([]CleanupItem, error)
	ClaimPendingCleanups(ctx context.Context, limit int, instanceID string, graceCutoff time.Time) ([]CleanupItem, error)
	CompleteCleanupItem(ctx context.Context, id int64) error
	RetryCleanupItem(ctx context.Context, id int64, backoff time.Duration, lastError string) error
	CleanupQueueDepth(ctx context.Context) (int64, error)
	IncrementOrphanBytes(ctx context.Context, backendName string, amount int64) error
	DecrementOrphanBytes(ctx context.Context, backendName string, amount int64) error
	SweepStaleCleanupQueueRows(ctx context.Context, storageKey, backend string) (int64, error)

	MoveCleanupToDLQ(ctx context.Context, id int64, lastError string) (bool, error)
	CleanupDLQDepth(ctx context.Context) (int64, error)                                      // also refreshes the cleanup_dlq_depth gauge
	ListCleanupDLQ(ctx context.Context, backend string, limit int) ([]CleanupDLQItem, error) // newest graduation first
	RequeueCleanupDLQ(ctx context.Context, backend string) (int64, error)
}

// PendingStore defines in-flight PutObject intent tracking. The write
// path inserts an intent before the backend PUT and removes it on a
// successful commit; the pending reaper resolves intents left behind by
// a failed commit so a DB outage between PUT and RecordObject cannot
// silently destroy the prior copy of an overwritten key.
type PendingStore interface {
	InsertPendingIfFits(ctx context.Context, p *PendingObject) (bool, error)
	DeletePending(ctx context.Context, intentID string) error
	GetStalePending(ctx context.Context, olderThan time.Time, limit int) ([]PendingObject, error)
	PromotePending(ctx context.Context, p *PendingObject) (PendingPromoteResult, []DeletedCopy, QuotaDeltas, error)
	CommitCompanionCopy(ctx context.Context, p *PendingObject) (CompanionCommitResult, []DeletedCopy, QuotaDeltas, error)
	PendingDepth(ctx context.Context) (int64, error)
	DeletePendingByBackend(ctx context.Context, backendName string) error
}

// IntegrityStore defines content hash verification operations. The scrub
// queries filter by backend in SQL so a copy the scrubber cannot read never
// occupies a batch slot. Both take scrubbedBefore so the count and the batch
// describe the same population.
type IntegrityStore interface {
	GetLeastRecentlyScrubbedObjects(ctx context.Context, limit int, backends []string, scrubbedBefore time.Time) ([]ObjectLocation, error)
	CountScrubCandidatesOnBackends(ctx context.Context, backends []string, scrubbedBefore time.Time) (int64, error)
	GetObjectsWithoutHash(ctx context.Context, limit int, after Cursor, backend string) ([]ObjectLocation, error)
	ListUnreadableLocations(ctx context.Context, limit int) ([]ObjectLocation, error)
	CountUnreadableLocations(ctx context.Context) (int64, error)
	UpdateContentHash(ctx context.Context, key, backendName, hash string) error
	MarkObjectScrubbed(ctx context.Context, key, backendName string) error
	IntegrityCoverage(ctx context.Context, reachable []string) (CoverageStat, error)
}

// ExpiredObjectsQuery selects the objects one lifecycle rule expires. Prefix
// and Tags are optional and every one set must match; a query with neither
// matches the whole namespace, which config validation refuses.
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

// DrainStore persists each backend's drain record, shared across instances and
// restarts. Admission refuses a backend with a record in any state.
//
// StartDrain reports false when the backend is already draining or drained; a
// failed drain is restarted. AddDrainedObjects and MarkDrainFailed act only on a
// drain in progress. CompleteDrain reports false while managed rows, intents or
// multipart uploads remain on the backend. ClearDrain deletes the record, making
// the backend writable again.
type DrainStore interface {
	StartDrain(ctx context.Context, backendName string) (bool, error)
	ListDrains(ctx context.Context) ([]BackendDrain, error)
	AddDrainedObjects(ctx context.Context, backendName string, moved int64) error
	MarkDrainFailed(ctx context.Context, backendName, reason string) error
	CompleteDrain(ctx context.Context, backendName string) (bool, error)
	ClearDrain(ctx context.Context, backendName string) (bool, error)
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
	GetObjectCounts(ctx context.Context) (map[string]int64, error)
	GetUnverifiedObjectCounts(ctx context.Context) (map[string]int64, error)
	GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error)
	GetUsageForPeriod(ctx context.Context, period string) (map[string]UsageStat, error)
	GetPoolUsageForPeriod(ctx context.Context, period string) (map[string]PoolUsage, error)
	ListDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*DirectoryListResult, error)
	IntegrityCoverage(ctx context.Context, reachable []string) (CoverageStat, error)
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
	ListEncryptedLocations(ctx context.Context, keyID string, limit int, after Cursor) ([]EncryptedLocation, error)
	UpdateEncryptionKey(ctx context.Context, objectKey, backendName string, newEncryptionKey []byte, newKeyID string) error
	ListUnencryptedLocations(ctx context.Context, limit int, after Cursor, backend string) ([]UnencryptedLocation, error)
	CountUnencryptedLocations(ctx context.Context) (int64, error)
	MarkObjectEncrypted(ctx context.Context, u *EncryptedUpdate) error
	ListAllEncryptedLocations(ctx context.Context, limit int, after Cursor, backend string) ([]DecryptableLocation, error)
	MarkObjectDecrypted(ctx context.Context, u *DecryptedUpdate) error
}

// CompressionAdmin defines the admin-only bulk compression operations. They are
// off the request hot path and bypass the circuit breaker.
//
// The listings page by cursor, not offset: a pass rewrites the rows it walks,
// so they leave the listing's predicate and an offset would skip rows. The
// uncompressed listing filters on the thresholds so copies under the size
// floor, or already measured as missing the ratio, are not fetched again.
type CompressionAdmin interface {
	ListUncompressedLocations(ctx context.Context, limit int, after Cursor, t CompressionThresholds, backend string) ([]RewritableLocation, error)
	ListCompressedLocations(ctx context.Context, limit int, after Cursor, backend string) ([]RewritableLocation, error)
	MarkObjectCompressed(ctx context.Context, u *CompressedUpdate, previousSize int64) error
	RecordCompressionProbe(ctx context.Context, probe *CompressionProbe) error
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
