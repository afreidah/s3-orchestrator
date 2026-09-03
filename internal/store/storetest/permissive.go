// -------------------------------------------------------------------------------
// Permissive Store Mock
//
// Author: Alex Freidah
//
// Stubs every store method a fixture might reach with an empty, successful
// answer, so a test that exercises one path does not have to state expectations
// for the twenty it merely passes through. Tests that care about a call
// override it afterwards.
// -------------------------------------------------------------------------------

package storetest

import (
	"context"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// Permissive registers a permissive .AnyTimes() expectation that returns
// zero values for every method on MockMetadataStore. Call it at the end
// of test setup, after registering specific stricter expectations: gomock
// matches expectations in declaration order, so specific stubs fire first
// (until their Times() bound is reached) and unstubbed methods fall
// through to the catch-all here. Lets tests opt into mocking only the
// methods they care about without the system under test panicking on
// incidental calls (orphan-bytes adjustments, sweep rows, advisory locks,
// etc.).
//
// helpers would be more boilerplate without meaningful structure.
//
//nolint:funlen // one stanza per interface method; expanding to per-method
func Permissive(m *MockMetadataStore) {
	r := m.EXPECT()
	a := gomock.Any()

	r.BackendObjectStats(a, a).Return(int64(0), int64(0), nil).AnyTimes()
	r.ClaimPendingCleanups(a, a, a, a).Return(nil, nil).AnyTimes()
	r.CleanupDLQDepth(a).Return(int64(0), nil).AnyTimes()
	r.CleanupQueueDepth(a).Return(int64(0), nil).AnyTimes()
	r.CompleteCleanupItem(a, a).Return(nil).AnyTimes()
	r.CompleteNotification(a, a).Return(nil).AnyTimes()
	r.CountActiveMultipartUploads(a, a).Return(int64(0), nil).AnyTimes()
	r.CountOverReplicatedObjects(a, a).Return(int64(0), nil).AnyTimes()
	r.CreateMultipartUpload(a, a).Return(nil).AnyTimes()
	r.DecrementOrphanBytes(a, a, a).Return(nil).AnyTimes()
	r.DeleteBackendData(a, a).Return(nil).AnyTimes()
	r.DeleteMultipartUpload(a, a).Return(nil).AnyTimes()
	r.DeleteObject(a, a).Return(nil, nil).AnyTimes()
	r.DeleteObjectLocation(a, a, a).Return(nil).AnyTimes()
	r.DeleteObjectTags(a, a).Return(nil).AnyTimes()
	r.GetObjectTags(a, a).Return(nil, nil).AnyTimes()
	r.CountObjectTags(a, a).Return(0, nil).AnyTimes()
	r.ReplaceObjectTags(a, a, a).Return(nil).AnyTimes()
	r.DeleteObjectsBatch(a, a).Return(nil, nil).AnyTimes()
	r.DeletePending(a, a).Return(nil).AnyTimes()
	r.DeletePendingByBackend(a, a).Return(nil).AnyTimes()
	r.EnqueueCleanup(a, a, a, a, a).Return(nil).AnyTimes()
	r.FlushUsageDeltas(a, a, a, a, a, a).Return(nil).AnyTimes()
	r.GetActiveMultipartCounts(a).Return(nil, nil).AnyTimes()
	r.GetAllObjectLocations(a, a).Return(nil, nil).AnyTimes()
	r.GetBackendWithSpace(a, a, a).Return("", nil).AnyTimes()
	r.GetLeastUtilizedBackend(a, a, a).Return("", nil).AnyTimes()
	r.GetMultipartUpload(a, a).Return(nil, nil).AnyTimes()
	r.GetMultipartUploadsByBackend(a, a).Return(nil, nil).AnyTimes()
	r.GetObjectBackendsForKeys(a, a).Return(nil, nil).AnyTimes()
	r.GetObjectCounts(a).Return(nil, nil).AnyTimes()
	r.GetUnverifiedObjectCounts(a).Return(nil, nil).AnyTimes()
	r.GetObjectsWithoutHash(a, a, a).Return(nil, nil).AnyTimes()
	r.GetOverReplicatedObjects(a, a, a).Return(nil, nil).AnyTimes()
	r.GetParts(a, a).Return(nil, nil).AnyTimes()
	r.GetPendingCleanups(a, a).Return(nil, nil).AnyTimes()
	r.GetPendingNotifications(a, a).Return(nil, nil).AnyTimes()
	r.GetQuotaStats(a).Return(nil, nil).AnyTimes()
	r.GetLeastRecentlyScrubbedObjects(a, a, a).Return(nil, nil).AnyTimes()
	r.CountScrubCandidatesOnBackends(a, a).Return(int64(0), nil).AnyTimes()
	r.MarkObjectScrubbed(a, a, a).Return(nil).AnyTimes()
	r.IntegrityCoverage(a, a).Return(core.CoverageStat{}, nil).AnyTimes()
	r.CountUnencryptedLocations(a).Return(int64(0), nil).AnyTimes()
	r.GetStaleMultipartUploads(a, a).Return(nil, nil).AnyTimes()
	r.GetStalePending(a, a, a).Return(nil, nil).AnyTimes()
	r.GetUnderReplicatedObjects(a, a, a).Return(nil, nil).AnyTimes()
	r.GetUnderReplicatedObjectsExcluding(a, a, a, a).Return(nil, nil).AnyTimes()
	r.GetUsageForPeriod(a, a).Return(nil, nil).AnyTimes()
	r.GetPoolUsageForPeriod(a, a).Return(nil, nil).AnyTimes()
	r.ImportObject(a, a).Return(core.ImportSkippedExisting, nil).AnyTimes()
	r.IncrementOrphanBytes(a, a, a).Return(nil).AnyTimes()
	r.InsertNotification(a, a, a, a).Return(nil).AnyTimes()
	r.InsertPending(a, a).Return(nil).AnyTimes()
	r.ListAllEncryptedLocations(a, a, a).Return(nil, nil).AnyTimes()
	r.ListCleanupDLQ(a, a, a).Return(nil, nil).AnyTimes()
	r.ListDirectoryChildren(a, a, a, a).Return(nil, nil).AnyTimes()
	r.ListEncryptedLocations(a, a, a, a).Return(nil, nil).AnyTimes()
	r.ListExpiredObjects(a, a).Return(nil, nil).AnyTimes()
	r.ListMultipartUploads(a, a, a).Return(nil, nil).AnyTimes()
	r.ListObjects(a, a, a, a).Return(nil, nil).AnyTimes()
	r.ListObjectsDelimited(a, a, a, a, a).Return(&core.ListDelimitedResult{}, nil).AnyTimes()
	r.ListObjectsByBackend(a, a, a).Return(nil, nil).AnyTimes()
	r.ListObjectsByBackendKeyAsc(a, a, a, a).Return(nil, nil).AnyTimes()
	r.ListUnencryptedLocations(a, a, a).Return(nil, nil).AnyTimes()
	r.CompressionStats(a).Return(nil, nil).AnyTimes()
	r.ListCompressedLocations(a, a, a).Return(nil, nil).AnyTimes()
	r.ListUncompressedLocations(a, a, a, a).Return(nil, nil).AnyTimes()
	r.MarkObjectCompressed(a, a, a).Return(nil).AnyTimes()
	r.RecordCompressionProbe(a, a).Return(nil).AnyTimes()
	r.MarkObjectDecrypted(a, a, a, a).Return(nil).AnyTimes()
	r.MarkObjectEncrypted(a, a).Return(nil).AnyTimes()
	r.MoveCleanupToDLQ(a, a, a).Return(false, nil).AnyTimes()
	r.MoveObjectLocation(a, a, a, a).Return(int64(0), nil).AnyTimes()
	r.PendingDepth(a).Return(int64(0), nil).AnyTimes()
	r.PromotePending(a, a).Return(core.PendingPromoteResult(0), nil, nil).AnyTimes()
	r.RecordObject(a, a).Return(nil, nil).AnyTimes()
	r.RecordObjectIdentity(a, a, a).Return(nil).AnyTimes()
	r.RecordPart(a, a).Return(nil).AnyTimes()
	r.RecordReplica(a, a, a, a).Return(int64(0), false, nil).AnyTimes()
	r.ReconcileUsage(a).Return(nil, nil).AnyTimes()
	r.RemoveExcessCopy(a, a, a, a).Return(true, nil).AnyTimes()
	r.RequeueCleanupDLQ(a, a).Return(int64(0), nil).AnyTimes()
	r.RetryCleanupItem(a, a, a, a).Return(nil).AnyTimes()
	r.RetryNotification(a, a, a, a).Return(nil).AnyTimes()
	r.RunMigrations(a).Return(nil).AnyTimes()
	r.SweepStaleCleanupQueueRows(a, a, a).Return(int64(0), nil).AnyTimes()
	r.SyncQuotaLimits(a, a).Return(nil).AnyTimes()
	r.UpdateContentHash(a, a, a, a).Return(nil).AnyTimes()
	r.UpdateEncryptionKey(a, a, a, a, a).Return(nil).AnyTimes()
	r.VerifySchemaVersion(a).Return(nil).AnyTimes()
	r.WithAdvisoryLock(a, a, a).DoAndReturn(
		func(ctx context.Context, _ int64, fn func(context.Context) error) (bool, error) {
			if err := fn(ctx); err != nil {
				return false, err
			}
			return true, nil
		}).AnyTimes()
}
