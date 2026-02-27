// -------------------------------------------------------------------------------
// UI Handler Tests
//
// Author: Alex Freidah
//
// Tests for the web dashboard HTTP handlers. Validates dashboard HTML rendering,
// JSON API endpoints for dashboard data and directory tree, and static asset
// serving.
// -------------------------------------------------------------------------------

package ui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/storage"
	"github.com/afreidah/s3-orchestrator/internal/testutil"
)

// newTestHandler builds a Handler wired to mock data for testing.
func newTestHandler(t *testing.T) (*Handler, *http.ServeMux) {
	t.Helper()

	mockStore := &testutil.MockStore{
		GetQuotaStatsResp: map[string]storage.QuotaStat{
			"b1": {BackendName: "b1", BytesUsed: 500, BytesLimit: 1000},
		},
		GetObjectCountsResp:    map[string]int64{"b1": 42},
		GetActiveMultipartResp: map[string]int64{"b1": 0},
		GetUsageForPeriodResp:  map[string]storage.UsageStat{"b1": {APIRequests: 100}},
		ListDirChildrenResp: &storage.DirectoryListResult{
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
	if !strings.Contains(string(body), "Storage Summary") {
		t.Error("response body missing Storage Summary section")
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

func TestSecurityHeaders_PresentOnAllEndpoints(t *testing.T) {
	_, mux := newTestHandler(t)

	endpoints := []string{"/ui/", "/ui/api/dashboard", "/ui/api/tree?prefix="}
	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, ep, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			resp := w.Result()
			checks := map[string]string{
				"X-Frame-Options":        "DENY",
				"X-Content-Type-Options":  "nosniff",
				"Referrer-Policy":         "strict-origin-when-cross-origin",
				"Content-Security-Policy": "default-src 'self'; style-src 'self' 'unsafe-inline'",
			}
			for header, want := range checks {
				got := resp.Header.Get(header)
				if got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
		})
	}
}

func TestUpdateConfig_ReflectsInDashboard(t *testing.T) {
	h, mux := newTestHandler(t)

	// Update config with a different routing strategy
	newCfg := &config.Config{
		Buckets: []config.BucketConfig{
			{Name: "updated-bucket"},
		},
		RoutingStrategy: "spread",
		Replication:     config.ReplicationConfig{Factor: 2},
		RateLimit:       config.RateLimitConfig{Enabled: true},
	}
	h.UpdateConfig(newCfg)

	req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body, _ := io.ReadAll(w.Result().Body)
	html := string(body)

	if !strings.Contains(html, "spread") {
		t.Error("dashboard should reflect updated routing strategy 'spread'")
	}
	if !strings.Contains(html, "updated-bucket") {
		t.Error("dashboard should reflect updated bucket name")
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
