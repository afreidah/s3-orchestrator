// Package testutil provides shared test helpers and mocks.
package testutil

import (
	"context"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/storage"
)

// MockStore is a configurable MetadataStore mock for unit testing.
// Each method returns its pre-configured response/error. Call tracking fields
// allow assertions on what the caller invoked.
type MockStore struct {
	Mu sync.Mutex

	// --- Configurable responses ---
	GetAllLocationsResp []storage.ObjectLocation
	GetAllLocationsErr  error

	GetBackendResp string
	GetBackendErr  error

	RecordObjectErr error

	DeleteObjectResp []storage.DeletedCopy
	DeleteObjectErr  error

	ListObjectsResp  *storage.ListObjectsResult
	ListObjectsPages []storage.ListObjectsResult // for paginated tests
	ListObjectsErr   error

	// Multipart
	CreateMultipartErr error
	GetMultipartResp   *storage.MultipartUpload
	GetMultipartErr    error
	GetPartsResp       []storage.MultipartPart
	GetPartsErr        error
	DeleteMultipartErr error
	RecordPartErr      error

	// Dashboard / background
	GetQuotaStatsResp      map[string]storage.QuotaStat
	GetQuotaStatsErr       error
	GetObjectCountsResp    map[string]int64
	GetObjectCountsErr     error
	GetActiveMultipartResp map[string]int64
	GetActiveMultipartErr  error
	GetUsageForPeriodResp  map[string]storage.UsageStat
	GetUsageForPeriodErr   error
	ListDirChildrenResp    *storage.DirectoryListResult
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

// Compile-time check.
var _ storage.MetadataStore = (*MockStore)(nil)

func (m *MockStore) GetAllObjectLocations(_ context.Context, key string) ([]storage.ObjectLocation, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.CallCount++
	if m.GetAllLocationsErr != nil {
		return nil, m.GetAllLocationsErr
	}
	if m.GetAllLocationsResp != nil {
		return m.GetAllLocationsResp, nil
	}
	return nil, storage.ErrObjectNotFound
}

func (m *MockStore) GetBackendWithSpace(_ context.Context, size int64, _ []string) (string, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.GetBackendWithSpaceCalls++
	if m.GetBackendErr != nil {
		return "", m.GetBackendErr
	}
	return m.GetBackendResp, nil
}

func (m *MockStore) GetLeastUtilizedBackend(_ context.Context, size int64, _ []string) (string, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.GetLeastUtilizedCalls++
	if m.GetBackendErr != nil {
		return "", m.GetBackendErr
	}
	return m.GetBackendResp, nil
}

func (m *MockStore) RecordObject(_ context.Context, key, backend string, size int64) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.RecordObjectCalls = append(m.RecordObjectCalls, RecordObjectCall{Key: key, Backend: backend, Size: size})
	return m.RecordObjectErr
}

func (m *MockStore) DeleteObject(_ context.Context, key string) ([]storage.DeletedCopy, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.DeleteObjectCalls = append(m.DeleteObjectCalls, key)
	if m.DeleteObjectErr != nil {
		return nil, m.DeleteObjectErr
	}
	return m.DeleteObjectResp, nil
}

func (m *MockStore) ListObjects(_ context.Context, _, startAfter string, _ int) (*storage.ListObjectsResult, error) {
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

func (m *MockStore) CreateMultipartUpload(_ context.Context, _, _, _, _ string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	return m.CreateMultipartErr
}

func (m *MockStore) GetMultipartUpload(_ context.Context, _ string) (*storage.MultipartUpload, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if m.GetMultipartErr != nil {
		return nil, m.GetMultipartErr
	}
	return m.GetMultipartResp, nil
}

func (m *MockStore) RecordPart(_ context.Context, _ string, _ int, _ string, _ int64) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	return m.RecordPartErr
}

func (m *MockStore) GetParts(_ context.Context, _ string) ([]storage.MultipartPart, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	if m.GetPartsErr != nil {
		return nil, m.GetPartsErr
	}
	return m.GetPartsResp, nil
}

func (m *MockStore) DeleteMultipartUpload(_ context.Context, _ string) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	return m.DeleteMultipartErr
}

func (m *MockStore) ListDirectoryChildren(_ context.Context, _, _ string, _ int) (*storage.DirectoryListResult, error) {
	if m.ListDirChildrenErr != nil {
		return nil, m.ListDirChildrenErr
	}
	if m.ListDirChildrenResp != nil {
		return m.ListDirChildrenResp, nil
	}
	return &storage.DirectoryListResult{}, nil
}

func (m *MockStore) GetQuotaStats(_ context.Context) (map[string]storage.QuotaStat, error) {
	if m.GetQuotaStatsErr != nil {
		return nil, m.GetQuotaStatsErr
	}
	if m.GetQuotaStatsResp != nil {
		return m.GetQuotaStatsResp, nil
	}
	return map[string]storage.QuotaStat{}, nil
}

func (m *MockStore) GetObjectCounts(_ context.Context) (map[string]int64, error) {
	if m.GetObjectCountsErr != nil {
		return nil, m.GetObjectCountsErr
	}
	if m.GetObjectCountsResp != nil {
		return m.GetObjectCountsResp, nil
	}
	return map[string]int64{}, nil
}

func (m *MockStore) GetActiveMultipartCounts(_ context.Context) (map[string]int64, error) {
	if m.GetActiveMultipartErr != nil {
		return nil, m.GetActiveMultipartErr
	}
	if m.GetActiveMultipartResp != nil {
		return m.GetActiveMultipartResp, nil
	}
	return map[string]int64{}, nil
}

func (m *MockStore) GetStaleMultipartUploads(_ context.Context, _ time.Duration) ([]storage.MultipartUpload, error) {
	return nil, nil
}

func (m *MockStore) ListObjectsByBackend(_ context.Context, _ string, _ int) ([]storage.ObjectLocation, error) {
	return nil, nil
}

func (m *MockStore) MoveObjectLocation(_ context.Context, _, _, _ string) (int64, error) {
	return 0, nil
}

func (m *MockStore) GetUnderReplicatedObjects(_ context.Context, _, _ int) ([]storage.ObjectLocation, error) {
	return nil, nil
}

func (m *MockStore) RecordReplica(_ context.Context, _, _, _ string, _ int64) (bool, error) {
	return false, nil
}

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

func (m *MockStore) GetUsageForPeriod(_ context.Context, _ string) (map[string]storage.UsageStat, error) {
	if m.GetUsageForPeriodErr != nil {
		return nil, m.GetUsageForPeriodErr
	}
	if m.GetUsageForPeriodResp != nil {
		return m.GetUsageForPeriodResp, nil
	}
	return map[string]storage.UsageStat{}, nil
}
