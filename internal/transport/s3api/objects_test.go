// -------------------------------------------------------------------------------
// Object Handler Tests
//
// Author: Alex Freidah
//
// Tests for S3 object operation handlers: PUT, GET, HEAD, DELETE, and COPY.
// Validates request parsing, error responses, and storage layer interaction.
// -------------------------------------------------------------------------------

package s3api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"

	"go.uber.org/mock/gomock"

	// serverMockBackend implements storage.ObjectBackend for server handler tests.
	"github.com/afreidah/s3-orchestrator/internal/proxy"
)

// serverMockBackend is the in-memory ObjectBackend used by S3-handler
// tests in this file. Holds objects in a map and lets tests inject
// per-method errors (putErr, getErr, headErr, delErr) so each handler
// newTestServer creates an httptest.Server wired with mock backends and store.
// Returns the server, a cleanup func, and the mock store/backend for assertions.
func newTestServer(t *testing.T, opts ...func(*storetest.MockMetadataStore)) (*httptest.Server, *storetest.MockMetadataStore, *backendtest.InMemory) {
	t.Helper()

	backend := backendtest.NewInMemory()
	mockStore := storetest.NewMockMetadataStore(gomock.NewController(t))
	for _, opt := range opts {
		opt(mockStore)
	}
	mockStore.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("b1", nil).AnyTimes()
	storetest.Permissive(mockStore)

	mgr := proxytest.NewManager(t, mockStore, &proxy.BackendManagerConfig{
		Storage: proxy.StorageDeps{
			Backends: map[string]s3be.ObjectBackend{"b1": backend},
			Order:    []string{"b1"},
		},
		Policies: proxy.PolicyConfig{
			RoutingStrategy: config.RoutingPack,
		},
		Operations: proxy.OperationalDeps{
			Metrics: mockStore,
		},
	})
	_ = proxytest.BuildWorkers(mgr, mockStore)
	t.Cleanup(mgr.Close)

	srv := &Server{
		Objects:       mgr.Objects(),
		Multipart:     mgr.Multipart(),
		MaxObjectSize: 10 * 1024 * 1024, // 10MB
	}

	buckets := []config.BucketConfig{
		{Name: "mybucket", Credentials: []config.CredentialConfig{
			{Token: "test-token"},
		}},
	}
	srv.SetBucketAuth(mustBucketRegistry(t, buckets))

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	return ts, mockStore, backend
}

// doReq sends a request to ts with auth using ts.Client(), not
// http.DefaultClient. Each httptest.Server has its own transport, so
// a sibling test's ts.Close() (which calls
// http.DefaultTransport.CloseIdleConnections internally) cannot reap
// connections the current test is mid-flight on. Regression pin for
// the parallel-test flake "transport connection broken:
// CloseIdleConnections called".
func doReq(t *testing.T, ts *httptest.Server, method, url string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Proxy-Token", "test-token")
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// -------------------------------------------------------------------------
// PUT
// -------------------------------------------------------------------------

// TestPut_Success verifies the put success contract.
// Asserts that status = , want 200.
func TestPut_Success(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t)
	data := []byte("hello world")

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey", bytes.NewReader(data))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("Content-Type", "text/plain")
	req.ContentLength = int64(len(data))
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("expected ETag header")
	}
	if _, ok := backend.Objects["mybucket/testkey"]; !ok {
		t.Error("object not stored on backend")
	}
}

// TestPut_MissingContentLength verifies the put missing content length contract.
// Asserts that status = , want 411.
func TestPut_MissingContentLength(t *testing.T) {
	t.Parallel()
	// No ops expectation is registered, so any call past the request-validation
	// check fails the test.
	ts, _, _ := newOpsServer(t)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey", strings.NewReader("data"))
	req.Header.Set("X-Proxy-Token", "test-token")
	// Explicitly set ContentLength to -1 to simulate missing Content-Length
	req.ContentLength = -1
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusLengthRequired {
		t.Fatalf("status = %d, want 411", resp.StatusCode)
	}
}

// TestPut_EntityTooLarge verifies the put entity too large contract.
// Asserts that status = , want 413.
func TestPut_EntityTooLarge(t *testing.T) {
	t.Parallel()
	// No ops expectation is registered, so any call past the request-validation
	// check fails the test.
	ts, _, _ := newOpsServer(t)

	// Create a body whose size exceeds the limit.
	// We use a LimitReader wrapping zeros so we don't allocate 20MB.
	bigSize := int64(20 * 1024 * 1024)
	body := io.LimitReader(neverEndingReader{}, bigSize)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey", body)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.ContentLength = bigSize
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

// neverEndingReader produces zero bytes indefinitely.
type neverEndingReader struct{}

// Read reads .
func (neverEndingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// TestPut_IfNoneMatchStarRejectsExistingKey verifies that PutObject with
// `If-None-Match: *` returns 412 PreconditionFailed when the key already
// has an object_locations row, before any backend bytes are written.
func TestPut_IfNoneMatchStarRejectsExistingKey(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/testkey", BackendName: "test-backend", SizeBytes: 100},
			}, nil).AnyTimes()
	})

	data := []byte("hello")
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey", bytes.NewReader(data))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-None-Match", "*")
	req.ContentLength = int64(len(data))
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", resp.StatusCode)
	}
	if _, ok := backend.Objects["mybucket/testkey"]; ok {
		t.Error("backend should not have stored bytes when precondition fails")
	}
}

// TestPut_IfNoneMatchStarAllowsNewKey verifies that PutObject with
// `If-None-Match: *` succeeds when no location row exists for the key.
func TestPut_IfNoneMatchStarAllowsNewKey(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return(nil, core.ErrObjectNotFound).AnyTimes()
	})

	data := []byte("hello")
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/newkey", bytes.NewReader(data))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-None-Match", "*")
	req.ContentLength = int64(len(data))
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if _, ok := backend.Objects["mybucket/newkey"]; !ok {
		t.Error("object not stored on backend")
	}
}

// TestPut_IfNoneMatchSpecificETagIgnored verifies that an `If-None-Match`
// header carrying a specific etag (not `*`) is ignored on PUT. AWS S3
// only honors the `*` form for write preconditions; specific-etag forms
// are accepted and the upload proceeds normally.
func TestPut_IfNoneMatchSpecificETagIgnored(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/testkey", BackendName: "test-backend", SizeBytes: 100},
			}, nil).AnyTimes()
	})

	data := []byte("hello")
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey", bytes.NewReader(data))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-None-Match", `"some-etag"`)
	req.ContentLength = int64(len(data))
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (specific etag form ignored on PUT)", resp.StatusCode)
	}
	if _, ok := backend.Objects["mybucket/testkey"]; !ok {
		t.Error("object not stored on backend")
	}
}

// TestPut_QuotaExhausted verifies the put quota exhausted contract.
// Asserts that status = , want 507.
func TestPut_QuotaExhausted(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
			Return("", core.ErrNoSpaceAvailable).AnyTimes()
	})

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey", strings.NewReader("data"))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.ContentLength = 4
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507", resp.StatusCode)
	}
}

// TestPut_NoBackendCapacity_BodyIncludesCapacityHint verifies the 507
// InsufficientStorage response body includes the per-backend
// used/limit summary when CanAcceptWrite returns false and
// GetQuotaStats returns data. Operators see which backends are at
// capacity without checking other surfaces.
//
// Forces the "no eligible backend" path by configuring per-backend
// MaxObjectSizes=1 so any non-trivial upload exceeds the cap and
// eligibleForWrite returns no backends.
func TestPut_NoBackendCapacity_BodyIncludesCapacityHint(t *testing.T) {
	t.Parallel()
	mockStore := storetest.NewMockMetadataStore(gomock.NewController(t))
	mockStore.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("b1", nil).AnyTimes()
	mockStore.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{
			"alpha": {BackendName: "alpha", BytesUsed: 1024, BytesLimit: 4096},
			"beta":  {BackendName: "beta", BytesUsed: 2048, BytesLimit: 4096},
		}, nil).AnyTimes()
	storetest.Permissive(mockStore)
	ts := newCapacityHintTestServer(t, mockStore)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey", strings.NewReader("data"))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.ContentLength = 4
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	bodyStr := string(body)
	wants := []string{
		"No backend can accept a 4 byte upload",
		"backend usage:",
		"alpha=1.0 KiB/4.0 KiB",
		"beta=2.0 KiB/4.0 KiB",
	}
	for _, want := range wants {
		if !strings.Contains(bodyStr, want) {
			t.Errorf("body missing %q\nbody: %s", want, bodyStr)
		}
	}
}

// TestPut_NoBackendCapacity_QuotaStatsErrFallsBack verifies that a
// GetQuotaStats DB failure does not corrupt the 507 response: the
// body keeps the terse default message without the optional
// capacity-hint suffix.
func TestPut_NoBackendCapacity_QuotaStatsErrFallsBack(t *testing.T) {
	t.Parallel()
	mockStore := storetest.NewMockMetadataStore(gomock.NewController(t))
	mockStore.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("b1", nil).AnyTimes()
	storetest.Permissive(mockStore)
	mockStore.EXPECT().GetQuotaStats(gomock.Any()).
		Return(nil, core.ErrDBUnavailable).AnyTimes()
	ts := newCapacityHintTestServer(t, mockStore)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey", strings.NewReader("data"))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.ContentLength = 4
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "backend usage:") {
		t.Errorf("body should not include capacity hint when stats lookup failed: %s", body)
	}
	if !strings.Contains(string(body), "No backend can accept a 4 byte upload") {
		t.Errorf("body should include the terse default 507 message: %s", body)
	}
}

// newCapacityHintTestServer builds a Server whose single backend has
// MaxObjectSizes=1, so any upload of more than one byte fails the
// eligibleForWrite check and exercises the capacity-hint code path
// in handlePut.
func newCapacityHintTestServer(t *testing.T, mockStore *storetest.MockMetadataStore) *httptest.Server {
	t.Helper()
	backend := backendtest.NewInMemory()
	mgr := proxytest.NewManager(t, mockStore, &proxy.BackendManagerConfig{
		Storage: proxy.StorageDeps{
			Backends: map[string]s3be.ObjectBackend{"b1": backend},
			Order:    []string{"b1"},
		},
		Policies: proxy.PolicyConfig{
			MaxObjectSizes:  map[string]int64{"b1": 1},
			RoutingStrategy: config.RoutingPack,
		},
		Operations: proxy.OperationalDeps{
			Metrics: mockStore,
		},
	})
	_ = proxytest.BuildWorkers(mgr, mockStore)
	t.Cleanup(mgr.Close)

	srv := &Server{
		Objects:       mgr.Objects(),
		Multipart:     mgr.Multipart(),
		MaxObjectSize: 10 * 1024 * 1024,
	}
	srv.SetBucketAuth(mustBucketRegistry(t, []config.BucketConfig{
		{Name: "mybucket", Credentials: []config.CredentialConfig{{Token: "test-token"}}},
	}))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// TestPut_DBUnavailable verifies the put dbunavailable contract.
// Asserts that status = , want 503.
func TestPut_DBUnavailable(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
			Return("", core.ErrDBUnavailable).AnyTimes()
	})

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey", strings.NewReader("data"))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.ContentLength = 4
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// -------------------------------------------------------------------------
// GET
// -------------------------------------------------------------------------

// TestGet_Success verifies the get success contract.
// Asserts that status = , want 200.
func TestGet_Success(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
			}, nil).AnyTimes()
	})

	// Pre-store an object
	backend.Objects["mybucket/testkey"] = backendtest.Object{
		Data: []byte("hello"), ContentType: "text/plain", ETag: `"abc"`,
	}
	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello" {
		t.Errorf("body = %q, want %q", body, "hello")
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("expected ETag header")
	}
}

// TestGet_NotFound verifies the get not found contract.
// Asserts that status = , want 404.
func TestGet_NotFound(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return(nil, core.ErrObjectNotFound).AnyTimes()
	})

	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/nonexistent", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// -------------------------------------------------------------------------
// HEAD
// -------------------------------------------------------------------------

// TestHead_Success verifies the head success contract.
// Asserts that status = , want 200.
func TestHead_Success(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
			}, nil).AnyTimes()
	})

	backend.Objects["mybucket/testkey"] = backendtest.Object{
		Data: []byte("12345"), ContentType: "text/plain", ETag: `"abc"`,
	}
	resp := doReq(t, ts, http.MethodHead, ts.URL+"/mybucket/testkey", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Content-Length") != "5" {
		t.Errorf("Content-Length = %q, want 5", resp.Header.Get("Content-Length"))
	}
	if resp.Header.Get("Content-Type") != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("expected ETag header")
	}
}

// TestHead_NotFound verifies the head not found contract.
// Asserts that status = , want 404.
func TestHead_NotFound(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return(nil, core.ErrObjectNotFound).AnyTimes()
	})

	resp := doReq(t, ts, http.MethodHead, ts.URL+"/mybucket/nonexistent", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// -------------------------------------------------------------------------
// CONDITIONAL REQUESTS
// -------------------------------------------------------------------------

// TestGet_LastModifiedHeader verifies the get last modified header contract.
// Asserts that status = , want 200.
func TestGet_LastModifiedHeader(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5,
					CreatedAt: time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)},
			}, nil).AnyTimes()
	})

	backend.Objects["mybucket/testkey"] = backendtest.Object{
		Data: []byte("hello"), ContentType: "text/plain", ETag: `"abc"`,
	}
	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// LastModified comes from the backend, not the store  -  the mock backend
	// doesn't set it, so the header should be absent for this test.
	// This test validates that the header is at least not causing errors.
	if resp.Header.Get("ETag") != `"abc"` {
		t.Errorf("ETag = %q, want %q", resp.Header.Get("ETag"), `"abc"`)
	}
}

// TestGet_ConditionalIfNoneMatch verifies the get conditional if none match contract.
// Asserts that status = , want 304.
func TestGet_ConditionalIfNoneMatch(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
			}, nil).AnyTimes()
	})

	backend.Objects["mybucket/testkey"] = backendtest.Object{
		Data: []byte("hello"), ContentType: "text/plain", ETag: `"abc"`,
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-None-Match", `"abc"`)
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.StatusCode)
	}
}

// TestGet_ConditionalIfNoneMatchMismatch verifies the get conditional if none match mismatch contract.
// Asserts that status = , want 200.
func TestGet_ConditionalIfNoneMatchMismatch(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
			}, nil).AnyTimes()
	})

	backend.Objects["mybucket/testkey"] = backendtest.Object{
		Data: []byte("hello"), ContentType: "text/plain", ETag: `"abc"`,
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-None-Match", `"different"`)
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestGet_ConditionalIfMatch verifies the get conditional if match contract.
// Asserts that status = , want 412.
func TestGet_ConditionalIfMatch(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
			}, nil).AnyTimes()
	})

	backend.Objects["mybucket/testkey"] = backendtest.Object{
		Data: []byte("hello"), ContentType: "text/plain", ETag: `"abc"`,
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-Match", `"wrong"`)
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", resp.StatusCode)
	}
}

// TestHead_ConditionalIfNoneMatch verifies the head conditional if none match contract.
// Asserts that status = , want 304.
func TestHead_ConditionalIfNoneMatch(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
			}, nil).AnyTimes()
	})

	backend.Objects["mybucket/testkey"] = backendtest.Object{
		Data: []byte("hello"), ContentType: "text/plain", ETag: `"abc"`,
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodHead, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-None-Match", `"abc"`)
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.StatusCode)
	}
}

// TestGet_ConditionalIfModifiedSince verifies the get conditional if modified since contract.
// Asserts that status = , want 304.
func TestGet_ConditionalIfModifiedSince(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
			}, nil).AnyTimes()
	})

	objTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	backend.Objects["mybucket/testkey"] = backendtest.Object{
		Data: []byte("hello"), ContentType: "text/plain", ETag: `"abc"`,
		LastModified: objTime,
	}
	// Request with a time after the object's last modification -> 304
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-Modified-Since", objTime.Add(time.Hour).UTC().Format(http.TimeFormat))
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.StatusCode)
	}
}

// TestGet_ConditionalIfModifiedSinceNewer verifies the get conditional if modified since newer contract.
// Asserts that status = , want 200.
func TestGet_ConditionalIfModifiedSinceNewer(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
			}, nil).AnyTimes()
	})

	objTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	backend.Objects["mybucket/testkey"] = backendtest.Object{
		Data: []byte("hello"), ContentType: "text/plain", ETag: `"abc"`,
		LastModified: objTime,
	}
	// Request with a time before the object's last modification -> 200
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-Modified-Since", objTime.Add(-time.Hour).UTC().Format(http.TimeFormat))
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestGet_ConditionalIfUnmodifiedSince verifies the get conditional if unmodified since contract.
// Asserts that status = , want 412.
func TestGet_ConditionalIfUnmodifiedSince(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
			}, nil).AnyTimes()
	})

	objTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	backend.Objects["mybucket/testkey"] = backendtest.Object{
		Data: []byte("hello"), ContentType: "text/plain", ETag: `"abc"`,
		LastModified: objTime,
	}
	// Object was modified after the given time -> 412
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-Unmodified-Since", objTime.Add(-time.Hour).UTC().Format(http.TimeFormat))
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", resp.StatusCode)
	}
}

// TestGet_LastModifiedHeaderSet verifies the get last modified header set contract.
// Asserts that status = , want 200.
func TestGet_LastModifiedHeaderSet(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
			}, nil).AnyTimes()
	})

	objTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	backend.Objects["mybucket/testkey"] = backendtest.Object{
		Data: []byte("hello"), ContentType: "text/plain", ETag: `"abc"`,
		LastModified: objTime,
	}
	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	lm := resp.Header.Get("Last-Modified")
	if lm == "" {
		t.Fatal("expected Last-Modified header to be set")
	}
	expected := objTime.UTC().Format(http.TimeFormat)
	if lm != expected {
		t.Errorf("Last-Modified = %q, want %q", lm, expected)
	}
}

// TestHead_LastModifiedHeaderSet verifies the head last modified header set contract.
// Asserts that status = , want 200.
func TestHead_LastModifiedHeaderSet(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
			}, nil).AnyTimes()
	})

	objTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	backend.Objects["mybucket/testkey"] = backendtest.Object{
		Data: []byte("hello"), ContentType: "text/plain", ETag: `"abc"`,
		LastModified: objTime,
	}
	resp := doReq(t, ts, http.MethodHead, ts.URL+"/mybucket/testkey", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	lm := resp.Header.Get("Last-Modified")
	if lm == "" {
		t.Fatal("expected Last-Modified header to be set")
	}
	expected := objTime.UTC().Format(http.TimeFormat)
	if lm != expected {
		t.Errorf("Last-Modified = %q, want %q", lm, expected)
	}
}

// -------------------------------------------------------------------------
// METADATA ROUND-TRIP
// -------------------------------------------------------------------------

// TestPut_MetadataStored verifies the put metadata stored contract.
// Asserts that status = , want 200.
func TestPut_MetadataStored(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t)
	data := []byte("hello")

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/metakey", bytes.NewReader(data))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Amz-Meta-Project", "acme")
	req.Header.Set("X-Amz-Meta-Env", "prod")
	req.ContentLength = int64(len(data))
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	obj, ok := backend.Objects["mybucket/metakey"]
	if !ok {
		t.Fatal("object not stored")
	}
	if obj.Metadata["project"] != "acme" || obj.Metadata["env"] != "prod" {
		t.Errorf("metadata = %v, want project=acme env=prod", obj.Metadata)
	}
}

// TestGet_MetadataReturned verifies the get metadata returned contract.
// Asserts that status = , want 200.
func TestGet_MetadataReturned(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/metakey", BackendName: "b1", SizeBytes: 5},
			}, nil).AnyTimes()
	})

	backend.Objects["mybucket/metakey"] = backendtest.Object{
		Data: []byte("hello"), ContentType: "text/plain", ETag: `"abc"`,
		Metadata: map[string]string{"project": "acme", "env": "prod"},
	}
	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/metakey", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-Amz-Meta-Project") != "acme" {
		t.Errorf("x-amz-meta-project = %q, want acme", resp.Header.Get("X-Amz-Meta-Project"))
	}
	if resp.Header.Get("X-Amz-Meta-Env") != "prod" {
		t.Errorf("x-amz-meta-env = %q, want prod", resp.Header.Get("X-Amz-Meta-Env"))
	}
}

// TestHead_MetadataReturned verifies the head metadata returned contract.
// Asserts that status = , want 200.
func TestHead_MetadataReturned(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/metakey", BackendName: "b1", SizeBytes: 5},
			}, nil).AnyTimes()
	})

	backend.Objects["mybucket/metakey"] = backendtest.Object{
		Data: []byte("hello"), ContentType: "text/plain", ETag: `"abc"`,
		Metadata: map[string]string{"project": "acme"},
	}
	resp := doReq(t, ts, http.MethodHead, ts.URL+"/mybucket/metakey", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-Amz-Meta-Project") != "acme" {
		t.Errorf("x-amz-meta-project = %q, want acme", resp.Header.Get("X-Amz-Meta-Project"))
	}
}

// TestPut_MetadataTooLarge verifies the put metadata too large contract.
// Asserts that status = , want 400.
func TestPut_MetadataTooLarge(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)
	data := []byte("hello")

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/metakey", bytes.NewReader(data))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Amz-Meta-Big", strings.Repeat("x", maxUserMetadataBytes+1))
	req.ContentLength = int64(len(data))
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// -------------------------------------------------------------------------
// DELETE
// -------------------------------------------------------------------------

// TestDelete_Success verifies the delete success contract.
// Asserts that status = , want 204.
func TestDelete_Success(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).
			Return([]core.DeletedCopy{
				{BackendName: "b1", SizeBytes: 2},
			}, nil).AnyTimes()
	})

	backend.Objects["mybucket/testkey"] = backendtest.Object{Data: []byte("hi")}
	resp := doReq(t, ts, http.MethodDelete, ts.URL+"/mybucket/testkey", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

// TestDelete_IdempotentForMissing verifies the delete idempotent for missing contract.
// Asserts that status = , want 204.
func TestDelete_IdempotentForMissing(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).
			Return(nil, core.ErrObjectNotFound).AnyTimes()
	})

	resp := doReq(t, ts, http.MethodDelete, ts.URL+"/mybucket/nonexistent", nil)
	defer resp.Body.Close()

	// Manager treats missing objects as success (idempotent delete)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

// -------------------------------------------------------------------------
// DELETE OBJECTS (BATCH)
// -------------------------------------------------------------------------

// TestDeleteObjects_Success verifies the delete objects success contract.
// Asserts that status = , want 200.
func TestDeleteObjects_Success(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).
			Return([]core.DeletedCopy{{BackendName: "b1", SizeBytes: 1}}, nil).AnyTimes()
		m.EXPECT().DeleteObjectsBatch(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, keys []string) (map[string][]core.DeletedCopy, error) {
				out := make(map[string][]core.DeletedCopy, len(keys))
				for _, k := range keys {
					out[k] = []core.DeletedCopy{{BackendName: "b1", SizeBytes: 1}}
				}
				return out, nil
			}).AnyTimes()
	})

	backend.Objects["mybucket/key1"] = backendtest.Object{Data: []byte("a")}
	backend.Objects["mybucket/key2"] = backendtest.Object{Data: []byte("b")}
	body := strings.NewReader(`<Delete><Object><Key>key1</Key></Object><Object><Key>key2</Key></Object></Delete>`)
	resp := doReq(t, ts, http.MethodPost, ts.URL+"/mybucket?delete", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	respBody, _ := io.ReadAll(resp.Body)
	s := string(respBody)
	if !strings.Contains(s, "<Deleted>") {
		t.Error("response missing <Deleted> elements")
	}
	if strings.Count(s, "<Key>key1</Key>") != 1 || strings.Count(s, "<Key>key2</Key>") != 1 {
		t.Errorf("unexpected response body: %s", s)
	}
}

// TestDeleteObjects_QuietMode verifies the delete objects quiet mode contract.
// Asserts that status = , want 200.
func TestDeleteObjects_QuietMode(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).
			Return([]core.DeletedCopy{{BackendName: "b1", SizeBytes: 1}}, nil).AnyTimes()
		m.EXPECT().DeleteObjectsBatch(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, keys []string) (map[string][]core.DeletedCopy, error) {
				out := make(map[string][]core.DeletedCopy, len(keys))
				for _, k := range keys {
					out[k] = []core.DeletedCopy{{BackendName: "b1", SizeBytes: 1}}
				}
				return out, nil
			}).AnyTimes()
	})

	body := strings.NewReader(`<Delete><Quiet>true</Quiet><Object><Key>key1</Key></Object></Delete>`)
	resp := doReq(t, ts, http.MethodPost, ts.URL+"/mybucket?delete", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	respBody, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(respBody), "<Deleted>") {
		t.Error("quiet mode should suppress <Deleted> elements")
	}
}

// TestDeleteObjects_MalformedXML verifies the delete objects malformed xml contract.
// Asserts that status = , want 400.
func TestDeleteObjects_MalformedXML(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	body := strings.NewReader(`not xml at all`)
	resp := doReq(t, ts, http.MethodPost, ts.URL+"/mybucket?delete", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestDeleteObjects_TooManyObjects verifies the delete objects too many objects contract.
// Asserts that status = , want 400.
func TestDeleteObjects_TooManyObjects(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	var sb strings.Builder
	sb.WriteString("<Delete>")
	for range 1001 {
		sb.WriteString("<Object><Key>k</Key></Object>")
	}
	sb.WriteString("</Delete>")

	resp := doReq(t, ts, http.MethodPost, ts.URL+"/mybucket?delete", strings.NewReader(sb.String()))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestDeleteObjects_EmptyRequest verifies the delete objects empty request contract.
// Asserts that status = , want 400.
func TestDeleteObjects_EmptyRequest(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	body := strings.NewReader(`<Delete></Delete>`)
	resp := doReq(t, ts, http.MethodPost, ts.URL+"/mybucket?delete", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestDeleteObjects_WholeBatchFailure verifies that a transaction-level
// failure during the single-tx batch surfaces an <Error> element for
// every key in the request. Single-tx semantics: the batch is
// all-or-nothing, so an error fans out to every result rather than
// applying to one key.
func TestDeleteObjects_WholeBatchFailure(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().DeleteObjectsBatch(gomock.Any(), gomock.Any()).
			Return(nil, &core.S3Error{StatusCode: 500, Code: "InternalError", Message: "db error"}).AnyTimes()
	})

	body := strings.NewReader(`<Delete><Object><Key>good</Key></Object><Object><Key>bad</Key></Object></Delete>`)
	resp := doReq(t, ts, http.MethodPost, ts.URL+"/mybucket?delete", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (batch failures still return 200)", resp.StatusCode)
	}
	respBody, _ := io.ReadAll(resp.Body)
	s := string(respBody)
	if strings.Contains(s, "<Deleted>") {
		t.Error("response should not contain <Deleted> when whole batch failed")
	}
	// Both keys must surface as <Error>.
	if errCount := strings.Count(s, "<Error>"); errCount != 2 {
		t.Errorf("response should contain 2 <Error> elements, got %d: %s", errCount, s)
	}
}

// TestDeleteObjects_TypedErrorSurfaces verifies a typed *store.S3Error
// returned by the batch propagates its canonical Code and Message into
// every per-key <Error> element instead of the legacy hardcoded
// InternalError. Untyped errors still fall back to InternalError so
// clients see a valid S3 error envelope.
func TestDeleteObjects_TypedErrorSurfaces(t *testing.T) {
	t.Parallel()

	// Typed S3Error case.
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().DeleteObjectsBatch(gomock.Any(), gomock.Any()).
			Return(nil, &core.S3Error{StatusCode: 503, Code: "ServiceUnavailable", Message: "db down"}).AnyTimes()
	})

	body := strings.NewReader(`<Delete><Object><Key>k1</Key></Object><Object><Key>k2</Key></Object></Delete>`)
	resp := doReq(t, ts, http.MethodPost, ts.URL+"/mybucket?delete", body)
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	s := string(respBody)

	if !strings.Contains(s, "<Code>ServiceUnavailable</Code>") {
		t.Errorf("typed S3Error Code missing: %s", s)
	}
	if !strings.Contains(s, "<Message>db down</Message>") {
		t.Errorf("typed S3Error Message missing: %s", s)
	}

	// Untyped error case -> InternalError fallback.
	tsUntyped, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().DeleteObjectsBatch(gomock.Any(), gomock.Any()).
			Return(nil, errors.New("untyped backend error")).AnyTimes()
	})

	bodyUntyped := strings.NewReader(`<Delete><Object><Key>k1</Key></Object></Delete>`)
	respUntyped := doReq(t, ts, http.MethodPost, tsUntyped.URL+"/mybucket?delete", bodyUntyped)
	defer respUntyped.Body.Close()
	respBodyUntyped, _ := io.ReadAll(respUntyped.Body)
	su := string(respBodyUntyped)

	if !strings.Contains(su, "<Code>InternalError</Code>") {
		t.Errorf("untyped error should map to InternalError fallback: %s", su)
	}
}

// -------------------------------------------------------------------------
// COPY
// -------------------------------------------------------------------------

// TestCopy_Success verifies the copy success contract.
// Asserts that status = , want 200. body:.
func TestCopy_Success(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/source-key", BackendName: "b1", SizeBytes: 7},
			}, nil).AnyTimes()
	})

	// Pre-store source object
	backend.Objects["mybucket/source-key"] = backendtest.Object{
		Data: []byte("copy me"), ContentType: "text/plain", ETag: `"src"`,
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/dest-key", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("X-Amz-Copy-Source", "/mybucket/source-key")
	req.ContentLength = 0
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200. body: %s", resp.StatusCode, body)
	}

	// Verify the response is valid XML
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "CopyObjectResult") {
		t.Error("response missing CopyObjectResult element")
	}
}

// TestCopy_SourceNotFound verifies the copy source not found contract.
// Asserts that status = , want 404.
func TestCopy_SourceNotFound(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return(nil, core.ErrObjectNotFound).AnyTimes()
	})

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/dest-key", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("X-Amz-Copy-Source", "/mybucket/no-such-key")
	req.ContentLength = 0
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestCopy_URLEncodedSource verifies the copy urlencoded source contract.
// Asserts that status = , want 200. body:.
func TestCopy_URLEncodedSource(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/my file.txt", BackendName: "b1", SizeBytes: 7},
			}, nil).AnyTimes()
	})

	// Pre-store source object with a space in the key
	backend.Objects["mybucket/my file.txt"] = backendtest.Object{
		Data: []byte("encoded"), ContentType: "text/plain", ETag: `"enc"`,
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/dest-key", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("X-Amz-Copy-Source", "/mybucket/my%20file.txt")
	req.ContentLength = 0
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200. body: %s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "CopyObjectResult") {
		t.Error("response missing CopyObjectResult element")
	}
}

// TestCopy_CrossBucketDenied verifies the copy cross bucket denied contract.
// Asserts that status = , want 403.
func TestCopy_CrossBucketDenied(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/dest-key", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("X-Amz-Copy-Source", "/otherbucket/source-key")
	req.ContentLength = 0
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// -------------------------------------------------------------------------
// AUTH
// -------------------------------------------------------------------------

// TestAuth_BadCredentials verifies the auth bad credentials contract.
// Asserts that status = , want 403.
func TestAuth_BadCredentials(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "wrong-token")
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// TestAuth_BucketMismatch verifies the auth bucket mismatch contract.
// Asserts that status = , want 403.
func TestAuth_BucketMismatch(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	// Token is valid for "mybucket" but request goes to "otherbucket"
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/otherbucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// TestAuth_AccessDeniedDoesNotLeakBucketName verifies the auth access denied does not leak bucket name path by exercising http.NewRequestWithContext, context.Background, io.ReadAll.
func TestAuth_AccessDeniedDoesNotLeakBucketName(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/otherbucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "otherbucket") {
		t.Error("AccessDenied response should not contain the bucket name")
	}
}

// -------------------------------------------------------------------------
// ROUTING
// -------------------------------------------------------------------------

// TestUnsupportedMethod verifies the unsupported method contract.
// Asserts that status = , want 405.
func TestUnsupportedMethod(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPatch, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

// TestBucketOnlyGET_RoutesToList verifies the bucket only get routes to list contract.
// Asserts that status = , want 200.
func TestBucketOnlyGET_RoutesToList(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().ListObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(&core.ListObjectsResult{
				Objects: []core.ObjectLocation{
					{ObjectKey: "mybucket/file.txt", BackendName: "b1", SizeBytes: 100, CreatedAt: time.Now()},
				},
			}, nil).AnyTimes()
	})

	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/", nil)
	defer resp.Body.Close()

	// Should route to ListObjectsV2 and return XML
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/xml") {
		t.Errorf("Content-Type = %q, want application/xml", ct)
	}
}

// -------------------------------------------------------------------------
// RANGE + CONDITIONAL HEADERS
// -------------------------------------------------------------------------

// rangeCondServer serves one 11-byte object with a known ETag and
// Last-Modified, which is all the conditional cases need.
func rangeCondServer(t *testing.T) (*httptest.Server, time.Time) {
	t.Helper()
	modified := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 11},
			}, nil).AnyTimes()
	})
	backend.Put("mybucket/testkey", &backendtest.Object{
		Data:         []byte("hello world"),
		ContentType:  "text/plain",
		ETag:         `"abc"`,
		LastModified: modified,
	})
	return ts, modified
}

// rangeCondGet issues a ranged GET carrying the supplied conditional headers.
func rangeCondGet(t *testing.T, ts *httptest.Server, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("Range", "bytes=0-4")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestGet_RangeHonorsPreconditions pins the fix for the corruption case: a
// failed precondition aborts the request even when only a range was asked
// for. Serving 206 here let a resumable download splice bytes from a
// replaced object onto what it had already fetched.
func TestGet_RangeHonorsPreconditions(t *testing.T) {
	t.Parallel()
	ts, modified := rangeCondServer(t)

	for _, c := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"if-match mismatch", map[string]string{"If-Match": `"stale"`}, http.StatusPreconditionFailed},
		{"if-match hit", map[string]string{"If-Match": `"abc"`}, http.StatusPartialContent},
		{"if-match star", map[string]string{"If-Match": "*"}, http.StatusPartialContent},
		{"if-none-match hit", map[string]string{"If-None-Match": `"abc"`}, http.StatusNotModified},
		{"if-none-match miss", map[string]string{"If-None-Match": `"other"`}, http.StatusPartialContent},
		{
			"if-unmodified-since older than the object",
			map[string]string{"If-Unmodified-Since": modified.Add(-time.Hour).Format(http.TimeFormat)},
			http.StatusPreconditionFailed,
		},
		{
			"if-modified-since newer than the object",
			map[string]string{"If-Modified-Since": modified.Add(time.Hour).Format(http.TimeFormat)},
			http.StatusNotModified,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			resp := rangeCondGet(t, ts, c.headers)
			defer resp.Body.Close()
			if resp.StatusCode != c.want {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, c.want, body)
			}
		})
	}
}

// TestGet_NotModifiedCarriesValidators asserts a 304 still carries the ETag
// it would have carried on a 200, which is what lets a cache refresh its
// stored validator instead of discarding the entry.
func TestGet_NotModifiedCarriesValidators(t *testing.T) {
	t.Parallel()
	ts, _ := rangeCondServer(t)

	resp := rangeCondGet(t, ts, map[string]string{"If-None-Match": `"abc"`})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.StatusCode)
	}
	if got := resp.Header.Get("ETag"); got != `"abc"` {
		t.Errorf("ETag = %q, want %q on a 304", got, `"abc"`)
	}
	if resp.Header.Get("Last-Modified") == "" {
		t.Error("Last-Modified missing on a 304")
	}
}

// TestGet_IfRange pins the range-or-restart contract: a matching validator
// serves the range, and a stale one serves the whole object instead of a
// partial the client would have spliced onto bytes from another version.
func TestGet_IfRange(t *testing.T) {
	t.Parallel()
	ts, modified := rangeCondServer(t)

	for _, c := range []struct {
		name     string
		ifRange  string
		want     int
		wantBody string
	}{
		{"matching etag serves the range", `"abc"`, http.StatusPartialContent, "hello"},
		{"stale etag serves the whole object", `"stale"`, http.StatusOK, "hello world"},
		{
			"matching date serves the range",
			modified.Format(http.TimeFormat), http.StatusPartialContent, "hello",
		},
		{
			"stale date serves the whole object",
			modified.Add(-time.Hour).Format(http.TimeFormat), http.StatusOK, "hello world",
		},
		// A weak validator cannot safely gate a partial response, so it is
		// treated as a mismatch and the whole object is served.
		{"weak etag serves the whole object", `W/"abc"`, http.StatusOK, "hello world"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			resp := rangeCondGet(t, ts, map[string]string{"If-Range": c.ifRange})
			defer resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.want)
			}
			body, _ := io.ReadAll(resp.Body)
			if string(body) != c.wantBody {
				t.Errorf("body = %q, want %q", body, c.wantBody)
			}
			if c.want == http.StatusOK && resp.Header.Get("Content-Range") != "" {
				t.Error("a full response must not carry Content-Range")
			}
		})
	}
}

// TestGet_RangeWithoutConditionalsStillPartial guards against the fix
// over-reaching: a plain ranged GET is unaffected.
func TestGet_RangeWithoutConditionalsStillPartial(t *testing.T) {
	t.Parallel()
	ts, _ := rangeCondServer(t)

	resp := rangeCondGet(t, ts, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello" {
		t.Errorf("body = %q, want %q", body, "hello")
	}
}

// TestDeleteObjects_RejectsSecondDocument proves the one-document rule is wired
// into the live route, not just the helper. Two concatenated documents used to
// parse as the first alone, so the orchestrator deleted one key set while the
// full body described another.
func TestDeleteObjects_RejectsSecondDocument(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	body := strings.NewReader(
		`<Delete><Object><Key>a.txt</Key></Object></Delete>` +
			`<Delete><Object><Key>b.txt</Key></Object></Delete>`)
	resp := doReq(t, ts, http.MethodPost, ts.URL+"/mybucket?delete", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestDeleteObjects_RejectsOversizedBody proves an over-ceiling body is
// reported as too large through the live route, rather than being truncated
// and blamed on the client's XML.
func TestDeleteObjects_RejectsOversizedBody(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	var b strings.Builder
	b.WriteString(`<Delete>`)
	for b.Len() < maxDeleteObjectsBody+1024 {
		b.WriteString(`<Object><Key>` + strings.Repeat("k", 1024) + `</Key></Object>`)
	}
	b.WriteString(`</Delete>`)

	resp := doReq(t, ts, http.MethodPost, ts.URL+"/mybucket?delete", strings.NewReader(b.String()))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

// TestDeleteObjects_AcceptsMaximumLegalRequest pins the ceiling above what S3
// permits: 1000 keys at the maximum key length. The previous 1 MB limit sat
// under this, so a legal request was truncated and rejected as malformed.
func TestDeleteObjects_AcceptsMaximumLegalRequest(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	var b strings.Builder
	b.WriteString(`<Delete>`)
	for i := range 1000 {
		fmt.Fprintf(&b, `<Object><Key>%s%03d</Key></Object>`, strings.Repeat("k", 1021), i)
	}
	b.WriteString(`</Delete>`)

	resp := doReq(t, ts, http.MethodPost, ts.URL+"/mybucket?delete", strings.NewReader(b.String()))
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		t.Fatalf("a legal 1000-key request must not be rejected as too large (body %d bytes)", b.Len())
	}
}
