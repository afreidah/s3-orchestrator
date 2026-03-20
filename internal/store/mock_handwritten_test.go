// -------------------------------------------------------------------------------
// Mock Store - Test Double for MetadataStore
//
// Author: Alex Freidah
//
// Configurable MetadataStore implementation for storage package unit tests.
// Provides pre-set responses and call counters for verifying store interactions
// without a database.
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"sync"
	"time"

)

// mockStore is a configurable MetadataStore mock for unit testing manager logic.
// Each method returns its pre-configured response/error. Call tracking fields
// allow assertions on what the manager called.
type mockStore struct {
	mu sync.Mutex

	// --- Configurable responses ---
	getAllLocationsResp []ObjectLocation
	getAllLocationsErr  error

	getBackendResp         string
	getBackendErr          error
	getBackendFromEligible bool                                                // when true, returns eligible[0] instead of getBackendResp
	getBackendFunc         func(size int64, eligible []string) (string, error) // overrides all other getBackend fields when set

	recordObjectResp []DeletedCopy
	recordObjectErr  error

	deleteObjectResp []DeletedCopy
	deleteObjectErr  error
	deleteObjectFunc func(key string) ([]DeletedCopy, error)

	listObjectsResp  *ListObjectsResult
	listObjectsPages []ListObjectsResult // for paginated tests
	listObjectsErr   error

	// Multipart
	createMultipartErr    error
	getMultipartResp      *MultipartUpload
	getMultipartErr       error
	getPartsResp          []MultipartPart
	getPartsErr           error
	deleteMultipartErr    error
	recordPartErr         error
	getStaleMultipartResp []MultipartUpload
	getStaleMultipartErr  error

	// Dashboard / background
	getQuotaStatsResp      map[string]QuotaStat
	getQuotaStatsErr       error
	getObjectCountsResp    map[string]int64
	getObjectCountsErr     error
	getActiveMultipartResp map[string]int64
	getActiveMultipartErr  error
	getUsageForPeriodResp  map[string]UsageStat
	getUsageForPeriodErr   error
	listDirChildrenResp    *DirectoryListResult
	listDirChildrenErr     error

	// Usage tracking
	flushUsageErr   error
	flushUsageCalls []flushUsageCall

	// Rebalance / Drain
	listObjectsByBackendResp  []ObjectLocation
	listObjectsByBackendPages [][]ObjectLocation // for paginated tests
	listObjectsByBackendErr   error
	listObjectsByBackendGate  chan struct{} // if set, blocks until closed
	deleteObjectLocationCalls []deleteObjectLocationCall
	deleteObjectLocationErr   error
	deleteBackendDataErr      error
	moveObjectLocationSize    int64
	moveObjectLocationErr     error

	// Cleanup queue
	enqueueCleanupErr    error
	pendingCleanups      []CleanupItem
	getPendingErr        error
	completeCleanupErr   error
	retryCleanupErr      error
	cleanupQueueDepthVal int64
	cleanupQueueDepthErr error

	// Orphan bytes tracking
	incrementOrphanBytesCalls []orphanBytesCall
	decrementOrphanBytesCalls []orphanBytesCall
	incrementOrphanBytesErr   error
	decrementOrphanBytesErr   error

	// Replication
	getUnderReplicatedResp          []ObjectLocation
	getUnderReplicatedErr           error
	getUnderReplicatedExcludingResp []ObjectLocation
	getUnderReplicatedExcludingErr  error
	recordReplicaInserted           bool
	recordReplicaErr                error
	recordReplicaCalls              []recordReplicaCall

	// Over-replication
	getOverReplicatedResp   []ObjectLocation
	getOverReplicatedErr    error
	countOverReplicatedResp int64
	countOverReplicatedErr  error
	removeExcessCopyErr     error
	removeExcessCopyCalls   []removeExcessCopyCall

	// Lifecycle
	listExpiredObjectsResp  []ObjectLocation
	listExpiredObjectsPages [][]ObjectLocation // for paginated lifecycle tests
	listExpiredObjectsErr   error

	// Import / Reconcile
	importObjectErr error

	// --- Call tracking ---
	recordObjectCalls        []recordObjectCall
	deleteObjectCalls        []string
	enqueueCleanupCalls      []enqueueCleanupCall
	completeCleanupCalls     []int64
	retryCleanupCalls        []retryCleanupCall
	callCount                int
	getBackendWithSpaceCalls int
	getLeastUtilizedCalls    int
}

type recordObjectCall struct {
	Key, Backend string
	Size         int64
}

type enqueueCleanupCall struct {
	backendName string
	objectKey   string
	reason      string
}

type retryCleanupCall struct {
	id        int64
	backoff   time.Duration
	lastError string
}

type deleteObjectLocationCall struct {
	key, backend string
}

type recordReplicaCall struct {
	key, targetBackend, sourceBackend string
	size                              int64
}

type removeExcessCopyCall struct {
	key, backend string
	size         int64
}

type orphanBytesCall struct {
	backendName string
	sizeBytes   int64
}

type flushUsageCall struct {
	backendName  string
	period       string
	apiRequests  int64
	egressBytes  int64
	ingressBytes int64
}

var _ MetadataStore = (*mockStore)(nil)

func (m *mockStore) GetAllObjectLocations(_ context.Context, key string) ([]ObjectLocation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callCount++
	if m.getAllLocationsErr != nil {
		return nil, m.getAllLocationsErr
	}
	if m.getAllLocationsResp != nil {
		return m.getAllLocationsResp, nil
	}
	return nil, ErrObjectNotFound
}

func (m *mockStore) GetBackendWithSpace(_ context.Context, size int64, eligible []string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getBackendWithSpaceCalls++
	if m.getBackendFunc != nil {
		return m.getBackendFunc(size, eligible)
	}
	if m.getBackendErr != nil {
		return "", m.getBackendErr
	}
	if m.getBackendFromEligible && len(eligible) > 0 {
		return eligible[0], nil
	}
	return m.getBackendResp, nil
}

func (m *mockStore) GetLeastUtilizedBackend(_ context.Context, size int64, eligible []string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getLeastUtilizedCalls++
	if m.getBackendFunc != nil {
		return m.getBackendFunc(size, eligible)
	}
	if m.getBackendErr != nil {
		return "", m.getBackendErr
	}
	if m.getBackendFromEligible && len(eligible) > 0 {
		return eligible[0], nil
	}
	return m.getBackendResp, nil
}

func (m *mockStore) RecordObject(_ context.Context, key, backend string, size int64, _ *EncryptionMeta) ([]DeletedCopy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordObjectCalls = append(m.recordObjectCalls, recordObjectCall{Key: key, Backend: backend, Size: size})
	return m.recordObjectResp, m.recordObjectErr
}

func (m *mockStore) DeleteObject(_ context.Context, key string) ([]DeletedCopy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteObjectCalls = append(m.deleteObjectCalls, key)
	if m.deleteObjectFunc != nil {
		return m.deleteObjectFunc(key)
	}
	if m.deleteObjectErr != nil {
		return nil, m.deleteObjectErr
	}
	return m.deleteObjectResp, nil
}

func (m *mockStore) ListObjects(_ context.Context, _, startAfter string, _ int) (*ListObjectsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listObjectsErr != nil {
		return nil, m.listObjectsErr
	}
	// Support paginated tests: if pages are configured, pop the first page
	// on each call (the startAfter cursor distinguishes successive calls).
	if len(m.listObjectsPages) > 0 {
		page := m.listObjectsPages[0]
		m.listObjectsPages = m.listObjectsPages[1:]
		return &page, nil
	}
	return m.listObjectsResp, nil
}

func (m *mockStore) CreateMultipartUpload(_ context.Context, _, _, _, _ string, _ map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createMultipartErr
}

func (m *mockStore) GetMultipartUpload(_ context.Context, _ string) (*MultipartUpload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getMultipartErr != nil {
		return nil, m.getMultipartErr
	}
	return m.getMultipartResp, nil
}

func (m *mockStore) RecordPart(_ context.Context, _ string, _ int, _ string, _ int64, _ *EncryptionMeta) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recordPartErr
}

func (m *mockStore) GetParts(_ context.Context, _ string) ([]MultipartPart, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getPartsErr != nil {
		return nil, m.getPartsErr
	}
	return m.getPartsResp, nil
}

func (m *mockStore) DeleteMultipartUpload(_ context.Context, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteMultipartErr
}

func (m *mockStore) ListDirectoryChildren(_ context.Context, _, _ string, _ int) (*DirectoryListResult, error) {
	if m.listDirChildrenErr != nil {
		return nil, m.listDirChildrenErr
	}
	if m.listDirChildrenResp != nil {
		return m.listDirChildrenResp, nil
	}
	return &DirectoryListResult{}, nil
}

// --- Background operations (stubs) ---

func (m *mockStore) GetQuotaStats(_ context.Context) (map[string]QuotaStat, error) {
	if m.getQuotaStatsErr != nil {
		return nil, m.getQuotaStatsErr
	}
	if m.getQuotaStatsResp != nil {
		return m.getQuotaStatsResp, nil
	}
	return map[string]QuotaStat{}, nil
}

func (m *mockStore) GetObjectCounts(_ context.Context) (map[string]int64, error) {
	if m.getObjectCountsErr != nil {
		return nil, m.getObjectCountsErr
	}
	if m.getObjectCountsResp != nil {
		return m.getObjectCountsResp, nil
	}
	return map[string]int64{}, nil
}

func (m *mockStore) GetActiveMultipartCounts(_ context.Context) (map[string]int64, error) {
	if m.getActiveMultipartErr != nil {
		return nil, m.getActiveMultipartErr
	}
	if m.getActiveMultipartResp != nil {
		return m.getActiveMultipartResp, nil
	}
	return map[string]int64{}, nil
}

func (m *mockStore) GetStaleMultipartUploads(_ context.Context, _ time.Duration) ([]MultipartUpload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getStaleMultipartErr != nil {
		return nil, m.getStaleMultipartErr
	}
	return m.getStaleMultipartResp, nil
}

// ListMultipartUploads returns nil (stub).
func (m *mockStore) ListMultipartUploads(_ context.Context, _ string, _ int) ([]MultipartUpload, error) {
	return nil, nil
}

func (m *mockStore) ListObjectsByBackend(ctx context.Context, _ string, _ int) ([]ObjectLocation, error) {
	m.mu.Lock()
	gate := m.listObjectsByBackendGate
	m.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listObjectsByBackendErr != nil {
		return nil, m.listObjectsByBackendErr
	}
	if len(m.listObjectsByBackendPages) > 0 {
		page := m.listObjectsByBackendPages[0]
		m.listObjectsByBackendPages = m.listObjectsByBackendPages[1:]
		return page, nil
	}
	return m.listObjectsByBackendResp, nil
}

func (m *mockStore) MoveObjectLocation(_ context.Context, _, _, _ string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.moveObjectLocationErr != nil {
		return 0, m.moveObjectLocationErr
	}
	return m.moveObjectLocationSize, nil
}

func (m *mockStore) GetUnderReplicatedObjects(_ context.Context, _, _ int) ([]ObjectLocation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getUnderReplicatedErr != nil {
		return nil, m.getUnderReplicatedErr
	}
	return m.getUnderReplicatedResp, nil
}

func (m *mockStore) GetUnderReplicatedObjectsExcluding(_ context.Context, _, _ int, _ []string) ([]ObjectLocation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getUnderReplicatedExcludingErr != nil {
		return nil, m.getUnderReplicatedExcludingErr
	}
	return m.getUnderReplicatedExcludingResp, nil
}

func (m *mockStore) RecordReplica(_ context.Context, key, targetBackend, sourceBackend string, size int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordReplicaCalls = append(m.recordReplicaCalls, recordReplicaCall{
		key:           key,
		targetBackend: targetBackend,
		sourceBackend: sourceBackend,
		size:          size,
	})
	if m.recordReplicaErr != nil {
		return false, m.recordReplicaErr
	}
	return m.recordReplicaInserted, nil
}

func (m *mockStore) FlushUsageDeltas(_ context.Context, backendName, period string, apiRequests, egressBytes, ingressBytes int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushUsageCalls = append(m.flushUsageCalls, flushUsageCall{
		backendName:  backendName,
		period:       period,
		apiRequests:  apiRequests,
		egressBytes:  egressBytes,
		ingressBytes: ingressBytes,
	})
	return m.flushUsageErr
}

func (m *mockStore) GetUsageForPeriod(_ context.Context, _ string) (map[string]UsageStat, error) {
	if m.getUsageForPeriodErr != nil {
		return nil, m.getUsageForPeriodErr
	}
	if m.getUsageForPeriodResp != nil {
		return m.getUsageForPeriodResp, nil
	}
	return map[string]UsageStat{}, nil
}

// --- Cleanup queue operations ---

func (m *mockStore) EnqueueCleanup(_ context.Context, backendName, objectKey, reason string, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enqueueCleanupCalls = append(m.enqueueCleanupCalls, enqueueCleanupCall{
		backendName: backendName,
		objectKey:   objectKey,
		reason:      reason,
	})
	return m.enqueueCleanupErr
}

func (m *mockStore) IncrementOrphanBytes(_ context.Context, backendName string, sizeBytes int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.incrementOrphanBytesCalls = append(m.incrementOrphanBytesCalls, orphanBytesCall{backendName: backendName, sizeBytes: sizeBytes})
	return m.incrementOrphanBytesErr
}

func (m *mockStore) DecrementOrphanBytes(_ context.Context, backendName string, sizeBytes int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decrementOrphanBytesCalls = append(m.decrementOrphanBytesCalls, orphanBytesCall{backendName: backendName, sizeBytes: sizeBytes})
	return m.decrementOrphanBytesErr
}

func (m *mockStore) GetPendingCleanups(_ context.Context, _ int) ([]CleanupItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getPendingErr != nil {
		return nil, m.getPendingErr
	}
	return m.pendingCleanups, nil
}

func (m *mockStore) CompleteCleanupItem(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completeCleanupCalls = append(m.completeCleanupCalls, id)
	return m.completeCleanupErr
}

func (m *mockStore) RetryCleanupItem(_ context.Context, id int64, backoff time.Duration, lastError string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retryCleanupCalls = append(m.retryCleanupCalls, retryCleanupCall{
		id:        id,
		backoff:   backoff,
		lastError: lastError,
	})
	return m.retryCleanupErr
}

func (m *mockStore) CleanupQueueDepth(_ context.Context) (int64, error) {
	return m.cleanupQueueDepthVal, m.cleanupQueueDepthErr
}

func (m *mockStore) ListExpiredObjects(_ context.Context, _ string, _ time.Time, _ int) ([]ObjectLocation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listExpiredObjectsErr != nil {
		return nil, m.listExpiredObjectsErr
	}
	if len(m.listExpiredObjectsPages) > 0 {
		page := m.listExpiredObjectsPages[0]
		m.listExpiredObjectsPages = m.listExpiredObjectsPages[1:]
		return page, nil
	}
	return m.listExpiredObjectsResp, nil
}

func (m *mockStore) WithAdvisoryLock(_ context.Context, _ int64, fn func(ctx context.Context) error) (bool, error) {
	return true, fn(context.Background())
}

func (m *mockStore) ImportObject(_ context.Context, _, _ string, _ int64) (bool, error) {
	if m.importObjectErr != nil {
		return false, m.importObjectErr
	}
	return true, nil
}

func (m *mockStore) BackendObjectStats(_ context.Context, _ string) (int64, int64, error) {
	return 0, 0, nil
}

func (m *mockStore) DeleteBackendData(_ context.Context, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteBackendDataErr
}

func (m *mockStore) DeleteObjectLocation(_ context.Context, key, backend string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteObjectLocationCalls = append(m.deleteObjectLocationCalls, deleteObjectLocationCall{key: key, backend: backend})
	return m.deleteObjectLocationErr
}

func (m *mockStore) GetOverReplicatedObjects(_ context.Context, _, _ int) ([]ObjectLocation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getOverReplicatedErr != nil {
		return nil, m.getOverReplicatedErr
	}
	return m.getOverReplicatedResp, nil
}

func (m *mockStore) CountOverReplicatedObjects(_ context.Context, _ int) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.countOverReplicatedErr != nil {
		return 0, m.countOverReplicatedErr
	}
	return m.countOverReplicatedResp, nil
}

func (m *mockStore) RemoveExcessCopy(_ context.Context, key, backend string, size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeExcessCopyCalls = append(m.removeExcessCopyCalls, removeExcessCopyCall{key: key, backend: backend, size: size})
	return m.removeExcessCopyErr
}
