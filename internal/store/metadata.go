// -------------------------------------------------------------------------------
// Store Role Interfaces
//
// Author: Alex Freidah
//
// Declares the narrow role interfaces that consumers request from DI. The
// concrete *Store (PostgreSQL) and sqlite.Store (SQLite) each satisfy every
// role — this package exposes no composed "god" interface; consumers only
// ever import the role they actually use.
//
// Startup/admin methods live on AdminStore and are resolved separately from
// the request-time roles.
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"errors"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// -------------------------------------------------------------------------
// SENTINEL ERRORS
// -------------------------------------------------------------------------

var (
	// ErrDBUnavailable is returned by CircuitBreakerStore when the circuit is
	// open (database is known to be down). The manager uses errors.Is checks
	// to trigger broadcast fallback (reads) or 503 rejection (writes).
	ErrDBUnavailable = errors.New("database unavailable")

	// ErrServiceUnavailable is returned to S3 clients when writes are rejected
	// during a database outage. The server layer's writeStorageError helper
	// translates this into the appropriate S3 XML error response.
	ErrServiceUnavailable = &S3Error{
		StatusCode: 503,
		Code:       "ServiceUnavailable",
		Message:    "database unavailable, writes are temporarily rejected",
	}

	// ErrInsufficientStorage is returned when no backend has enough quota.
	ErrInsufficientStorage = &S3Error{StatusCode: 507, Code: "InsufficientStorage", Message: "no backend has sufficient quota"}

	// ErrUsageLimitExceeded is returned when all backends holding an object
	// have exceeded their monthly usage limits.
	ErrUsageLimitExceeded = &S3Error{StatusCode: 429, Code: "SlowDown", Message: "monthly usage limit exceeded for all backends holding this object"}
)

// -------------------------------------------------------------------------
// NARROW ROLE INTERFACES
// -------------------------------------------------------------------------

// ObjectStore defines object location CRUD and listing operations.
type ObjectStore interface {
	GetAllObjectLocations(ctx context.Context, key string) ([]ObjectLocation, error)
	RecordObject(ctx context.Context, key, backend string, size int64, enc *EncryptionMeta) ([]DeletedCopy, error)
	DeleteObject(ctx context.Context, key string) ([]DeletedCopy, error)
	ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*ListObjectsResult, error)
	ListObjectsByBackend(ctx context.Context, backendName string, limit int) ([]ObjectLocation, error)
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
	CreateMultipartUpload(ctx context.Context, uploadID, key, backend, contentType string, metadata map[string]string) error
	GetMultipartUpload(ctx context.Context, uploadID string) (*MultipartUpload, error)
	RecordPart(ctx context.Context, uploadID string, partNumber int, etag string, size int64, enc *EncryptionMeta) error
	GetParts(ctx context.Context, uploadID string) ([]MultipartPart, error)
	DeleteMultipartUpload(ctx context.Context, uploadID string) error
	ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]MultipartUpload, error)
	CountActiveMultipartUploads(ctx context.Context, bucketPrefix string) (int64, error)
	GetStaleMultipartUploads(ctx context.Context, olderThan time.Duration) ([]MultipartUpload, error)
	GetMultipartUploadsByBackend(ctx context.Context, backendName string) ([]MultipartUpload, error)
}

// ReplicationStore defines replication management operations.
type ReplicationStore interface {
	GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error)
	GetUnderReplicatedObjectsExcluding(ctx context.Context, factor, limit int, excludedBackends []string) ([]ObjectLocation, error)
	RecordReplica(ctx context.Context, key, targetBackend, sourceBackend string, size int64) (bool, error)
	GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error)
	CountOverReplicatedObjects(ctx context.Context, factor int) (int64, error)
	RemoveExcessCopy(ctx context.Context, key, backendName string, size int64) error
}

// CleanupStore defines cleanup queue and orphan byte tracking operations.
type CleanupStore interface {
	EnqueueCleanup(ctx context.Context, backendName, objectKey, reason string, sizeBytes int64) error
	GetPendingCleanups(ctx context.Context, limit int) ([]CleanupItem, error)
	CompleteCleanupItem(ctx context.Context, id int64) error
	RetryCleanupItem(ctx context.Context, id int64, backoff time.Duration, lastError string) error
	CleanupQueueDepth(ctx context.Context) (int64, error)
	IncrementOrphanBytes(ctx context.Context, backendName string, amount int64) error
	DecrementOrphanBytes(ctx context.Context, backendName string, amount int64) error
}

// IntegrityStore defines content hash verification operations.
type IntegrityStore interface {
	GetRandomHashedObjects(ctx context.Context, limit int) ([]ObjectLocation, error)
	GetObjectsWithoutHash(ctx context.Context, limit, offset int) ([]ObjectLocation, error)
	UpdateContentHash(ctx context.Context, key, backendName, hash string) error
}

// LifecycleStore defines object lifecycle expiration operations.
type LifecycleStore interface {
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

// AdvisoryLocker defines the store method used by background services for
// leader election across instances.
type AdvisoryLocker interface {
	WithAdvisoryLock(ctx context.Context, lockID int64, fn func(ctx context.Context) error) (bool, error)
}

// DashboardStore defines the store methods used by the web UI dashboard
// aggregator. It groups the four aggregate counters with the lazy-loaded
// directory listing; nothing else needs this exact set.
type DashboardStore interface {
	GetQuotaStats(ctx context.Context) (map[string]QuotaStat, error)
	GetObjectCounts(ctx context.Context) (map[string]int64, error)
	GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error)
	GetUsageForPeriod(ctx context.Context, period string) (map[string]UsageStat, error)
	ListDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*DirectoryListResult, error)
}

// -------------------------------------------------------------------------
// ADMIN STORE INTERFACE
// -------------------------------------------------------------------------

// AdminStore defines startup, shutdown, and admin-only operations on the
// concrete store. Both the PostgreSQL and SQLite stores satisfy this
// interface. Methods here are NOT wrapped by CircuitBreakerStore.
type AdminStore interface {
	// Lifecycle
	RunMigrations(ctx context.Context) error
	VerifySchemaVersion(ctx context.Context) error
	SyncQuotaLimits(ctx context.Context, backends []config.BackendConfig) error
	Close()

	// Encryption admin operations
	ListEncryptedLocations(ctx context.Context, keyID string, limit, offset int) ([]EncryptedLocation, error)
	UpdateEncryptionKey(ctx context.Context, objectKey, backendName string, newEncryptionKey []byte, newKeyID string) error
	ListUnencryptedLocations(ctx context.Context, limit, offset int) ([]UnencryptedLocation, error)
	MarkObjectEncrypted(ctx context.Context, objectKey, backendName string, encryptionKey []byte, keyID string, plaintextSize, ciphertextSize int64) error
	ListAllEncryptedLocations(ctx context.Context, limit, offset int) ([]DecryptableLocation, error)
	MarkObjectDecrypted(ctx context.Context, objectKey, backendName string, plaintextSize int64) error

	// Notification outbox
	InsertNotification(ctx context.Context, eventType, payload, endpointURL string) error
	GetPendingNotifications(ctx context.Context, limit int) ([]NotificationRow, error)
	CompleteNotification(ctx context.Context, id int64) error
	RetryNotification(ctx context.Context, id int64, backoff time.Duration, lastError string) error

	// Advisory lock (needed by notifier worker for leader election)
	WithAdvisoryLock(ctx context.Context, lockID int64, fn func(ctx context.Context) error) (bool, error)
}

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// UsageLimits holds configurable monthly usage limits for a single backend.
// Zero means unlimited for that dimension.
type UsageLimits struct {
	APIRequestLimit  int64
	EgressByteLimit  int64
	IngressByteLimit int64
}

// UsageStat holds usage statistics for a single backend in a given period.
type UsageStat struct {
	APIRequests  int64
	EgressBytes  int64
	IngressBytes int64
}

// NotificationRow represents a pending notification in the outbox table.
type NotificationRow struct {
	ID          int64
	EventType   string
	Payload     []byte
	EndpointURL string
	Attempts    int32
}

// CleanupItem represents a pending cleanup operation from the retry queue.
type CleanupItem struct {
	ID          int64
	BackendName string
	ObjectKey   string
	Reason      string
	Attempts    int32
	SizeBytes   int64
}

// EncryptedLocation represents an encrypted object location for key rotation.
type EncryptedLocation struct {
	ObjectKey     string
	BackendName   string
	EncryptionKey []byte
	KeyID         string
}

// UnencryptedLocation represents an unencrypted object location.
type UnencryptedLocation struct {
	ObjectKey   string
	BackendName string
	SizeBytes   int64
}

// DecryptableLocation represents an encrypted object location with all
// metadata needed for decryption.
type DecryptableLocation struct {
	ObjectKey     string
	BackendName   string
	SizeBytes     int64
	EncryptionKey []byte
	KeyID         string
	PlaintextSize int64
}

// -------------------------------------------------------------------------
// COMPILE-TIME CHECKS
// -------------------------------------------------------------------------

// Verify *Store satisfies every narrow role interface and AdminStore. Adding
// a method to *Store that does not land on any of these interfaces is a
// signal to either slot it into an existing role or declare a new one —
// there is no composed union for it to hide inside.
var (
	_ ObjectStore           = (*Store)(nil)
	_ QuotaStore            = (*Store)(nil)
	_ MultipartStore        = (*Store)(nil)
	_ ReplicationStore      = (*Store)(nil)
	_ CleanupStore          = (*Store)(nil)
	_ IntegrityStore        = (*Store)(nil)
	_ LifecycleStore        = (*Store)(nil)
	_ BackendLifecycleStore = (*Store)(nil)
	_ DashboardStore        = (*Store)(nil)
	_ UsageFlusher          = (*Store)(nil)
	_ AdvisoryLocker        = (*Store)(nil)
	_ AdminStore            = (*Store)(nil)
)

// GroupByKey groups a flat list of object locations into a map keyed by object_key.
func GroupByKey(locations []ObjectLocation) map[string][]ObjectLocation {
	m := make(map[string][]ObjectLocation, len(locations)/2)
	for i := range locations {
		m[locations[i].ObjectKey] = append(m[locations[i].ObjectKey], locations[i])
	}
	return m
}
