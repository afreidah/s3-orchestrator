// -------------------------------------------------------------------------------
// MetadataStore - Interface for Object Metadata Persistence
//
// Author: Alex Freidah
//
// Defines the contract between the BackendManager and its metadata store.
// Implemented by Store (real PostgreSQL) and CircuitBreakerStore (degraded
// wrapper). Startup-only methods (RunMigrations, SyncQuotaLimits, Close) live
// on the concrete Store, not the interface.
// -------------------------------------------------------------------------------

package store

//go:generate mockgen -destination=mock_generated_test.go -package=store github.com/afreidah/s3-orchestrator/internal/store MetadataStore

import (
	"context"
	"errors"
	"time"
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
// INTERFACE
// -------------------------------------------------------------------------

// MetadataStore defines the contract for object metadata and quota persistence.
// All methods called by BackendManager at request time or in background tasks
// are included. Startup-only operations remain on the concrete Store type.
type MetadataStore interface {
	// --- Object location operations ---
	GetAllObjectLocations(ctx context.Context, key string) ([]ObjectLocation, error)
	RecordObject(ctx context.Context, key, backend string, size int64, enc *EncryptionMeta) ([]DeletedCopy, error)
	DeleteObject(ctx context.Context, key string) ([]DeletedCopy, error)
	ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*ListObjectsResult, error)

	// --- Quota operations ---
	GetBackendWithSpace(ctx context.Context, size int64, backendOrder []string) (string, error)
	GetLeastUtilizedBackend(ctx context.Context, size int64, eligible []string) (string, error)

	// --- Multipart operations ---
	CreateMultipartUpload(ctx context.Context, uploadID, key, backend, contentType string, metadata map[string]string) error
	GetMultipartUpload(ctx context.Context, uploadID string) (*MultipartUpload, error)
	RecordPart(ctx context.Context, uploadID string, partNumber int, etag string, size int64, enc *EncryptionMeta) error
	GetParts(ctx context.Context, uploadID string) ([]MultipartPart, error)
	DeleteMultipartUpload(ctx context.Context, uploadID string) error
	ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]MultipartUpload, error)

	// --- Directory listing (dashboard) ---
	ListDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*DirectoryListResult, error)

	// --- Lifecycle operations ---
	ListExpiredObjects(ctx context.Context, prefix string, cutoff time.Time, limit int) ([]ObjectLocation, error)

	// --- Background operations (metrics, cleanup, rebalance, replication) ---
	GetQuotaStats(ctx context.Context) (map[string]QuotaStat, error)
	GetObjectCounts(ctx context.Context) (map[string]int64, error)
	GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error)
	GetStaleMultipartUploads(ctx context.Context, olderThan time.Duration) ([]MultipartUpload, error)
	ListObjectsByBackend(ctx context.Context, backendName string, limit int) ([]ObjectLocation, error)
	MoveObjectLocation(ctx context.Context, key, fromBackend, toBackend string) (int64, error)
	GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error)
	GetUnderReplicatedObjectsExcluding(ctx context.Context, factor, limit int, excludedBackends []string) ([]ObjectLocation, error)
	RecordReplica(ctx context.Context, key, targetBackend, sourceBackend string, size int64) (bool, error)
	GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error)
	CountOverReplicatedObjects(ctx context.Context, factor int) (int64, error)
	RemoveExcessCopy(ctx context.Context, key, backendName string, size int64) error
	// --- Usage tracking operations ---
	FlushUsageDeltas(ctx context.Context, backendName, period string, apiRequests, egressBytes, ingressBytes int64) error
	GetUsageForPeriod(ctx context.Context, period string) (map[string]UsageStat, error)

	// --- Cleanup queue operations ---
	EnqueueCleanup(ctx context.Context, backendName, objectKey, reason string, sizeBytes int64) error
	GetPendingCleanups(ctx context.Context, limit int) ([]CleanupItem, error)
	CompleteCleanupItem(ctx context.Context, id int64) error
	RetryCleanupItem(ctx context.Context, id int64, backoff time.Duration, lastError string) error
	CleanupQueueDepth(ctx context.Context) (int64, error)

	// --- Orphan bytes tracking ---
	IncrementOrphanBytes(ctx context.Context, backendName string, amount int64) error
	DecrementOrphanBytes(ctx context.Context, backendName string, amount int64) error

	// --- Advisory lock (multi-instance coordination) ---
	WithAdvisoryLock(ctx context.Context, lockID int64, fn func(ctx context.Context) error) (bool, error)

	// --- Import (sync CLI) ---
	ImportObject(ctx context.Context, key, backend string, size int64) (bool, error)

	// --- Backend lifecycle ---
	BackendObjectStats(ctx context.Context, backendName string) (int64, int64, error)
	DeleteBackendData(ctx context.Context, backendName string) error
	DeleteObjectLocation(ctx context.Context, key, backendName string) error
}

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

// CleanupItem represents a pending cleanup operation from the retry queue.
type CleanupItem struct {
	ID          int64
	BackendName string
	ObjectKey   string
	Reason      string
	Attempts    int32
	SizeBytes   int64
}

// -------------------------------------------------------------------------
// NARROW ROLE INTERFACES
// -------------------------------------------------------------------------

// MetricsStore defines the store methods used by MetricsCollector.
type MetricsStore interface {
	GetQuotaStats(ctx context.Context) (map[string]QuotaStat, error)
	GetObjectCounts(ctx context.Context) (map[string]int64, error)
	GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error)
	GetUsageForPeriod(ctx context.Context, period string) (map[string]UsageStat, error)
	GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]ObjectLocation, error)
}

// DashboardStore defines the store methods used by DashboardAggregator.
type DashboardStore interface {
	GetQuotaStats(ctx context.Context) (map[string]QuotaStat, error)
	GetObjectCounts(ctx context.Context) (map[string]int64, error)
	GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error)
	GetUsageForPeriod(ctx context.Context, period string) (map[string]UsageStat, error)
	ListDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*DirectoryListResult, error)
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

// -------------------------------------------------------------------------
// COMPILE-TIME CHECKS
// -------------------------------------------------------------------------

// Verify *Store satisfies MetadataStore.
var _ MetadataStore = (*Store)(nil)

// Verify MetadataStore satisfies all narrow interfaces.
var (
	_ MetricsStore   = (MetadataStore)(nil)
	_ DashboardStore = (MetadataStore)(nil)
	_ UsageFlusher   = (MetadataStore)(nil)
	_ AdvisoryLocker = (MetadataStore)(nil)
)

// GroupByKey groups a flat list of object locations into a map keyed by object_key.
func GroupByKey(locations []ObjectLocation) map[string][]ObjectLocation {
	m := make(map[string][]ObjectLocation, len(locations)/2)
	for _, loc := range locations {
		m[loc.ObjectKey] = append(m[loc.ObjectKey], loc)
	}
	return m
}
