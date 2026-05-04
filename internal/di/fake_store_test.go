// -------------------------------------------------------------------------------
// fakeConcreteStore  -  minimal test double for DI happy-path tests
//
// Author: Alex Freidah
//
// fakeConcreteStore satisfies every narrow store role interface so the
// narrow-provider happy-path tests can wire it into an injector without
// opening a real Postgres or SQLite connection. All methods return empty
// zero values  -  the tests here care about *wiring*, not behaviour.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// fakeConcreteStore is a no-op store double used by DI happy-path
// tests. It satisfies every narrow store role interface so do.Provide
// can wire dependencies without an actual database, and every method
// returns the zero value.
type fakeConcreteStore struct{}

// GetAllObjectLocations is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetAllObjectLocations(context.Context, string) ([]core.ObjectLocation, error) {
	return nil, nil
}
// GetObjectBackendsForKeys is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetObjectBackendsForKeys(context.Context, []string) (map[string][]string, error) {
	return nil, nil
}
// RecordObject is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) RecordObject(context.Context, string, string, int64, *core.EncryptionMeta) ([]core.DeletedCopy, error) {
	return nil, nil
}
// RecordObjectAndClearPending is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) RecordObjectAndClearPending(context.Context, string, string, int64, *core.EncryptionMeta, string) ([]core.DeletedCopy, error) {
	return nil, nil
}
// DeleteObject is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) DeleteObject(context.Context, string) ([]core.DeletedCopy, error) {
	return nil, nil
}
// DeleteObjectsBatch is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) DeleteObjectsBatch(context.Context, []string) (map[string][]core.DeletedCopy, error) {
	return nil, nil
}
// ListObjects is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) ListObjects(context.Context, string, string, int) (*core.ListObjectsResult, error) {
	return nil, nil
}
// ListObjectsByBackend is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) ListObjectsByBackend(context.Context, string, int) ([]core.ObjectLocation, error) {
	return nil, nil
}
// ListObjectsByBackendKeyAsc is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) ListObjectsByBackendKeyAsc(context.Context, string, string, int) ([]core.ObjectLocation, error) {
	return nil, nil
}
// MoveObjectLocation is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) MoveObjectLocation(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
// ImportObject is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) ImportObject(context.Context, string, string, int64) (bool, error) {
	return false, nil
}
// DeleteObjectLocation is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) DeleteObjectLocation(context.Context, string, string) error { return nil }

// GetBackendWithSpace is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetBackendWithSpace(context.Context, int64, []string) (string, error) {
	return "", nil
}
// GetLeastUtilizedBackend is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetLeastUtilizedBackend(context.Context, int64, []string) (string, error) {
	return "", nil
}
// GetQuotaStats is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetQuotaStats(context.Context) (map[string]core.QuotaStat, error) {
	return nil, nil
}

// CreateMultipartUpload is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) CreateMultipartUpload(context.Context, string, string, string, string, map[string]string) error {
	return nil
}
// GetMultipartUpload is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetMultipartUpload(context.Context, string) (*core.MultipartUpload, error) {
	return nil, nil
}
// RecordPart is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) RecordPart(context.Context, string, int, string, int64, *core.EncryptionMeta) error {
	return nil
}
// GetParts is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetParts(context.Context, string) ([]core.MultipartPart, error) {
	return nil, nil
}
// DeleteMultipartUpload is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) DeleteMultipartUpload(context.Context, string) error { return nil }
// ListMultipartUploads is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) ListMultipartUploads(context.Context, string, int) ([]core.MultipartUpload, error) {
	return nil, nil
}
// CountActiveMultipartUploads is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) CountActiveMultipartUploads(context.Context, string) (int64, error) {
	return 0, nil
}
// GetStaleMultipartUploads is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetStaleMultipartUploads(context.Context, time.Duration) ([]core.MultipartUpload, error) {
	return nil, nil
}
// GetMultipartUploadsByBackend is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetMultipartUploadsByBackend(context.Context, string) ([]core.MultipartUpload, error) {
	return nil, nil
}

// GetUnderReplicatedObjects is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetUnderReplicatedObjects(context.Context, int, int) ([]core.ObjectLocation, error) {
	return nil, nil
}
// GetUnderReplicatedObjectsExcluding is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetUnderReplicatedObjectsExcluding(context.Context, int, int, []string) ([]core.ObjectLocation, error) {
	return nil, nil
}
// RecordReplica is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) RecordReplica(context.Context, string, string, string) (int64, bool, error) {
	return 0, false, nil
}
// GetOverReplicatedObjects is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetOverReplicatedObjects(context.Context, int, int) ([]core.ObjectLocation, error) {
	return nil, nil
}
// CountOverReplicatedObjects is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) CountOverReplicatedObjects(context.Context, int) (int64, error) {
	return 0, nil
}
// RemoveExcessCopy is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) RemoveExcessCopy(context.Context, string, string, int64) error { return nil }

// EnqueueCleanup is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) EnqueueCleanup(context.Context, string, string, string, int64) error {
	return nil
}
// GetPendingCleanups is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetPendingCleanups(context.Context, int) ([]core.CleanupItem, error) {
	return nil, nil
}
// CompleteCleanupItem is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) CompleteCleanupItem(context.Context, int64) error { return nil }
// RetryCleanupItem is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) RetryCleanupItem(context.Context, int64, time.Duration, string) error {
	return nil
}
// CleanupQueueDepth is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) CleanupQueueDepth(context.Context) (int64, error) { return 0, nil }
// CleanupDLQDepth is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) CleanupDLQDepth(context.Context) (int64, error)   { return 0, nil }
// MoveCleanupToDLQ is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) MoveCleanupToDLQ(context.Context, int64, string) (bool, error) {
	return true, nil
}
// IncrementOrphanBytes is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) IncrementOrphanBytes(context.Context, string, int64) error { return nil }
// DecrementOrphanBytes is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) DecrementOrphanBytes(context.Context, string, int64) error { return nil }
// SweepStaleCleanupQueueRows is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) SweepStaleCleanupQueueRows(context.Context, string, string) (int64, error) {
	return 0, nil
}

// GetRandomHashedObjects is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetRandomHashedObjects(context.Context, int) ([]core.ObjectLocation, error) {
	return nil, nil
}
// GetObjectsWithoutHash is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetObjectsWithoutHash(context.Context, int, int) ([]core.ObjectLocation, error) {
	return nil, nil
}
// UpdateContentHash is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) UpdateContentHash(context.Context, string, string, string) error {
	return nil
}

// ListExpiredObjects is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) ListExpiredObjects(context.Context, string, time.Time, int) ([]core.ObjectLocation, error) {
	return nil, nil
}

// BackendObjectStats is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) BackendObjectStats(context.Context, string) (int64, int64, error) {
	return 0, 0, nil
}
// DeleteBackendData is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) DeleteBackendData(context.Context, string) error { return nil }

// GetObjectCounts is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetObjectCounts(context.Context) (map[string]int64, error) {
	return nil, nil
}
// GetActiveMultipartCounts is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetActiveMultipartCounts(context.Context) (map[string]int64, error) {
	return nil, nil
}
// GetUsageForPeriod is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetUsageForPeriod(context.Context, string) (map[string]core.UsageStat, error) {
	return nil, nil
}
// ListDirectoryChildren is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) ListDirectoryChildren(context.Context, string, string, int) (*core.DirectoryListResult, error) {
	return nil, nil
}

// FlushUsageDeltas is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) FlushUsageDeltas(context.Context, string, string, int64, int64, int64) error {
	return nil
}
// WithAdvisoryLock runs the supplied function with advisory lock.
func (fakeConcreteStore) WithAdvisoryLock(context.Context, int64, func(ctx context.Context) error) (bool, error) {
	return false, nil
}

// InsertPending is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) InsertPending(context.Context, *core.PendingObject) error { return nil }
// DeletePending is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) DeletePending(context.Context, string) error              { return nil }
// GetStalePending is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetStalePending(context.Context, time.Time, int) ([]core.PendingObject, error) {
	return nil, nil
}
// PromotePending is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) PromotePending(context.Context, *core.PendingObject) (core.PendingPromoteResult, []core.DeletedCopy, error) {
	return 0, nil, nil
}
// PendingDepth is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) PendingDepth(context.Context) (int64, error)          { return 0, nil }
// DeletePendingByBackend is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) DeletePendingByBackend(context.Context, string) error { return nil }

// LifecycleAdmin
// RunMigrations is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
// RunMigrations is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) RunMigrations(context.Context) error                              { return nil }
// VerifySchemaVersion is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) VerifySchemaVersion(context.Context) error                        { return nil }
// SyncQuotaLimits is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) SyncQuotaLimits(context.Context, []config.BackendConfig) error    { return nil }
// Close is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) Close()                                                           {}

// EncryptionAdmin
// ListEncryptedLocations is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
// ListEncryptedLocations is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) ListEncryptedLocations(context.Context, string, int, int) ([]core.EncryptedLocation, error) {
	return nil, nil
}
// UpdateEncryptionKey is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) UpdateEncryptionKey(context.Context, string, string, []byte, string) error {
	return nil
}
// ListUnencryptedLocations is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) ListUnencryptedLocations(context.Context, int, int) ([]core.UnencryptedLocation, error) {
	return nil, nil
}
// MarkObjectEncrypted is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) MarkObjectEncrypted(context.Context, string, string, []byte, string, int64, int64) error {
	return nil
}
// ListAllEncryptedLocations is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) ListAllEncryptedLocations(context.Context, int, int) ([]core.DecryptableLocation, error) {
	return nil, nil
}
// MarkObjectDecrypted is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) MarkObjectDecrypted(context.Context, string, string, int64) error {
	return nil
}

// NotificationOutbox
// InsertNotification is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
// InsertNotification is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) InsertNotification(context.Context, string, string, string) error {
	return nil
}
// GetPendingNotifications is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) GetPendingNotifications(context.Context, int) ([]core.NotificationRow, error) {
	return nil, nil
}
// CompleteNotification is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) CompleteNotification(context.Context, int64) error { return nil }
// RetryNotification is a stub on fakeConcreteStore; returns the zero value to
// satisfy the concreteStore interface contract.
func (fakeConcreteStore) RetryNotification(context.Context, int64, time.Duration, string) error {
	return nil
}
