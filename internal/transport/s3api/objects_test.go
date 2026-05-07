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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/testutil"

	// serverMockBackend implements storage.ObjectBackend for server handler tests.
	"github.com/afreidah/s3-orchestrator/internal/proxy"
)

// serverMockBackend is the in-memory ObjectBackend used by S3-handler
// tests in this file. Holds objects in a map and lets tests inject
// per-method errors (putErr, getErr, headErr, delErr) so each handler
// branch can be exercised without spinning up MinIO.
type serverMockBackend struct {
	mu      sync.Mutex
	objects map[string]serverMockObj
	putErr  error
	getErr  error
	headErr error
	delErr  error
}

// serverMockObj is one stored object inside serverMockBackend - the
// payload plus the metadata fields the handler-test assertions read
// (content-type, etag, last-modified, user metadata).
type serverMockObj struct {
	data         []byte
	contentType  string
	etag         string
	lastModified time.Time
	metadata     map[string]string
}

// newServerMockBackend constructs a new server mock backend.
func newServerMockBackend() *serverMockBackend {
	return &serverMockBackend{objects: make(map[string]serverMockObj)}
}

// PutObject satisfies backend.ObjectBackend by reading the body into
// memory, recording the resulting object under key, and returning a
// fixed test etag. Honours putErr so error-path tests can inject a
// failure without touching the backend interface.
func (b *serverMockBackend) PutObject(_ context.Context, key string, body io.Reader, _ int64, contentType string, metadata map[string]string) (string, error) {
	if b.putErr != nil {
		return "", b.putErr
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	etag := `"test-etag"`
	b.mu.Lock()
	b.objects[key] = serverMockObj{data: data, contentType: contentType, etag: etag, metadata: metadata}
	b.mu.Unlock()
	return etag, nil
}

// GetObject returns object.
func (b *serverMockBackend) GetObject(_ context.Context, key string, _ string) (*s3be.GetObjectResult, error) {
	if b.getErr != nil {
		return nil, b.getErr
	}
	b.mu.Lock()
	obj, ok := b.objects[key]
	b.mu.Unlock()
	if !ok {
		return nil, core.ErrObjectNotFound
	}
	return &s3be.GetObjectResult{
		Body:         io.NopCloser(bytes.NewReader(obj.data)),
		Size:         int64(len(obj.data)),
		ContentType:  obj.contentType,
		ETag:         obj.etag,
		LastModified: obj.lastModified,
		Metadata:     obj.metadata,
	}, nil
}

// HeadObject satisfies backend.ObjectBackend by returning the stored
// object's metadata. Honours headErr so HEAD-error path tests can
// inject a failure independently of GET/PUT.
func (b *serverMockBackend) HeadObject(_ context.Context, key string) (*s3be.HeadObjectResult, error) {
	if b.headErr != nil {
		return nil, b.headErr
	}
	b.mu.Lock()
	obj, ok := b.objects[key]
	b.mu.Unlock()
	if !ok {
		return nil, core.ErrObjectNotFound
	}
	return &s3be.HeadObjectResult{
		Size:         int64(len(obj.data)),
		ContentType:  obj.contentType,
		ETag:         obj.etag,
		LastModified: obj.lastModified,
		Metadata:     obj.metadata,
	}, nil
}

// DeleteObject deletes object.
func (b *serverMockBackend) DeleteObject(_ context.Context, key string) error {
	if b.delErr != nil {
		return b.delErr
	}
	b.mu.Lock()
	delete(b.objects, key)
	b.mu.Unlock()
	return nil
}

// newTestServer creates an httptest.Server wired with mock backends and store.
// Returns the server, a cleanup func, and the mock store/backend for assertions.
func newTestServer(t *testing.T) (*httptest.Server, *testutil.MockStore, *serverMockBackend) {
	t.Helper()

	backend := newServerMockBackend()
	mockStore := &testutil.MockStore{
		GetBackendResp: "b1",
	}

	mgr := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"b1": backend},
		Stores:          proxytest.StoresFromMock(mockStore),
		Dashboard:       mockStore,
		Metrics:         mockStore,
		Order:           []string{"b1"},
		RoutingStrategy: config.RoutingPack,
	})
	proxytest.AttachWorkers(mgr, mockStore)
	t.Cleanup(mgr.Close)

	srv := &Server{
		Manager:       mgr,
		MaxObjectSize: 10 * 1024 * 1024, // 10MB
	}

	buckets := []config.BucketConfig{
		{Name: "mybucket", Credentials: []config.CredentialConfig{
			{Token: "test-token"},
		}},
	}
	srv.SetBucketAuth(auth.NewBucketRegistry(buckets))

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	return ts, mockStore, backend
}

// doReq is a helper to send requests to the test server with auth.
func doReq(t *testing.T, method, url string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Proxy-Token", "test-token")
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	if _, ok := backend.objects["mybucket/testkey"]; !ok {
		t.Error("object not stored on backend")
	}
}

// TestPut_MissingContentLength verifies the put missing content length contract.
// Asserts that status = , want 411.
func TestPut_MissingContentLength(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey", strings.NewReader("data"))
	req.Header.Set("X-Proxy-Token", "test-token")
	// Explicitly set ContentLength to -1 to simulate missing Content-Length
	req.ContentLength = -1
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	ts, _, _ := newTestServer(t)

	// Create a body whose size exceeds the limit.
	// We use a LimitReader wrapping zeros so we don't allocate 20MB.
	bigSize := int64(20 * 1024 * 1024)
	body := io.LimitReader(neverEndingReader{}, bigSize)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey", body)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.ContentLength = bigSize
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	ts, mockStore, backend := newTestServer(t)
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "test-backend", SizeBytes: 100},
	}

	data := []byte("hello")
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey", bytes.NewReader(data))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-None-Match", "*")
	req.ContentLength = int64(len(data))
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", resp.StatusCode)
	}
	if _, ok := backend.objects["mybucket/testkey"]; ok {
		t.Error("backend should not have stored bytes when precondition fails")
	}
}

// TestPut_IfNoneMatchStarAllowsNewKey verifies that PutObject with
// `If-None-Match: *` succeeds when no location row exists for the key.
func TestPut_IfNoneMatchStarAllowsNewKey(t *testing.T) {
	t.Parallel()
	ts, _, backend := newTestServer(t)

	data := []byte("hello")
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/newkey", bytes.NewReader(data))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-None-Match", "*")
	req.ContentLength = int64(len(data))
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if _, ok := backend.objects["mybucket/newkey"]; !ok {
		t.Error("object not stored on backend")
	}
}

// TestPut_IfNoneMatchSpecificETagIgnored verifies that an `If-None-Match`
// header carrying a specific etag (not `*`) is ignored on PUT. AWS S3
// only honors the `*` form for write preconditions; specific-etag forms
// are accepted and the upload proceeds normally.
func TestPut_IfNoneMatchSpecificETagIgnored(t *testing.T) {
	t.Parallel()
	ts, mockStore, backend := newTestServer(t)
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "test-backend", SizeBytes: 100},
	}

	data := []byte("hello")
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey", bytes.NewReader(data))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-None-Match", `"some-etag"`)
	req.ContentLength = int64(len(data))
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (specific etag form ignored on PUT)", resp.StatusCode)
	}
	if _, ok := backend.objects["mybucket/testkey"]; !ok {
		t.Error("object not stored on backend")
	}
}

// TestPut_QuotaExhausted verifies the put quota exhausted contract.
// Asserts that status = , want 507.
func TestPut_QuotaExhausted(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	mockStore.GetBackendErr = core.ErrNoSpaceAvailable

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey", strings.NewReader("data"))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.ContentLength = 4
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507", resp.StatusCode)
	}
}

// TestPut_DBUnavailable verifies the put dbunavailable contract.
// Asserts that status = , want 503.
func TestPut_DBUnavailable(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	mockStore.GetBackendErr = core.ErrDBUnavailable

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey", strings.NewReader("data"))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.ContentLength = 4
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	ts, mockStore, backend := newTestServer(t)

	// Pre-store an object
	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
	}
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/testkey", nil)
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
	ts, mockStore, _ := newTestServer(t)
	mockStore.GetAllLocationsErr = core.ErrObjectNotFound

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/nonexistent", nil)
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
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("12345"), contentType: "text/plain", etag: `"abc"`,
	}
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	resp := doReq(t, http.MethodHead, ts.URL+"/mybucket/testkey", nil)
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
	ts, mockStore, _ := newTestServer(t)
	mockStore.GetAllLocationsErr = core.ErrObjectNotFound

	resp := doReq(t, http.MethodHead, ts.URL+"/mybucket/nonexistent", nil)
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
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
	}
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5,
			CreatedAt: time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)},
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/testkey", nil)
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
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
	}
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-None-Match", `"abc"`)
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
	}
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-None-Match", `"different"`)
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
	}
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-Match", `"wrong"`)
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
	}
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodHead, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-None-Match", `"abc"`)
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	ts, mockStore, backend := newTestServer(t)

	objTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
		lastModified: objTime,
	}
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	// Request with a time after the object's last modification -> 304
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-Modified-Since", objTime.Add(time.Hour).UTC().Format(http.TimeFormat))
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	ts, mockStore, backend := newTestServer(t)

	objTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
		lastModified: objTime,
	}
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	// Request with a time before the object's last modification -> 200
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-Modified-Since", objTime.Add(-time.Hour).UTC().Format(http.TimeFormat))
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	ts, mockStore, backend := newTestServer(t)

	objTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
		lastModified: objTime,
	}
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	// Object was modified after the given time -> 412
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-Unmodified-Since", objTime.Add(-time.Hour).UTC().Format(http.TimeFormat))
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	ts, mockStore, backend := newTestServer(t)

	objTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
		lastModified: objTime,
	}
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/testkey", nil)
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
	ts, mockStore, backend := newTestServer(t)

	objTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
		lastModified: objTime,
	}
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	resp := doReq(t, http.MethodHead, ts.URL+"/mybucket/testkey", nil)
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
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	obj, ok := backend.objects["mybucket/metakey"]
	if !ok {
		t.Fatal("object not stored")
	}
	if obj.metadata["project"] != "acme" || obj.metadata["env"] != "prod" {
		t.Errorf("metadata = %v, want project=acme env=prod", obj.metadata)
	}
}

// TestGet_MetadataReturned verifies the get metadata returned contract.
// Asserts that status = , want 200.
func TestGet_MetadataReturned(t *testing.T) {
	t.Parallel()
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/metakey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
		metadata: map[string]string{"project": "acme", "env": "prod"},
	}
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/metakey", BackendName: "b1", SizeBytes: 5},
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/metakey", nil)
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
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/metakey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
		metadata: map[string]string{"project": "acme"},
	}
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/metakey", BackendName: "b1", SizeBytes: 5},
	}

	resp := doReq(t, http.MethodHead, ts.URL+"/mybucket/metakey", nil)
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
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/testkey"] = serverMockObj{data: []byte("hi")}
	mockStore.DeleteObjectResp = []core.DeletedCopy{
		{BackendName: "b1", SizeBytes: 2},
	}

	resp := doReq(t, http.MethodDelete, ts.URL+"/mybucket/testkey", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

// TestDelete_IdempotentForMissing verifies the delete idempotent for missing contract.
// Asserts that status = , want 204.
func TestDelete_IdempotentForMissing(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	mockStore.DeleteObjectErr = core.ErrObjectNotFound

	resp := doReq(t, http.MethodDelete, ts.URL+"/mybucket/nonexistent", nil)
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
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/key1"] = serverMockObj{data: []byte("a")}
	backend.objects["mybucket/key2"] = serverMockObj{data: []byte("b")}
	mockStore.DeleteObjectFunc = func(key string) ([]core.DeletedCopy, error) {
		return []core.DeletedCopy{{BackendName: "b1", SizeBytes: 1}}, nil
	}

	body := strings.NewReader(`<Delete><Object><Key>key1</Key></Object><Object><Key>key2</Key></Object></Delete>`)
	resp := doReq(t, http.MethodPost, ts.URL+"/mybucket?delete", body)
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
	ts, mockStore, _ := newTestServer(t)
	mockStore.DeleteObjectFunc = func(key string) ([]core.DeletedCopy, error) {
		return []core.DeletedCopy{{BackendName: "b1", SizeBytes: 1}}, nil
	}

	body := strings.NewReader(`<Delete><Quiet>true</Quiet><Object><Key>key1</Key></Object></Delete>`)
	resp := doReq(t, http.MethodPost, ts.URL+"/mybucket?delete", body)
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
	resp := doReq(t, http.MethodPost, ts.URL+"/mybucket?delete", body)
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

	resp := doReq(t, http.MethodPost, ts.URL+"/mybucket?delete", strings.NewReader(sb.String()))
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
	resp := doReq(t, http.MethodPost, ts.URL+"/mybucket?delete", body)
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
	ts, mockStore, _ := newTestServer(t)
	mockStore.DeleteObjectsBatchErr = &core.S3Error{StatusCode: 500, Code: "InternalError", Message: "db error"}

	body := strings.NewReader(`<Delete><Object><Key>good</Key></Object><Object><Key>bad</Key></Object></Delete>`)
	resp := doReq(t, http.MethodPost, ts.URL+"/mybucket?delete", body)
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
	ts, mockStore, _ := newTestServer(t)
	mockStore.DeleteObjectsBatchErr = &core.S3Error{StatusCode: 503, Code: "ServiceUnavailable", Message: "db down"}

	body := strings.NewReader(`<Delete><Object><Key>k1</Key></Object><Object><Key>k2</Key></Object></Delete>`)
	resp := doReq(t, http.MethodPost, ts.URL+"/mybucket?delete", body)
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
	tsUntyped, mockStoreUntyped, _ := newTestServer(t)
	mockStoreUntyped.DeleteObjectsBatchErr = errors.New("untyped backend error")

	bodyUntyped := strings.NewReader(`<Delete><Object><Key>k1</Key></Object></Delete>`)
	respUntyped := doReq(t, http.MethodPost, tsUntyped.URL+"/mybucket?delete", bodyUntyped)
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
	ts, mockStore, backend := newTestServer(t)

	// Pre-store source object
	backend.objects["mybucket/source-key"] = serverMockObj{
		data: []byte("copy me"), contentType: "text/plain", etag: `"src"`,
	}
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/source-key", BackendName: "b1", SizeBytes: 7},
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/dest-key", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("X-Amz-Copy-Source", "/mybucket/source-key")
	req.ContentLength = 0
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	ts, mockStore, _ := newTestServer(t)
	mockStore.GetAllLocationsErr = core.ErrObjectNotFound

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/dest-key", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("X-Amz-Copy-Source", "/mybucket/no-such-key")
	req.ContentLength = 0
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	ts, mockStore, backend := newTestServer(t)

	// Pre-store source object with a space in the key
	backend.objects["mybucket/my file.txt"] = serverMockObj{
		data: []byte("encoded"), contentType: "text/plain", etag: `"enc"`,
	}
	mockStore.GetAllLocationsResp = []core.ObjectLocation{
		{ObjectKey: "mybucket/my file.txt", BackendName: "b1", SizeBytes: 7},
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/dest-key", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("X-Amz-Copy-Source", "/mybucket/my%20file.txt")
	req.ContentLength = 0
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
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
	ts, mockStore, _ := newTestServer(t)
	mockStore.ListObjectsResp = &core.ListObjectsResult{
		Objects: []core.ObjectLocation{
			{ObjectKey: "mybucket/file.txt", BackendName: "b1", SizeBytes: 100, CreatedAt: time.Now()},
		},
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/", nil)
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
