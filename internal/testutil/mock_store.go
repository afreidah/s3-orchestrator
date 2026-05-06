// -------------------------------------------------------------------------------
// Shared Test Utilities - Mock Store and Helpers
//
// Author: Alex Freidah
//
// Provides a configurable MetadataStore mock shared across packages for unit
// testing. Supports pre-set responses, injectable errors, and call tracking
// fields for assertion in tests outside the storage package.
// -------------------------------------------------------------------------------

// Package testutil provides shared mock implementations and test helpers used
// across multiple packages.
package testutil

import (
	"context"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// MockStore is a configurable MetadataStore mock for unit testing.
// Each method returns its pre-configured response/error. Call tracking fields
// allow assertions on what the caller invoked.
type MockStore struct {
	Mu sync.Mutex

	// --- Configurable responses ---
	GetAllLocationsResp []core.ObjectLocation
	GetAllLocationsErr  error

	GetObjectBackendsForKeysResp map[string][]string
	GetObjectBackendsForKeysErr  error

	GetBackendResp string
	GetBackendErr  error

	RecordObjectErr error

	DeleteObjectResp []core.DeletedCopy
	DeleteObjectErr  error
	DeleteObjectFunc func(key string) ([]core.DeletedCopy, error)

	DeleteObjectsBatchResp  map[string][]core.DeletedCopy
	DeleteObjectsBatchErr   error
	DeleteObjectsBatchFunc  func(keys []string) (map[string][]core.DeletedCopy, error)
	DeleteObjectsBatchCalls [][]string

	ListObjectsResp  *core.ListObjectsResult
	ListObjectsPages []core.ListObjectsResult // for paginated tests
	ListObjectsErr   error

	// ListObjectsByBackendKeyAscFn is invoked by tests that drive the
	// ReconcileBackend sorted-merge with a deterministic page sequence.
	// Returning an empty slice signals end-of-stream.
	ListObjectsByBackendKeyAscFn func(afterKey string, limit int) ([]core.ObjectLocation, error)

	// Multipart
	CreateMultipartErr        error
	GetMultipartResp          *core.MultipartUpload
	GetMultipartErr           error
	GetPartsResp              []core.MultipartPart
	GetPartsErr               error
	DeleteMultipartErr        error
	RecordPartErr             error
	ListMultipartUploadsResp  []core.MultipartUpload
	ListMultipartUploadsErr   error
	LegacyMultipartResp       []core.MultipartUpload
	LegacyMultipartErr        error
	UpdateUploadEncryptionErr error
	UpdatePartEncryptionErr   error

	// Dashboard / background
	GetQuotaStatsResp      map[string]core.QuotaStat
	GetQuotaStatsErr       error
	GetObjectCountsResp    map[string]int64
	GetObjectCountsErr     error
	GetActiveMultipartResp map[string]int64
	GetActiveMultipartErr  error
	GetUsageForPeriodResp  map[string]core.UsageStat
	GetUsageForPeriodErr   error
	ListDirChildrenResp    *core.DirectoryListResult
	ListDirChildrenErr     error

	// Usage tracking
	FlushUsageErr   error
	FlushUsageCalls []FlushUsageCall

	// --- Call tracking ---
	RecordObjectCalls        []RecordObjectCall
	DeleteObjectCalls        []string
	CallCount                int
	GetBackendWithSpaceCalls int
	GetLeastUtilizedCalls    int
}

// RecordObjectCall captures arguments to RecordObject.
type RecordObjectCall struct {
	Key, Backend string
	Size         int64
}

// FlushUsageCall captures arguments to FlushUsageDeltas.
type FlushUsageCall struct {
	BackendName  string
	Period       string
	APIRequests  int64
	EgressBytes  int64
	IngressBytes int64
}

// Compile-time checks  -  MockStore must satisfy every narrow role interface
// so handler tests can hand it wherever a role is requested.
var (
	_ core.ObjectStore           = (*MockStore)(nil)
	_ core.QuotaStore            = (*MockStore)(nil)
	_ core.MultipartStore        = (*MockStore)(nil)
	_ core.ReplicationStore      = (*MockStore)(nil)
	_ core.CleanupStore          = (*MockStore)(nil)
	_ core.IntegrityStore        = (*MockStore)(nil)
	_ core.ExpiredObjectsLister  = (*MockStore)(nil)
	_ core.BackendLifecycleStore = (*MockStore)(nil)
	_ core.DashboardStore        = (*MockStore)(nil)
	_ core.UsageFlusher          = (*MockStore)(nil)
	_ core.AdvisoryLocker        = (*MockStore)(nil)
)

// GetAllObjectLocations returns the pre-configured locations or ErrObjectNotFound.
func (m *MockStore) GetAllObjectLocations(_ context.Context, key string) ([]core.ObjectLocation, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.CallCount++
	if m.GetAllLocationsErr != nil {
		return nil, m.GetAllLocationsErr
	}
	if m.GetAllLocationsResp != nil {
		return m.GetAllLocationsResp, nil
	}
	return nil, core.ErrObjectNotFound
}

// GetObjectBackendsForKeys returns an empty map by default; tests that
// need a specific response set m.GetObjectBackendsForKeysResp and the
// mock returns it verbatim (a copy of the supplied keys is not made).
func (m *MockStore) GetObjectBackendsForKeys(_ context.Context, _ []string) (map[string][]string, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.CallCount++
	if m.GetObjectBackendsForKeysErr != nil {
		return nil, m.GetObjectBackendsForKeysErr
	}
	if m.GetObjectBackendsForKeysResp != nil {
		return m.GetObjectBackendsForKeysResp, nil
	}
	return map[string][]string{}, nil
}

// GetBackendWithSpace returns the pre-configured backend name or error.
func (m *MockStore) GetBackendWithSpace(_ context.Context, size int64, _ []string) (string, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.GetBackendWithSpaceCalls++
	if m.GetBackendErr != nil {
		return "", m.GetBackendErr
	}
	return m.GetBackendResp, nil
}

// GetLeastUtilizedBackend returns the pre-configured backend name or error.
func (m *MockStore) GetLeastUtilizedBackend(_ context.Context, size int64, _ []string) (string, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.GetLeastUtilizedCalls++
	if m.GetBackendErr != nil {
		return "", m.GetBackendErr
	}
	return m.GetBackendResp, nil
}

// RecordObject records the call arguments and returns the pre-configured error.
func (m *MockStore) RecordObject(_ context.Context, key, backend string, size int64, _ *core.EncryptionMeta) ([]core.DeletedCopy, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.RecordObjectCalls = append(m.RecordObjectCalls, RecordObjectCall{Key: key, Backend: backend, Size: size})
	return nil, m.RecordObjectErr
}

// RecordObjectAndClearPending records the call alongside RecordObjectCalls so
// existing call-count assertions stay green. Returns the same pre-configured
// error as RecordObject.
func (m *MockStore) RecordObjectAndClearPending(_ context.Context, key, backend string, size int64, _ *core.EncryptionMeta, _ string) ([]core.DeletedCopy, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.RecordObjectCalls = append(m.RecordObjectCalls, RecordObjectCall{Key: key, Backend: backend, Size: size})
	return nil, m.RecordObjectErr
}

// DeleteObject records the call and returns the pre-configured response or error.
func (m *MockStore) DeleteObject(_ context.Context, key string) ([]core.DeletedCopy, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.DeleteObjectCalls = append(m.DeleteObjectCalls, key)
	if m.DeleteObjectFunc != nil {
		return m.DeleteObjectFunc(key)
	}
	if m.DeleteObjectErr != nil {
		return nil, m.DeleteObjectErr
	}
	return m.DeleteObjectResp, nil
}

// DeleteObjectsBatch records the keys, then returns the pre-configured
// response or error. Default returns an empty map (every key
// treated as not-found).
func (m *MockStore) DeleteObjectsBatch(_ context.Context, keys []string) (map[string][]core.DeletedCopy, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.DeleteObjectsBatchCalls = append(m.DeleteObjectsBatchCalls, keys)
	if m.DeleteObjectsBatchErr != nil {
		return nil, m.DeleteObjectsBatchErr
	}
	if m.DeleteObjectsBatchFunc != nil {
		return m.DeleteObjectsBatchFunc(keys)
	}
	if m.DeleteObjectsBatchResp != nil {
		return m.DeleteObjectsBatchResp, nil
	}
	return map[string][]core.DeletedCopy{}, nil
}

// ListObjects returns the next pre-configured page or the static response.
func (m *MockStore) ListObjects(_ context.Context, _, startAfter string, _ int) (*core.ListObjectsResult, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if m.ListObjectsErr != nil {
		return nil, m.ListObjectsErr
	}
	if len(m.ListObjectsPages) > 0 {
		page := m.ListObjectsPages[0]
		m.ListObjectsPages = m.ListObjectsPages[1:]
		return &page, nil
	}
	return m.ListObjectsResp, nil
}

// CreateMultipartUpload returns the pre-configured error.
func (m *MockStore) CreateMultipartUpload(_ context.Context, _ *core.CreateMultipartUploadParams) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	return m.CreateMultipartErr
}

// ListLegacyMultipartUploads returns the pre-configured slice + error.
func (m *MockStore) ListLegacyMultipartUploads(_ context.Context, _ int) ([]core.MultipartUpload, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	return m.LegacyMultipartResp, m.LegacyMultipartErr
}

// UpdateUploadEncryption returns the pre-configured error.
func (m *MockStore) UpdateUploadEncryption(_ context.Context, _ string, _ []byte, _ string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	return m.UpdateUploadEncryptionErr
}

// UpdatePartEncryption returns the pre-configured error.
func (m *MockStore) UpdatePartEncryption(_ context.Context, _ string, _ int, _ int64, _ *core.EncryptionMeta) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	return m.UpdatePartEncryptionErr
}

// GetMultipartUpload returns the pre-configured upload or error.
func (m *MockStore) GetMultipartUpload(_ context.Context, _ string) (*core.MultipartUpload, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if m.GetMultipartErr != nil {
		return nil, m.GetMultipartErr
	}
	return m.GetMultipartResp, nil
}

// RecordPart returns the pre-configured error.
func (m *MockStore) RecordPart(_ context.Context, _ string, _ int, _ string, _ int64, _ *core.EncryptionMeta) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	return m.RecordPartErr
}

// GetParts returns the pre-configured parts or error.
func (m *MockStore) GetParts(_ context.Context, _ string) ([]core.MultipartPart, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if m.GetPartsErr != nil {
		return nil, m.GetPartsErr
	}
	return m.GetPartsResp, nil
}

// DeleteMultipartUpload returns the pre-configured error.
func (m *MockStore) DeleteMultipartUpload(_ context.Context, _ string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	return m.DeleteMultipartErr
}

// ListDirectoryChildren returns the pre-configured directory listing or error.
func (m *MockStore) ListDirectoryChildren(_ context.Context, _, _ string, _ int) (*core.DirectoryListResult, error) {
	if m.ListDirChildrenErr != nil {
		return nil, m.ListDirChildrenErr
	}
	if m.ListDirChildrenResp != nil {
		return m.ListDirChildrenResp, nil
	}
	return &core.DirectoryListResult{}, nil
}

// GetQuotaStats returns the pre-configured quota stats or error.
func (m *MockStore) GetQuotaStats(_ context.Context) (map[string]core.QuotaStat, error) {
	if m.GetQuotaStatsErr != nil {
		return nil, m.GetQuotaStatsErr
	}
	if m.GetQuotaStatsResp != nil {
		return m.GetQuotaStatsResp, nil
	}
	return map[string]core.QuotaStat{}, nil
}

// GetObjectCounts returns the pre-configured object counts or error.
func (m *MockStore) GetObjectCounts(_ context.Context) (map[string]int64, error) {
	if m.GetObjectCountsErr != nil {
		return nil, m.GetObjectCountsErr
	}
	if m.GetObjectCountsResp != nil {
		return m.GetObjectCountsResp, nil
	}
	return map[string]int64{}, nil
}

// GetActiveMultipartCounts returns the pre-configured multipart counts or error.
func (m *MockStore) GetActiveMultipartCounts(_ context.Context) (map[string]int64, error) {
	if m.GetActiveMultipartErr != nil {
		return nil, m.GetActiveMultipartErr
	}
	if m.GetActiveMultipartResp != nil {
		return m.GetActiveMultipartResp, nil
	}
	return map[string]int64{}, nil
}

// GetStaleMultipartUploads returns nil (stub).
func (m *MockStore) GetStaleMultipartUploads(_ context.Context, _ time.Duration) ([]core.MultipartUpload, error) {
	return nil, nil
}

// GetMultipartUploadsByBackend returns nil (stub).
func (m *MockStore) GetMultipartUploadsByBackend(_ context.Context, _ string) ([]core.MultipartUpload, error) {
	return nil, nil
}

// ListMultipartUploads returns the pre-configured uploads or error.
func (m *MockStore) ListMultipartUploads(_ context.Context, _ string, _ int) ([]core.MultipartUpload, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if m.ListMultipartUploadsErr != nil {
		return nil, m.ListMultipartUploadsErr
	}
	return m.ListMultipartUploadsResp, nil
}

// CountActiveMultipartUploads returns zero (stub).
func (m *MockStore) CountActiveMultipartUploads(_ context.Context, _ string) (int64, error) {
	return 0, nil
}

// ListObjectsByBackend returns nil (stub).
func (m *MockStore) ListObjectsByBackend(_ context.Context, _ string, _ int) ([]core.ObjectLocation, error) {
	return nil, nil
}

// ListObjectsByBackendKeyAsc returns nil by default. Tests that exercise
// reconciliation paths can set ListObjectsByBackendKeyAscPages on the mock
// to drive the sorted-merge with deterministic responses.
func (m *MockStore) ListObjectsByBackendKeyAsc(_ context.Context, _ /*backend*/ string, afterKey string, limit int) ([]core.ObjectLocation, error) {
	if m.ListObjectsByBackendKeyAscFn != nil {
		return m.ListObjectsByBackendKeyAscFn(afterKey, limit)
	}
	return nil, nil
}

// MoveObjectLocation returns zero (stub).
func (m *MockStore) MoveObjectLocation(_ context.Context, _, _, _ string) (int64, error) {
	return 0, nil
}

// GetUnderReplicatedObjects returns nil (stub).
func (m *MockStore) GetUnderReplicatedObjects(_ context.Context, _, _ int) ([]core.ObjectLocation, error) {
	return nil, nil
}

// GetUnderReplicatedObjectsExcluding returns nil (stub).
func (m *MockStore) GetUnderReplicatedObjectsExcluding(_ context.Context, _, _ int, _ []string) ([]core.ObjectLocation, error) {
	return nil, nil
}

// RecordReplica returns (0, false, nil) (stub).
func (m *MockStore) RecordReplica(_ context.Context, _, _, _ string) (int64, bool, error) {
	return 0, false, nil
}

// FlushUsageDeltas records the call arguments and returns the pre-configured error.
func (m *MockStore) FlushUsageDeltas(_ context.Context, backendName, period string, apiRequests, egressBytes, ingressBytes int64) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.FlushUsageCalls = append(m.FlushUsageCalls, FlushUsageCall{
		BackendName:  backendName,
		Period:       period,
		APIRequests:  apiRequests,
		EgressBytes:  egressBytes,
		IngressBytes: ingressBytes,
	})
	return m.FlushUsageErr
}

// GetUsageForPeriod returns the pre-configured usage stats or error.
func (m *MockStore) GetUsageForPeriod(_ context.Context, _ string) (map[string]core.UsageStat, error) {
	if m.GetUsageForPeriodErr != nil {
		return nil, m.GetUsageForPeriodErr
	}
	if m.GetUsageForPeriodResp != nil {
		return m.GetUsageForPeriodResp, nil
	}
	return map[string]core.UsageStat{}, nil
}

// -------------------------------------------------------------------------
// CLEANUP QUEUE STUBS
// -------------------------------------------------------------------------

// EnqueueCleanup returns nil (stub).
func (m *MockStore) EnqueueCleanup(_ context.Context, _, _, _ string, _ int64) error {
	return nil
}

// IncrementOrphanBytes returns nil (stub).
func (m *MockStore) IncrementOrphanBytes(_ context.Context, _ string, _ int64) error {
	return nil
}

// DecrementOrphanBytes returns nil (stub).
// SweepStaleCleanupQueueRows is a stub returning (0, nil).
func (m *MockStore) SweepStaleCleanupQueueRows(_ context.Context, _, _ string) (int64, error) {
	return 0, nil
}

func (m *MockStore) DecrementOrphanBytes(_ context.Context, _ string, _ int64) error {
	return nil
}

// GetPendingCleanups returns nil (stub).
func (m *MockStore) GetPendingCleanups(_ context.Context, _ int) ([]core.CleanupItem, error) {
	return nil, nil
}

// CompleteCleanupItem returns nil (stub).
func (m *MockStore) CompleteCleanupItem(_ context.Context, _ int64) error {
	return nil
}

// RetryCleanupItem returns nil (stub).
func (m *MockStore) RetryCleanupItem(_ context.Context, _ int64, _ time.Duration, _ string) error {
	return nil
}

// CleanupQueueDepth returns zero (stub).
func (m *MockStore) CleanupQueueDepth(_ context.Context) (int64, error) {
	return 0, nil
}

// MoveCleanupToDLQ records nothing and reports the row as moved (stub).
func (m *MockStore) MoveCleanupToDLQ(_ context.Context, _ int64, _ string) (bool, error) {
	return true, nil
}

// CleanupDLQDepth returns zero (stub).
func (m *MockStore) CleanupDLQDepth(_ context.Context) (int64, error) {
	return 0, nil
}

// ListExpiredObjects returns nil (stub).
func (m *MockStore) ListExpiredObjects(_ context.Context, _ string, _ time.Time, _ int) ([]core.ObjectLocation, error) {
	return nil, nil
}

// WithAdvisoryLock executes the function directly without acquiring a lock.
func (m *MockStore) WithAdvisoryLock(_ context.Context, _ int64, fn func(ctx context.Context) error) (bool, error) {
	return true, fn(context.Background())
}

// ImportObject returns true (stub).
func (m *MockStore) ImportObject(_ context.Context, _, _ string, _ int64) (bool, error) {
	return true, nil
}

func (m *MockStore) BackendObjectStats(_ context.Context, _ string) (int64, int64, error) {
	return 0, 0, nil
}

func (m *MockStore) DeleteBackendData(_ context.Context, _ string) error {
	return nil
}

// DeleteObjectLocation removes a single object location (stub).
func (m *MockStore) DeleteObjectLocation(_ context.Context, _, _ string) error {
	return nil
}

// GetOverReplicatedObjects returns nil (stub).
func (m *MockStore) GetOverReplicatedObjects(_ context.Context, _, _ int) ([]core.ObjectLocation, error) {
	return nil, nil
}

// CountOverReplicatedObjects returns zero (stub).
func (m *MockStore) CountOverReplicatedObjects(_ context.Context, _ int) (int64, error) {
	return 0, nil
}

// RemoveExcessCopy returns nil (stub).
func (m *MockStore) RemoveExcessCopy(_ context.Context, _, _ string, _ int64) error {
	return nil
}

// GetRandomHashedObjects returns nil (stub).
func (m *MockStore) GetRandomHashedObjects(_ context.Context, _ int) ([]core.ObjectLocation, error) {
	return nil, nil
}

// GetObjectsWithoutHash returns nil (stub).
func (m *MockStore) GetObjectsWithoutHash(_ context.Context, _, _ int) ([]core.ObjectLocation, error) {
	return nil, nil
}

// UpdateContentHash returns nil (stub).
func (m *MockStore) UpdateContentHash(_ context.Context, _, _, _ string) error {
	return nil
}

// -------------------------------------------------------------------------
// PENDING OBJECTS - PutObject Intent Tracking
// -------------------------------------------------------------------------

// InsertPending returns nil (stub).
func (m *MockStore) InsertPending(_ context.Context, _ *core.PendingObject) error {
	return nil
}

// DeletePending returns nil (stub).
func (m *MockStore) DeletePending(_ context.Context, _ string) error {
	return nil
}

// GetStalePending returns an empty slice (stub).
func (m *MockStore) GetStalePending(_ context.Context, _ time.Time, _ int) ([]core.PendingObject, error) {
	return nil, nil
}

// PromotePending returns the AlreadyResolved sentinel (stub: nothing to do).
func (m *MockStore) PromotePending(_ context.Context, _ *core.PendingObject) (core.PendingPromoteResult, []core.DeletedCopy, error) {
	return core.PendingPromoteAlreadyResolved, nil, nil
}

// PendingDepth returns 0 (stub).
func (m *MockStore) PendingDepth(_ context.Context) (int64, error) {
	return 0, nil
}

// DeletePendingByBackend returns nil (stub).
func (m *MockStore) DeletePendingByBackend(_ context.Context, _ string) error {
	return nil
}
