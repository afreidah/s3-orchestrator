// -------------------------------------------------------------------------------
// fakeConcreteStore — minimal test double for DI happy-path tests
//
// Author: Alex Freidah
//
// fakeConcreteStore satisfies every narrow store role interface so the
// narrow-provider happy-path tests can wire it into an injector without
// opening a real Postgres or SQLite connection. All methods return empty
// zero values — the tests here care about *wiring*, not behaviour.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

type fakeConcreteStore struct{}

func (fakeConcreteStore) GetAllObjectLocations(context.Context, string) ([]core.ObjectLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) GetObjectBackendsForKeys(context.Context, []string) (map[string][]string, error) {
	return nil, nil
}
func (fakeConcreteStore) RecordObject(context.Context, string, string, int64, *core.EncryptionMeta) ([]core.DeletedCopy, error) {
	return nil, nil
}
func (fakeConcreteStore) RecordObjectAndClearPending(context.Context, string, string, int64, *core.EncryptionMeta, string) ([]core.DeletedCopy, error) {
	return nil, nil
}
func (fakeConcreteStore) DeleteObject(context.Context, string) ([]core.DeletedCopy, error) {
	return nil, nil
}
func (fakeConcreteStore) DeleteObjectsBatch(context.Context, []string) (map[string][]core.DeletedCopy, error) {
	return nil, nil
}
func (fakeConcreteStore) ListObjects(context.Context, string, string, int) (*core.ListObjectsResult, error) {
	return nil, nil
}
func (fakeConcreteStore) ListObjectsByBackend(context.Context, string, int) ([]core.ObjectLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) ListObjectsByBackendKeyAsc(context.Context, string, string, int) ([]core.ObjectLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) MoveObjectLocation(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
func (fakeConcreteStore) ImportObject(context.Context, string, string, int64) (bool, error) {
	return false, nil
}
func (fakeConcreteStore) DeleteObjectLocation(context.Context, string, string) error { return nil }

func (fakeConcreteStore) GetBackendWithSpace(context.Context, int64, []string) (string, error) {
	return "", nil
}
func (fakeConcreteStore) GetLeastUtilizedBackend(context.Context, int64, []string) (string, error) {
	return "", nil
}
func (fakeConcreteStore) GetQuotaStats(context.Context) (map[string]core.QuotaStat, error) {
	return nil, nil
}

func (fakeConcreteStore) CreateMultipartUpload(context.Context, string, string, string, string, map[string]string) error {
	return nil
}
func (fakeConcreteStore) GetMultipartUpload(context.Context, string) (*core.MultipartUpload, error) {
	return nil, nil
}
func (fakeConcreteStore) RecordPart(context.Context, string, int, string, int64, *core.EncryptionMeta) error {
	return nil
}
func (fakeConcreteStore) GetParts(context.Context, string) ([]core.MultipartPart, error) {
	return nil, nil
}
func (fakeConcreteStore) DeleteMultipartUpload(context.Context, string) error { return nil }
func (fakeConcreteStore) ListMultipartUploads(context.Context, string, int) ([]core.MultipartUpload, error) {
	return nil, nil
}
func (fakeConcreteStore) CountActiveMultipartUploads(context.Context, string) (int64, error) {
	return 0, nil
}
func (fakeConcreteStore) GetStaleMultipartUploads(context.Context, time.Duration) ([]core.MultipartUpload, error) {
	return nil, nil
}
func (fakeConcreteStore) GetMultipartUploadsByBackend(context.Context, string) ([]core.MultipartUpload, error) {
	return nil, nil
}

func (fakeConcreteStore) GetUnderReplicatedObjects(context.Context, int, int) ([]core.ObjectLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) GetUnderReplicatedObjectsExcluding(context.Context, int, int, []string) ([]core.ObjectLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) RecordReplica(context.Context, string, string, string) (int64, bool, error) {
	return 0, false, nil
}
func (fakeConcreteStore) GetOverReplicatedObjects(context.Context, int, int) ([]core.ObjectLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) CountOverReplicatedObjects(context.Context, int) (int64, error) {
	return 0, nil
}
func (fakeConcreteStore) RemoveExcessCopy(context.Context, string, string, int64) error { return nil }

func (fakeConcreteStore) EnqueueCleanup(context.Context, string, string, string, int64) error {
	return nil
}
func (fakeConcreteStore) GetPendingCleanups(context.Context, int) ([]core.CleanupItem, error) {
	return nil, nil
}
func (fakeConcreteStore) CompleteCleanupItem(context.Context, int64) error { return nil }
func (fakeConcreteStore) RetryCleanupItem(context.Context, int64, time.Duration, string) error {
	return nil
}
func (fakeConcreteStore) CleanupQueueDepth(context.Context) (int64, error) { return 0, nil }
func (fakeConcreteStore) CleanupDLQDepth(context.Context) (int64, error)   { return 0, nil }
func (fakeConcreteStore) MoveCleanupToDLQ(context.Context, int64, string) (bool, error) {
	return true, nil
}
func (fakeConcreteStore) IncrementOrphanBytes(context.Context, string, int64) error { return nil }
func (fakeConcreteStore) DecrementOrphanBytes(context.Context, string, int64) error { return nil }
func (fakeConcreteStore) SweepStaleCleanupQueueRows(context.Context, string, string) (int64, error) {
	return 0, nil
}

func (fakeConcreteStore) GetRandomHashedObjects(context.Context, int) ([]core.ObjectLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) GetObjectsWithoutHash(context.Context, int, int) ([]core.ObjectLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) UpdateContentHash(context.Context, string, string, string) error {
	return nil
}

func (fakeConcreteStore) ListExpiredObjects(context.Context, string, time.Time, int) ([]core.ObjectLocation, error) {
	return nil, nil
}

func (fakeConcreteStore) BackendObjectStats(context.Context, string) (int64, int64, error) {
	return 0, 0, nil
}
func (fakeConcreteStore) DeleteBackendData(context.Context, string) error { return nil }

func (fakeConcreteStore) GetObjectCounts(context.Context) (map[string]int64, error) {
	return nil, nil
}
func (fakeConcreteStore) GetActiveMultipartCounts(context.Context) (map[string]int64, error) {
	return nil, nil
}
func (fakeConcreteStore) GetUsageForPeriod(context.Context, string) (map[string]core.UsageStat, error) {
	return nil, nil
}
func (fakeConcreteStore) ListDirectoryChildren(context.Context, string, string, int) (*core.DirectoryListResult, error) {
	return nil, nil
}

func (fakeConcreteStore) FlushUsageDeltas(context.Context, string, string, int64, int64, int64) error {
	return nil
}
func (fakeConcreteStore) WithAdvisoryLock(context.Context, int64, func(ctx context.Context) error) (bool, error) {
	return false, nil
}

func (fakeConcreteStore) InsertPending(context.Context, *core.PendingObject) error { return nil }
func (fakeConcreteStore) DeletePending(context.Context, string) error              { return nil }
func (fakeConcreteStore) GetStalePending(context.Context, time.Time, int) ([]core.PendingObject, error) {
	return nil, nil
}
func (fakeConcreteStore) PromotePending(context.Context, *core.PendingObject) (core.PendingPromoteResult, []core.DeletedCopy, error) {
	return 0, nil, nil
}
func (fakeConcreteStore) PendingDepth(context.Context) (int64, error)          { return 0, nil }
func (fakeConcreteStore) DeletePendingByBackend(context.Context, string) error { return nil }

// LifecycleAdmin
func (fakeConcreteStore) RunMigrations(context.Context) error                              { return nil }
func (fakeConcreteStore) VerifySchemaVersion(context.Context) error                        { return nil }
func (fakeConcreteStore) SyncQuotaLimits(context.Context, []config.BackendConfig) error    { return nil }
func (fakeConcreteStore) Close()                                                           {}

// EncryptionAdmin
func (fakeConcreteStore) ListEncryptedLocations(context.Context, string, int, int) ([]core.EncryptedLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) UpdateEncryptionKey(context.Context, string, string, []byte, string) error {
	return nil
}
func (fakeConcreteStore) ListUnencryptedLocations(context.Context, int, int) ([]core.UnencryptedLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) MarkObjectEncrypted(context.Context, string, string, []byte, string, int64, int64) error {
	return nil
}
func (fakeConcreteStore) ListAllEncryptedLocations(context.Context, int, int) ([]core.DecryptableLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) MarkObjectDecrypted(context.Context, string, string, int64) error {
	return nil
}

// NotificationOutbox
func (fakeConcreteStore) InsertNotification(context.Context, string, string, string) error {
	return nil
}
func (fakeConcreteStore) GetPendingNotifications(context.Context, int) ([]core.NotificationRow, error) {
	return nil, nil
}
func (fakeConcreteStore) CompleteNotification(context.Context, int64) error { return nil }
func (fakeConcreteStore) RetryNotification(context.Context, int64, time.Duration, string) error {
	return nil
}
