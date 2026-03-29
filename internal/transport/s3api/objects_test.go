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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/config"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/testutil"

	// serverMockBackend implements storage.ObjectBackend for server handler tests.
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

type serverMockBackend struct {
	mu      sync.Mutex
	objects map[string]serverMockObj
	putErr  error
	getErr  error
	headErr error
	delErr  error
}

type serverMockObj struct {
	data         []byte
	contentType  string
	etag         string
	lastModified time.Time
	metadata     map[string]string
}

func newServerMockBackend() *serverMockBackend {
	return &serverMockBackend{objects: make(map[string]serverMockObj)}
}

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

func (b *serverMockBackend) GetObject(_ context.Context, key string, _ string) (*s3be.GetObjectResult, error) {
	if b.getErr != nil {
		return nil, b.getErr
	}
	b.mu.Lock()
	obj, ok := b.objects[key]
	b.mu.Unlock()
	if !ok {
		return nil, store.ErrObjectNotFound
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

func (b *serverMockBackend) HeadObject(_ context.Context, key string) (*s3be.HeadObjectResult, error) {
	if b.headErr != nil {
		return nil, b.headErr
	}
	b.mu.Lock()
	obj, ok := b.objects[key]
	b.mu.Unlock()
	if !ok {
		return nil, store.ErrObjectNotFound
	}
	return &s3be.HeadObjectResult{
		Size:         int64(len(obj.data)),
		ContentType:  obj.contentType,
		ETag:         obj.etag,
		LastModified: obj.lastModified,
		Metadata:     obj.metadata,
	}, nil
}

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
		Store:           mockStore,
		Order:           []string{"b1"},
		RoutingStrategy: config.RoutingPack,
	})
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
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Proxy-Token", "test-token")
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// -------------------------------------------------------------------------
// PUT
// -------------------------------------------------------------------------

func TestPut_Success(t *testing.T) {
	ts, _, backend := newTestServer(t)
	data := []byte("hello world")

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket/testkey", bytes.NewReader(data))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("Content-Type", "text/plain")
	req.ContentLength = int64(len(data))
	resp, err := http.DefaultClient.Do(req)
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

func TestPut_MissingContentLength(t *testing.T) {
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket/testkey", strings.NewReader("data"))
	req.Header.Set("X-Proxy-Token", "test-token")
	// Explicitly set ContentLength to -1 to simulate missing Content-Length
	req.ContentLength = -1
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusLengthRequired {
		t.Fatalf("status = %d, want 411", resp.StatusCode)
	}
}

func TestPut_EntityTooLarge(t *testing.T) {
	ts, _, _ := newTestServer(t)

	// Create a body whose size exceeds the limit.
	// We use a LimitReader wrapping zeros so we don't allocate 20MB.
	bigSize := int64(20 * 1024 * 1024)
	body := io.LimitReader(neverEndingReader{}, bigSize)

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket/testkey", body)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.ContentLength = bigSize
	resp, err := http.DefaultClient.Do(req)
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

func (neverEndingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestPut_QuotaExhausted(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	mockStore.GetBackendErr = store.ErrNoSpaceAvailable

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket/testkey", strings.NewReader("data"))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.ContentLength = 4
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507", resp.StatusCode)
	}
}

func TestPut_DBUnavailable(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	mockStore.GetBackendErr = store.ErrDBUnavailable

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket/testkey", strings.NewReader("data"))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.ContentLength = 4
	resp, err := http.DefaultClient.Do(req)
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

func TestGet_Success(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	// Pre-store an object
	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
	}
	mockStore.GetAllLocationsResp = []store.ObjectLocation{
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

func TestGet_NotFound(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	mockStore.GetAllLocationsErr = store.ErrObjectNotFound

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/nonexistent", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// -------------------------------------------------------------------------
// HEAD
// -------------------------------------------------------------------------

func TestHead_Success(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("12345"), contentType: "text/plain", etag: `"abc"`,
	}
	mockStore.GetAllLocationsResp = []store.ObjectLocation{
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

func TestHead_NotFound(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	mockStore.GetAllLocationsErr = store.ErrObjectNotFound

	resp := doReq(t, http.MethodHead, ts.URL+"/mybucket/nonexistent", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// -------------------------------------------------------------------------
// CONDITIONAL REQUESTS
// -------------------------------------------------------------------------

func TestGet_LastModifiedHeader(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
	}
	mockStore.GetAllLocationsResp = []store.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5,
			CreatedAt: time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)},
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// LastModified comes from the backend, not the store — the mock backend
	// doesn't set it, so the header should be absent for this test.
	// This test validates that the header is at least not causing errors.
	if resp.Header.Get("ETag") != `"abc"` {
		t.Errorf("ETag = %q, want %q", resp.Header.Get("ETag"), `"abc"`)
	}
}

func TestGet_ConditionalIfNoneMatch(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
	}
	mockStore.GetAllLocationsResp = []store.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-None-Match", `"abc"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.StatusCode)
	}
}

func TestGet_ConditionalIfNoneMatchMismatch(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
	}
	mockStore.GetAllLocationsResp = []store.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-None-Match", `"different"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestGet_ConditionalIfMatch(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
	}
	mockStore.GetAllLocationsResp = []store.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-Match", `"wrong"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", resp.StatusCode)
	}
}

func TestHead_ConditionalIfNoneMatch(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
	}
	mockStore.GetAllLocationsResp = []store.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	req, _ := http.NewRequest(http.MethodHead, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-None-Match", `"abc"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.StatusCode)
	}
}

func TestGet_ConditionalIfModifiedSince(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	objTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
		lastModified: objTime,
	}
	mockStore.GetAllLocationsResp = []store.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	// Request with a time after the object's last modification → 304
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-Modified-Since", objTime.Add(time.Hour).UTC().Format(http.TimeFormat))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.StatusCode)
	}
}

func TestGet_ConditionalIfModifiedSinceNewer(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	objTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
		lastModified: objTime,
	}
	mockStore.GetAllLocationsResp = []store.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	// Request with a time before the object's last modification → 200
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-Modified-Since", objTime.Add(-time.Hour).UTC().Format(http.TimeFormat))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestGet_ConditionalIfUnmodifiedSince(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	objTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
		lastModified: objTime,
	}
	mockStore.GetAllLocationsResp = []store.ObjectLocation{
		{ObjectKey: "mybucket/testkey", BackendName: "b1", SizeBytes: 5},
	}

	// Object was modified after the given time → 412
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("If-Unmodified-Since", objTime.Add(-time.Hour).UTC().Format(http.TimeFormat))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", resp.StatusCode)
	}
}

func TestGet_LastModifiedHeaderSet(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	objTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
		lastModified: objTime,
	}
	mockStore.GetAllLocationsResp = []store.ObjectLocation{
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

func TestHead_LastModifiedHeaderSet(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	objTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	backend.objects["mybucket/testkey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
		lastModified: objTime,
	}
	mockStore.GetAllLocationsResp = []store.ObjectLocation{
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

func TestPut_MetadataStored(t *testing.T) {
	ts, _, backend := newTestServer(t)
	data := []byte("hello")

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket/metakey", bytes.NewReader(data))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Amz-Meta-Project", "acme")
	req.Header.Set("X-Amz-Meta-Env", "prod")
	req.ContentLength = int64(len(data))
	resp, err := http.DefaultClient.Do(req)
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

func TestGet_MetadataReturned(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/metakey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
		metadata: map[string]string{"project": "acme", "env": "prod"},
	}
	mockStore.GetAllLocationsResp = []store.ObjectLocation{
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

func TestHead_MetadataReturned(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/metakey"] = serverMockObj{
		data: []byte("hello"), contentType: "text/plain", etag: `"abc"`,
		metadata: map[string]string{"project": "acme"},
	}
	mockStore.GetAllLocationsResp = []store.ObjectLocation{
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

func TestPut_MetadataTooLarge(t *testing.T) {
	ts, _, _ := newTestServer(t)
	data := []byte("hello")

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket/metakey", bytes.NewReader(data))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Amz-Meta-Big", strings.Repeat("x", maxUserMetadataBytes+1))
	req.ContentLength = int64(len(data))
	resp, err := http.DefaultClient.Do(req)
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

func TestDelete_Success(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/testkey"] = serverMockObj{data: []byte("hi")}
	mockStore.DeleteObjectResp = []store.DeletedCopy{
		{BackendName: "b1", SizeBytes: 2},
	}

	resp := doReq(t, http.MethodDelete, ts.URL+"/mybucket/testkey", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

func TestDelete_IdempotentForMissing(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	mockStore.DeleteObjectErr = store.ErrObjectNotFound

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

func TestDeleteObjects_Success(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	backend.objects["mybucket/key1"] = serverMockObj{data: []byte("a")}
	backend.objects["mybucket/key2"] = serverMockObj{data: []byte("b")}
	mockStore.DeleteObjectFunc = func(key string) ([]store.DeletedCopy, error) {
		return []store.DeletedCopy{{BackendName: "b1", SizeBytes: 1}}, nil
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

func TestDeleteObjects_QuietMode(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	mockStore.DeleteObjectFunc = func(key string) ([]store.DeletedCopy, error) {
		return []store.DeletedCopy{{BackendName: "b1", SizeBytes: 1}}, nil
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

func TestDeleteObjects_MalformedXML(t *testing.T) {
	ts, _, _ := newTestServer(t)

	body := strings.NewReader(`not xml at all`)
	resp := doReq(t, http.MethodPost, ts.URL+"/mybucket?delete", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestDeleteObjects_TooManyObjects(t *testing.T) {
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

func TestDeleteObjects_EmptyRequest(t *testing.T) {
	ts, _, _ := newTestServer(t)

	body := strings.NewReader(`<Delete></Delete>`)
	resp := doReq(t, http.MethodPost, ts.URL+"/mybucket?delete", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestDeleteObjects_PartialFailure(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	mockStore.DeleteObjectFunc = func(key string) ([]store.DeletedCopy, error) {
		if key == "mybucket/bad" {
			return nil, &store.S3Error{StatusCode: 500, Code: "InternalError", Message: "db error"}
		}
		return []store.DeletedCopy{{BackendName: "b1", SizeBytes: 1}}, nil
	}

	body := strings.NewReader(`<Delete><Object><Key>good</Key></Object><Object><Key>bad</Key></Object></Delete>`)
	resp := doReq(t, http.MethodPost, ts.URL+"/mybucket?delete", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (partial failures still return 200)", resp.StatusCode)
	}
	respBody, _ := io.ReadAll(resp.Body)
	s := string(respBody)
	if !strings.Contains(s, "<Deleted>") {
		t.Error("response missing <Deleted> for successful key")
	}
	if !strings.Contains(s, "<Error>") {
		t.Error("response missing <Error> for failed key")
	}
}

// -------------------------------------------------------------------------
// COPY
// -------------------------------------------------------------------------

func TestCopy_Success(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	// Pre-store source object
	backend.objects["mybucket/source-key"] = serverMockObj{
		data: []byte("copy me"), contentType: "text/plain", etag: `"src"`,
	}
	mockStore.GetAllLocationsResp = []store.ObjectLocation{
		{ObjectKey: "mybucket/source-key", BackendName: "b1", SizeBytes: 7},
	}

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket/dest-key", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("X-Amz-Copy-Source", "/mybucket/source-key")
	req.ContentLength = 0
	resp, err := http.DefaultClient.Do(req)
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

func TestCopy_SourceNotFound(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	mockStore.GetAllLocationsErr = store.ErrObjectNotFound

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket/dest-key", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("X-Amz-Copy-Source", "/mybucket/no-such-key")
	req.ContentLength = 0
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestCopy_URLEncodedSource(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	// Pre-store source object with a space in the key
	backend.objects["mybucket/my file.txt"] = serverMockObj{
		data: []byte("encoded"), contentType: "text/plain", etag: `"enc"`,
	}
	mockStore.GetAllLocationsResp = []store.ObjectLocation{
		{ObjectKey: "mybucket/my file.txt", BackendName: "b1", SizeBytes: 7},
	}

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket/dest-key", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("X-Amz-Copy-Source", "/mybucket/my%20file.txt")
	req.ContentLength = 0
	resp, err := http.DefaultClient.Do(req)
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

func TestCopy_CrossBucketDenied(t *testing.T) {
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket/dest-key", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("X-Amz-Copy-Source", "/otherbucket/source-key")
	req.ContentLength = 0
	resp, err := http.DefaultClient.Do(req)
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

func TestAuth_BadCredentials(t *testing.T) {
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "wrong-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestAuth_BucketMismatch(t *testing.T) {
	ts, _, _ := newTestServer(t)

	// Token is valid for "mybucket" but request goes to "otherbucket"
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/otherbucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestAuth_AccessDeniedDoesNotLeakBucketName(t *testing.T) {
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/otherbucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := http.DefaultClient.Do(req)
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

func TestUnsupportedMethod(t *testing.T) {
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/mybucket/testkey", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestBucketOnlyGET_RoutesToList(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	mockStore.ListObjectsResp = &store.ListObjectsResult{
		Objects: []store.ObjectLocation{
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
