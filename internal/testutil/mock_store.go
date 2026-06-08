// Package testutil provides shared mock implementations and test helpers used
// across multiple packages.
package testutil

import (
	"context"
	"sync"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// MockStore is a thin field-configurable shim over the canonical
// storetest.MockMetadataStore. The embedded mock supplies Permissive
// .AnyTimes() defaults for every interface method; MockStore overrides
// only the handful of methods whose return values or call counts the
// transport / DI handler tests need to drive imperatively. Keeping the
// public field surface lets the dozens of existing test fixtures remain
// untouched while consolidating the underlying mock implementation
// behind a single mockgen-generated source of truth.
type MockStore struct {
	*storetest.MockMetadataStore

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
	ListObjectsPages []core.ListObjectsResult
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
	CountActiveMultipartResp  int64
	CountActiveMultipartErr   error

	// Dashboard / background
	GetQuotaStatsResp      map[string]core.QuotaStat
	GetQuotaStatsErr       error
	GetObjectCountsResp           map[string]int64
	GetObjectCountsErr            error
	GetUnverifiedObjectCountsResp map[string]int64
	GetUnverifiedObjectCountsErr  error
	GetActiveMultipartResp map[string]int64
	GetActiveMultipartErr  error
	GetUsageForPeriodResp  map[string]core.UsageStat
	GetUsageForPeriodErr   error
	ListDirChildrenResp    *core.DirectoryListResult
	ListDirChildrenErr     error

	// Usage tracking
	FlushUsageErr   error
	FlushUsageCalls []FlushUsageCall

	// Cleanup queue
	PendingCleanupsResp   []core.CleanupItem
	PendingCleanupsErr    error
	CleanupQueueDepthResp int64
	CleanupQueueDepthErr  error

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

// NewMockStore builds a MockStore with Permissive .AnyTimes() defaults
// registered on the embedded storetest.MockMetadataStore. Tests that
// need imperative control over specific methods set the corresponding
// public fields after construction; the overridden methods below shadow
// the embedded permissive defaults.
func NewMockStore(t gomock.TestReporter) *MockStore {
	inner := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(inner)
	return &MockStore{MockMetadataStore: inner}
}

// GetAllObjectLocations returns the pre-configured locations or
// ErrObjectNotFound. The not-found default is load-bearing - several
// handler tests rely on it as the implicit "missing key" path.
func (m *MockStore) GetAllObjectLocations(_ context.Context, _ string) ([]core.ObjectLocation, error) {
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
// need a specific response set GetObjectBackendsForKeysResp.
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

// CountActiveMultipartUploads returns the pre-configured count or error.
func (m *MockStore) CountActiveMultipartUploads(_ context.Context, _ string) (int64, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if m.CountActiveMultipartErr != nil {
		return 0, m.CountActiveMultipartErr
	}
	return m.CountActiveMultipartResp, nil
}

// GetBackendWithSpace returns the pre-configured backend name or error.
func (m *MockStore) GetBackendWithSpace(_ context.Context, _ int64, _ []string) (string, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.GetBackendWithSpaceCalls++
	if m.GetBackendErr != nil {
		return "", m.GetBackendErr
	}
	return m.GetBackendResp, nil
}

// GetLeastUtilizedBackend returns the pre-configured backend name or error.
func (m *MockStore) GetLeastUtilizedBackend(_ context.Context, _ int64, _ []string) (string, error) {
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

// RecordObjectAndClearPending shares RecordObjectCalls with RecordObject.
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

// DeleteObjectsBatch records keys and returns the pre-configured response or error.
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
func (m *MockStore) ListObjects(_ context.Context, _, _ string, _ int) (*core.ListObjectsResult, error) {
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

// ReconcileUsage is a no-op stub: reconciliation is exercised against the real
// store via testcontainers; the mock reports no adjustments.
func (m *MockStore) ReconcileUsage(_ context.Context) (map[string]int64, error) {
	return map[string]int64{}, nil
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

// GetUnverifiedObjectCounts returns the pre-configured unverified counts or error.
func (m *MockStore) GetUnverifiedObjectCounts(_ context.Context) (map[string]int64, error) {
	if m.GetUnverifiedObjectCountsErr != nil {
		return nil, m.GetUnverifiedObjectCountsErr
	}
	if m.GetUnverifiedObjectCountsResp != nil {
		return m.GetUnverifiedObjectCountsResp, nil
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

// ListMultipartUploads returns the pre-configured uploads or error.
func (m *MockStore) ListMultipartUploads(_ context.Context, _ string, _ int) ([]core.MultipartUpload, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if m.ListMultipartUploadsErr != nil {
		return nil, m.ListMultipartUploadsErr
	}
	return m.ListMultipartUploadsResp, nil
}

// ListObjectsByBackendKeyAsc returns nil by default. Tests that exercise
// reconciliation paths set ListObjectsByBackendKeyAscFn.
func (m *MockStore) ListObjectsByBackendKeyAsc(_ context.Context, _ string, afterKey string, limit int) ([]core.ObjectLocation, error) {
	if m.ListObjectsByBackendKeyAscFn != nil {
		return m.ListObjectsByBackendKeyAscFn(afterKey, limit)
	}
	return nil, nil
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

// GetPendingCleanups returns the pre-configured slice + error.
func (m *MockStore) GetPendingCleanups(_ context.Context, _ int) ([]core.CleanupItem, error) {
	return m.PendingCleanupsResp, m.PendingCleanupsErr
}

// ClaimPendingCleanups stubs the worker-claim path by returning the same
// pre-configured slice as GetPendingCleanups.
func (m *MockStore) ClaimPendingCleanups(_ context.Context, _ int, _ string, _ time.Time) ([]core.CleanupItem, error) {
	return m.PendingCleanupsResp, m.PendingCleanupsErr
}

// CleanupQueueDepth returns the pre-configured value + error.
func (m *MockStore) CleanupQueueDepth(_ context.Context) (int64, error) {
	return m.CleanupQueueDepthResp, m.CleanupQueueDepthErr
}

// WithAdvisoryLock executes fn directly without acquiring a real lock.
func (m *MockStore) WithAdvisoryLock(_ context.Context, _ int64, fn func(ctx context.Context) error) (bool, error) {
	return true, fn(context.Background())
}
