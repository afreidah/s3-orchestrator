package ui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/storage"
)

// uiMockStore implements storage.MetadataStore for UI handler tests.
type uiMockStore struct {
	quotaStats      map[string]storage.QuotaStat
	objectCounts    map[string]int64
	multipartCounts map[string]int64
	usageStats      map[string]storage.UsageStat
	dirChildren     *storage.DirectoryListResult
}

func (m *uiMockStore) GetAllObjectLocations(_ context.Context, _ string) ([]storage.ObjectLocation, error) {
	return nil, storage.ErrObjectNotFound
}
func (m *uiMockStore) RecordObject(_ context.Context, _, _ string, _ int64) error { return nil }
func (m *uiMockStore) DeleteObject(_ context.Context, _ string) ([]storage.DeletedCopy, error) {
	return nil, nil
}
func (m *uiMockStore) ListObjects(_ context.Context, _, _ string, _ int) (*storage.ListObjectsResult, error) {
	return &storage.ListObjectsResult{}, nil
}
func (m *uiMockStore) GetBackendWithSpace(_ context.Context, _ int64, _ []string) (string, error) {
	return "b1", nil
}
func (m *uiMockStore) GetLeastUtilizedBackend(_ context.Context, _ int64, _ []string) (string, error) {
	return "b1", nil
}
func (m *uiMockStore) CreateMultipartUpload(_ context.Context, _, _, _, _ string) error { return nil }
func (m *uiMockStore) GetMultipartUpload(_ context.Context, _ string) (*storage.MultipartUpload, error) {
	return nil, nil
}
func (m *uiMockStore) RecordPart(_ context.Context, _ string, _ int, _ string, _ int64) error {
	return nil
}
func (m *uiMockStore) GetParts(_ context.Context, _ string) ([]storage.MultipartPart, error) {
	return nil, nil
}
func (m *uiMockStore) DeleteMultipartUpload(_ context.Context, _ string) error { return nil }
func (m *uiMockStore) ListDirectoryChildren(_ context.Context, _, _ string, _ int) (*storage.DirectoryListResult, error) {
	if m.dirChildren != nil {
		return m.dirChildren, nil
	}
	return &storage.DirectoryListResult{}, nil
}
func (m *uiMockStore) GetQuotaStats(_ context.Context) (map[string]storage.QuotaStat, error) {
	return m.quotaStats, nil
}
func (m *uiMockStore) GetObjectCounts(_ context.Context) (map[string]int64, error) {
	return m.objectCounts, nil
}
func (m *uiMockStore) GetActiveMultipartCounts(_ context.Context) (map[string]int64, error) {
	return m.multipartCounts, nil
}
func (m *uiMockStore) GetStaleMultipartUploads(_ context.Context, _ time.Duration) ([]storage.MultipartUpload, error) {
	return nil, nil
}
func (m *uiMockStore) ListObjectsByBackend(_ context.Context, _ string, _ int) ([]storage.ObjectLocation, error) {
	return nil, nil
}
func (m *uiMockStore) MoveObjectLocation(_ context.Context, _, _, _ string) (int64, error) {
	return 0, nil
}
func (m *uiMockStore) GetUnderReplicatedObjects(_ context.Context, _, _ int) ([]storage.ObjectLocation, error) {
	return nil, nil
}
func (m *uiMockStore) RecordReplica(_ context.Context, _, _, _ string, _ int64) (bool, error) {
	return false, nil
}
func (m *uiMockStore) FlushUsageDeltas(_ context.Context, _, _ string, _, _, _ int64) error {
	return nil
}
func (m *uiMockStore) GetUsageForPeriod(_ context.Context, _ string) (map[string]storage.UsageStat, error) {
	return m.usageStats, nil
}

// newTestHandler builds a Handler wired to mock data for testing.
func newTestHandler(t *testing.T) (*Handler, *http.ServeMux) {
	t.Helper()

	mockStore := &uiMockStore{
		quotaStats: map[string]storage.QuotaStat{
			"b1": {BackendName: "b1", BytesUsed: 500, BytesLimit: 1000},
		},
		objectCounts:    map[string]int64{"b1": 42},
		multipartCounts: map[string]int64{"b1": 0},
		usageStats:      map[string]storage.UsageStat{"b1": {ApiRequests: 100}},
		dirChildren: &storage.DirectoryListResult{
			Entries: []storage.DirEntry{
				{Name: "bucket1/", IsDir: true, FileCount: 10, TotalSize: 4096},
			},
		},
	}

	// mockBackend satisfies ObjectBackend — unused but needed for NewBackendManager.
	mgr := storage.NewBackendManager(&storage.BackendManagerConfig{
		Backends:        map[string]storage.ObjectBackend{},
		Store:           mockStore,
		Order:           []string{"b1"},
		RoutingStrategy: "pack",
	})
	t.Cleanup(mgr.Close)

	cfg := &config.Config{
		Buckets: []config.BucketConfig{
			{Name: "test-bucket"},
		},
		RoutingStrategy: "pack",
		Replication:     config.ReplicationConfig{Factor: 1},
		RateLimit:       config.RateLimitConfig{Enabled: false},
	}

	h := New(mgr, func() bool { return true }, cfg)

	mux := http.NewServeMux()
	h.Register(mux, "/ui")

	return h, mux
}

func TestDashboard_Returns200HTML(t *testing.T) {
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "S3 Orchestrator") {
		t.Error("response body missing expected title")
	}
	if !strings.Contains(string(body), "Backends") {
		t.Error("response body missing Backends section")
	}
}

func TestAPIDashboard_ReturnsJSON(t *testing.T) {
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/ui/api/dashboard", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var data storage.DashboardData
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
	if len(data.QuotaStats) == 0 {
		t.Error("expected non-empty QuotaStats")
	}
}

func TestTreeAPI_ReturnsJSON(t *testing.T) {
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/ui/api/tree?prefix=", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var result storage.DirectoryListResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
	if len(result.Entries) == 0 {
		t.Error("expected entries in tree response")
	}
}

func TestStaticCSS_Returns200(t *testing.T) {
	_, mux := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/ui/static/style.css", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "css") {
		t.Errorf("Content-Type = %q, want text/css", ct)
	}
}
