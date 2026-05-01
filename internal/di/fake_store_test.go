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

	"github.com/afreidah/s3-orchestrator/internal/store"
)

type fakeConcreteStore struct{}

func (fakeConcreteStore) GetAllObjectLocations(context.Context, string) ([]store.ObjectLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) RecordObject(context.Context, string, string, int64, *store.EncryptionMeta) ([]store.DeletedCopy, error) {
	return nil, nil
}
func (fakeConcreteStore) RecordObjectAndClearPending(context.Context, string, string, int64, *store.EncryptionMeta, string) ([]store.DeletedCopy, error) {
	return nil, nil
}
func (fakeConcreteStore) DeleteObject(context.Context, string) ([]store.DeletedCopy, error) {
	return nil, nil
}
func (fakeConcreteStore) ListObjects(context.Context, string, string, int) (*store.ListObjectsResult, error) {
	return nil, nil
}
func (fakeConcreteStore) ListObjectsByBackend(context.Context, string, int) ([]store.ObjectLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) ListObjectsByBackendKeyAsc(context.Context, string, string, int) ([]store.ObjectLocation, error) {
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
func (fakeConcreteStore) GetQuotaStats(context.Context) (map[string]store.QuotaStat, error) {
	return nil, nil
}

func (fakeConcreteStore) CreateMultipartUpload(context.Context, string, string, string, string, map[string]string) error {
	return nil
}
func (fakeConcreteStore) GetMultipartUpload(context.Context, string) (*store.MultipartUpload, error) {
	return nil, nil
}
func (fakeConcreteStore) RecordPart(context.Context, string, int, string, int64, *store.EncryptionMeta) error {
	return nil
}
func (fakeConcreteStore) GetParts(context.Context, string) ([]store.MultipartPart, error) {
	return nil, nil
}
func (fakeConcreteStore) DeleteMultipartUpload(context.Context, string) error { return nil }
func (fakeConcreteStore) ListMultipartUploads(context.Context, string, int) ([]store.MultipartUpload, error) {
	return nil, nil
}
func (fakeConcreteStore) CountActiveMultipartUploads(context.Context, string) (int64, error) {
	return 0, nil
}
func (fakeConcreteStore) GetStaleMultipartUploads(context.Context, time.Duration) ([]store.MultipartUpload, error) {
	return nil, nil
}
func (fakeConcreteStore) GetMultipartUploadsByBackend(context.Context, string) ([]store.MultipartUpload, error) {
	return nil, nil
}

func (fakeConcreteStore) GetUnderReplicatedObjects(context.Context, int, int) ([]store.ObjectLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) GetUnderReplicatedObjectsExcluding(context.Context, int, int, []string) ([]store.ObjectLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) RecordReplica(context.Context, string, string, string, int64) (bool, error) {
	return false, nil
}
func (fakeConcreteStore) GetOverReplicatedObjects(context.Context, int, int) ([]store.ObjectLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) CountOverReplicatedObjects(context.Context, int) (int64, error) {
	return 0, nil
}
func (fakeConcreteStore) RemoveExcessCopy(context.Context, string, string, int64) error { return nil }

func (fakeConcreteStore) EnqueueCleanup(context.Context, string, string, string, int64) error {
	return nil
}
func (fakeConcreteStore) GetPendingCleanups(context.Context, int) ([]store.CleanupItem, error) {
	return nil, nil
}
func (fakeConcreteStore) CompleteCleanupItem(context.Context, int64) error { return nil }
func (fakeConcreteStore) RetryCleanupItem(context.Context, int64, time.Duration, string) error {
	return nil
}
func (fakeConcreteStore) CleanupQueueDepth(context.Context) (int64, error)          { return 0, nil }
func (fakeConcreteStore) IncrementOrphanBytes(context.Context, string, int64) error { return nil }
func (fakeConcreteStore) DecrementOrphanBytes(context.Context, string, int64) error { return nil }

func (fakeConcreteStore) GetRandomHashedObjects(context.Context, int) ([]store.ObjectLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) GetObjectsWithoutHash(context.Context, int, int) ([]store.ObjectLocation, error) {
	return nil, nil
}
func (fakeConcreteStore) UpdateContentHash(context.Context, string, string, string) error {
	return nil
}

func (fakeConcreteStore) ListExpiredObjects(context.Context, string, time.Time, int) ([]store.ObjectLocation, error) {
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
func (fakeConcreteStore) GetUsageForPeriod(context.Context, string) (map[string]store.UsageStat, error) {
	return nil, nil
}
func (fakeConcreteStore) ListDirectoryChildren(context.Context, string, string, int) (*store.DirectoryListResult, error) {
	return nil, nil
}

func (fakeConcreteStore) FlushUsageDeltas(context.Context, string, string, int64, int64, int64) error {
	return nil
}
func (fakeConcreteStore) WithAdvisoryLock(context.Context, int64, func(ctx context.Context) error) (bool, error) {
	return false, nil
}

func (fakeConcreteStore) InsertPending(context.Context, *store.PendingObject) error { return nil }
func (fakeConcreteStore) DeletePending(context.Context, string) error               { return nil }
func (fakeConcreteStore) GetStalePending(context.Context, time.Time, int) ([]store.PendingObject, error) {
	return nil, nil
}
func (fakeConcreteStore) PromotePending(context.Context, *store.PendingObject) (store.PendingPromoteResult, []store.DeletedCopy, error) {
	return 0, nil, nil
}
func (fakeConcreteStore) PendingDepth(context.Context) (int64, error)         { return 0, nil }
func (fakeConcreteStore) DeletePendingByBackend(context.Context, string) error { return nil }
