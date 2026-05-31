// -------------------------------------------------------------------------------
// Multipart Upload Handler Tests
//
// Author: Alex Freidah
//
// Tests for S3 multipart upload HTTP handlers: create upload, upload part,
// complete upload, abort upload, list parts, and list multipart uploads.
// Validates request parsing, error responses, and storage layer interaction
// via the test server harness.
// -------------------------------------------------------------------------------

package s3api

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/testutil"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
)

// -------------------------------------------------------------------------
// CreateMultipartUpload
// -------------------------------------------------------------------------

// TestCreateMultipartUpload_Success verifies the create multipart upload success contract.
// Asserts that status = , want 200. body:.
func TestCreateMultipartUpload_Success(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)

	mockStore.GetMultipartResp = &core.MultipartUpload{
		UploadID:    "test-upload-id",
		ObjectKey:   "mybucket/testkey",
		BackendName: "b1",
		ContentType: "text/plain",
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mybucket/testkey?uploads", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("Content-Type", "text/plain")
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL is localhost, not tainted
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200. body: %s", resp.StatusCode, body)
	}

	var result initiateMultipartUploadResult
	if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode XML response: %v", err)
	}
	if result.Bucket != "mybucket" {
		t.Errorf("Bucket = %q, want %q", result.Bucket, "mybucket")
	}
	if result.Key != "testkey" {
		t.Errorf("Key = %q, want %q", result.Key, "testkey")
	}
	if result.UploadId == "" {
		t.Error("expected non-empty UploadId")
	}
}

// TestCreateMultipartUpload_StoreError verifies the create multipart upload store error contract.
// Asserts that status = , want 500.
func TestCreateMultipartUpload_StoreError(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	mockStore.CreateMultipartErr = &core.S3Error{
		StatusCode: 500,
		Code:       "InternalError",
		Message:    "db error",
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mybucket/testkey?uploads", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL is localhost, not tainted
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

// TestCreateMultipartUpload_DefaultContentType verifies the create multipart upload default content type contract.
// Asserts that status = , want 200.
func TestCreateMultipartUpload_DefaultContentType(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)

	mockStore.GetMultipartResp = &core.MultipartUpload{
		UploadID:    "test-upload-id",
		ObjectKey:   "mybucket/testkey",
		BackendName: "b1",
		ContentType: "application/octet-stream",
	}

	// No Content-Type header  -  should default to application/octet-stream
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mybucket/testkey?uploads", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL is localhost, not tainted
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestCreateMultipartUpload_MetadataTooLarge verifies the create multipart upload metadata too large contract.
// Asserts that status = , want 400.
func TestCreateMultipartUpload_MetadataTooLarge(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mybucket/testkey?uploads", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("X-Amz-Meta-Big", strings.Repeat("x", maxUserMetadataBytes+1))
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL is localhost, not tainted
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// -------------------------------------------------------------------------
// UploadPart
// -------------------------------------------------------------------------

// TestUploadPart_Success verifies the upload part success contract.
// Asserts that status = , want 200. body:.
func TestUploadPart_Success(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)

	mockStore.GetMultipartResp = &core.MultipartUpload{
		UploadID:    "upload-1",
		ObjectKey:   "mybucket/testkey",
		BackendName: "b1",
		ContentType: "application/octet-stream",
	}

	body := strings.NewReader("part-data")
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey?uploadId=upload-1&partNumber=1", body)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.ContentLength = int64(len("part-data"))
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL is localhost, not tainted
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200. body: %s", resp.StatusCode, respBody)
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("expected ETag header")
	}
}

// TestUploadPart_InvalidPartNumber verifies the upload part invalid part number contract.
// Asserts that status = , want 400.
func TestUploadPart_InvalidPartNumber(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey?uploadId=upload-1&partNumber=abc", strings.NewReader("data"))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.ContentLength = 4
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL is localhost, not tainted
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestUploadPart_ZeroPartNumber verifies the upload part zero part number contract.
// Asserts that status = , want 400.
func TestUploadPart_ZeroPartNumber(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey?uploadId=upload-1&partNumber=0", strings.NewReader("data"))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.ContentLength = 4
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL is localhost, not tainted
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestUploadPart_MissingContentLength verifies the upload part missing content length contract.
// Asserts that status = , want 411.
func TestUploadPart_MissingContentLength(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey?uploadId=upload-1&partNumber=1", strings.NewReader("data"))
	req.Header.Set("X-Proxy-Token", "test-token")
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

// TestUploadPart_EntityTooLarge verifies the upload part entity too large contract.
// Asserts that status = , want 413.
func TestUploadPart_EntityTooLarge(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	bigSize := int64(20 * 1024 * 1024)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+"/mybucket/testkey?uploadId=upload-1&partNumber=1", io.LimitReader(neverEndingReader{}, bigSize))
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

// -------------------------------------------------------------------------
// CompleteMultipartUpload
// -------------------------------------------------------------------------

// TestCompleteMultipartUpload_Success verifies the complete multipart upload success contract.
// Asserts that status = , want 200. body:.
func TestCompleteMultipartUpload_Success(t *testing.T) {
	t.Parallel()
	ts, mockStore, backend := newTestServer(t)

	// Store has a multipart upload with one part
	mockStore.GetMultipartResp = &core.MultipartUpload{
		UploadID:    "upload-1",
		ObjectKey:   "mybucket/testkey",
		BackendName: "b1",
		ContentType: "text/plain",
	}
	mockStore.GetPartsResp = []core.MultipartPart{
		{PartNumber: 1, ETag: `"part1"`, SizeBytes: 4},
	}
	// Pre-store the part object on the backend at the internal part key
	backend.objects["__multipart/upload-1/1"] = serverMockObj{
		data: []byte("data"), contentType: "application/octet-stream", etag: `"part1"`,
	}
	mockStore.GetBackendResp = "b1"

	xmlBody := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"part1"</ETag></Part></CompleteMultipartUpload>`
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mybucket/testkey?uploadId=upload-1", strings.NewReader(xmlBody))
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200. body: %s", resp.StatusCode, body)
	}

	var result completeMultipartUploadResult
	body, _ := io.ReadAll(resp.Body)
	if err := xml.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to decode XML: %v", err)
	}
	if result.Bucket != "mybucket" {
		t.Errorf("Bucket = %q, want %q", result.Bucket, "mybucket")
	}
	if result.Key != "testkey" {
		t.Errorf("Key = %q, want %q", result.Key, "testkey")
	}
}

// TestCompleteMultipartUpload_MalformedXML verifies the complete multipart upload malformed xml contract.
// Asserts that status = , want 400.
func TestCompleteMultipartUpload_MalformedXML(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mybucket/testkey?uploadId=upload-1", strings.NewReader("not xml"))
	req.Header.Set("X-Proxy-Token", "test-token")
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
// AbortMultipartUpload
// -------------------------------------------------------------------------

// TestAbortMultipartUpload_Success verifies the abort multipart upload success contract.
// Asserts that status = , want 204. body:.
func TestAbortMultipartUpload_Success(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)

	mockStore.GetMultipartResp = &core.MultipartUpload{
		UploadID:    "upload-1",
		ObjectKey:   "mybucket/testkey",
		BackendName: "b1",
		ContentType: "text/plain",
	}
	mockStore.GetPartsResp = nil // no parts to clean up

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, ts.URL+"/mybucket/testkey?uploadId=upload-1", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 204. body: %s", resp.StatusCode, body)
	}
}

// TestAbortMultipartUpload_NotFound verifies the abort multipart upload not found contract.
// Asserts that status = , want 404.
func TestAbortMultipartUpload_NotFound(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	mockStore.GetMultipartErr = core.ErrObjectNotFound

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, ts.URL+"/mybucket/testkey?uploadId=nonexistent", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// -------------------------------------------------------------------------
// ListParts
// -------------------------------------------------------------------------

// TestListParts_Success verifies the list parts success contract.
// Asserts that status = , want 200. body:.
func TestListParts_Success(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)

	now := time.Now().UTC()
	mockStore.GetMultipartResp = &core.MultipartUpload{
		UploadID:    "upload-1",
		ObjectKey:   "mybucket/testkey",
		BackendName: "b1",
	}
	mockStore.GetPartsResp = []core.MultipartPart{
		{PartNumber: 1, ETag: `"aaa"`, SizeBytes: 100, CreatedAt: now},
		{PartNumber: 2, ETag: `"bbb"`, SizeBytes: 200, CreatedAt: now},
	}

	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/testkey?uploadId=upload-1", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200. body: %s", resp.StatusCode, body)
	}

	var result listPartsResult
	if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode XML: %v", err)
	}
	if result.Bucket != "mybucket" {
		t.Errorf("Bucket = %q, want %q", result.Bucket, "mybucket")
	}
	if result.Key != "testkey" {
		t.Errorf("Key = %q, want %q", result.Key, "testkey")
	}
	if result.UploadId != "upload-1" {
		t.Errorf("UploadId = %q, want %q", result.UploadId, "upload-1")
	}
	if len(result.Parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(result.Parts))
	}
	if result.Parts[0].PartNumber != 1 || result.Parts[0].Size != 100 {
		t.Errorf("Part[0] = %+v", result.Parts[0])
	}
	if result.Parts[1].PartNumber != 2 || result.Parts[1].Size != 200 {
		t.Errorf("Part[1] = %+v", result.Parts[1])
	}
}

// TestListParts_StoreError verifies the list parts store error contract.
// Asserts that status = , want 500.
func TestListParts_StoreError(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	mockStore.GetMultipartResp = &core.MultipartUpload{
		UploadID:    "upload-1",
		ObjectKey:   "mybucket/testkey",
		BackendName: "b1",
	}
	mockStore.GetPartsErr = &core.S3Error{
		StatusCode: 500,
		Code:       "InternalError",
		Message:    "db error",
	}

	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/testkey?uploadId=upload-1", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

// TestListParts_EmptyParts verifies the list parts empty parts contract.
// Asserts that status = , want 200.
func TestListParts_EmptyParts(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	mockStore.GetMultipartResp = &core.MultipartUpload{
		UploadID:    "upload-1",
		ObjectKey:   "mybucket/testkey",
		BackendName: "b1",
	}
	mockStore.GetPartsResp = nil // no parts

	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/testkey?uploadId=upload-1", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var result listPartsResult
	if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode XML: %v", err)
	}
	if len(result.Parts) != 0 {
		t.Errorf("expected 0 parts, got %d", len(result.Parts))
	}
}

// -------------------------------------------------------------------------
// ListMultipartUploads
// -------------------------------------------------------------------------

// TestListMultipartUploads_Success verifies the list multipart uploads success contract.
// Asserts that status = , want 200. body:.
func TestListMultipartUploads_Success(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	now := time.Now().UTC()

	mockStore.ListMultipartUploadsResp = []core.MultipartUpload{
		{UploadID: "upload-1", ObjectKey: "mybucket/file1.txt", ContentType: "text/plain", CreatedAt: now},
		{UploadID: "upload-2", ObjectKey: "mybucket/file2.txt", ContentType: "text/plain", CreatedAt: now},
	}

	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/?uploads", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200. body: %s", resp.StatusCode, body)
	}

	var result xmlListMultipartUploadsResult
	if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode XML: %v", err)
	}
	if result.Bucket != "mybucket" {
		t.Errorf("Bucket = %q, want %q", result.Bucket, "mybucket")
	}
	if len(result.Upload) != 2 {
		t.Fatalf("expected 2 uploads, got %d", len(result.Upload))
	}
	if result.Upload[0].Key != "file1.txt" {
		t.Errorf("Upload[0].Key = %q, want %q", result.Upload[0].Key, "file1.txt")
	}
	if result.Upload[0].UploadId != "upload-1" {
		t.Errorf("Upload[0].UploadId = %q, want %q", result.Upload[0].UploadId, "upload-1")
	}
	if result.IsTruncated {
		t.Error("expected IsTruncated=false")
	}
}

// TestListMultipartUploads_Empty verifies the list multipart uploads empty contract.
// Asserts that status = , want 200.
func TestListMultipartUploads_Empty(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	mockStore.ListMultipartUploadsResp = nil

	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/?uploads", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var result xmlListMultipartUploadsResult
	if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode XML: %v", err)
	}
	if len(result.Upload) != 0 {
		t.Errorf("expected 0 uploads, got %d", len(result.Upload))
	}
}

// TestListMultipartUploads_Truncated verifies the list multipart uploads truncated contract.
// Asserts that status = , want 200.
func TestListMultipartUploads_Truncated(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	now := time.Now().UTC()

	// Return 3 uploads when max-uploads=2; handler fetches maxUploads+1 to
	// detect truncation, so the mock needs to return 3.
	mockStore.ListMultipartUploadsResp = []core.MultipartUpload{
		{UploadID: "u1", ObjectKey: "mybucket/a.txt", ContentType: "text/plain", CreatedAt: now},
		{UploadID: "u2", ObjectKey: "mybucket/b.txt", ContentType: "text/plain", CreatedAt: now},
		{UploadID: "u3", ObjectKey: "mybucket/c.txt", ContentType: "text/plain", CreatedAt: now},
	}

	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/?uploads&max-uploads=2", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var result xmlListMultipartUploadsResult
	if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode XML: %v", err)
	}
	if !result.IsTruncated {
		t.Error("expected IsTruncated=true")
	}
	if len(result.Upload) != 2 {
		t.Errorf("expected 2 uploads, got %d", len(result.Upload))
	}
	if result.MaxUploads != 2 {
		t.Errorf("MaxUploads = %d, want 2", result.MaxUploads)
	}
}

// TestListMultipartUploads_StoreError verifies the list multipart uploads store error contract.
// Asserts that status = , want 500.
func TestListMultipartUploads_StoreError(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	mockStore.ListMultipartUploadsErr = &core.S3Error{
		StatusCode: 500,
		Code:       "InternalError",
		Message:    "db error",
	}

	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/?uploads", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

// TestListMultipartUploads_NoAuth verifies the list multipart uploads no auth contract.
// Asserts that expected 403, got.
func TestListMultipartUploads_NoAuth(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	getReq, _ := http.NewRequestWithContext(context.Background(), "GET", ts.URL+"/mybucket/?uploads", nil)
	resp, err := ts.Client().Do(getReq) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

// -------------------------------------------------------------------------
// CreateMultipartUpload - per-bucket limit (#984 coverage)
// -------------------------------------------------------------------------

// newTestServerWithMultipartLimit builds an httptest.Server whose
// "mybucket" credential is configured with a per-bucket multipart upload
// cap. Used by the per-bucket limit tests so the limit branch in
// handleCreateMultipartUpload is exercised end-to-end (the default
// newTestServer leaves the cap at 0 == unlimited and never enters that
// branch).
func newTestServerWithMultipartLimit(t *testing.T, maxUploads int) (*httptest.Server, *testutil.MockStore) {
	t.Helper()

	backend := newServerMockBackend()
	mockStore := testutil.NewMockStore(t)
	mockStore.GetBackendResp = "b1"

	mgr := proxytest.NewManager(t, &proxy.BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"b1": backend},
		Stores:          mockStore,
		Dashboard:       mockStore,
		Metrics:         mockStore,
		Order:           []string{"b1"},
		RoutingStrategy: config.RoutingPack,
	})
	_ = proxytest.BuildWorkers(mgr, mockStore)
	t.Cleanup(mgr.Close)

	srv := &Server{Manager: mgr, MaxObjectSize: 10 * 1024 * 1024}
	buckets := []config.BucketConfig{{
		Name:                "mybucket",
		MaxMultipartUploads: maxUploads,
		Credentials:         []config.CredentialConfig{{Token: "test-token"}},
	}}
	srv.SetBucketAuth(auth.NewBucketRegistry(buckets))

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, mockStore
}

// TestCreateMultipartUpload_PerBucketLimit_Exceeded pins the 503 response
// when the active-upload count is at or above the per-bucket cap.
func TestCreateMultipartUpload_PerBucketLimit_Exceeded(t *testing.T) {
	t.Parallel()
	ts, mockStore := newTestServerWithMultipartLimit(t, 2)
	mockStore.CountActiveMultipartResp = 2 // already at limit

	resp := doReq(t, ts, http.MethodPost, ts.URL+"/mybucket/k?uploads", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 503. body: %s", resp.StatusCode, body)
	}
}

// TestCreateMultipartUpload_PerBucketLimit_StoreError pins that a
// CountActiveMultipartUploads failure surfaces as a storage error
// instead of letting the create proceed unguarded.
func TestCreateMultipartUpload_PerBucketLimit_StoreError(t *testing.T) {
	t.Parallel()
	ts, mockStore := newTestServerWithMultipartLimit(t, 1)
	mockStore.CountActiveMultipartErr = errors.New("count failed")

	resp := doReq(t, ts, http.MethodPost, ts.URL+"/mybucket/k?uploads", nil)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected non-200 on count error, got %d", resp.StatusCode)
	}
}

// TestCreateMultipartUpload_PerBucketLimit_BelowAllows pins the success
// path: when active count is below the cap, the create proceeds.
func TestCreateMultipartUpload_PerBucketLimit_BelowAllows(t *testing.T) {
	t.Parallel()
	ts, mockStore := newTestServerWithMultipartLimit(t, 5)
	mockStore.CountActiveMultipartResp = 1
	mockStore.GetMultipartResp = &core.MultipartUpload{
		UploadID:    "upl-1",
		ObjectKey:   "mybucket/k",
		BackendName: "b1",
		ContentType: "application/octet-stream",
	}

	resp := doReq(t, ts, http.MethodPost, ts.URL+"/mybucket/k?uploads", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200. body: %s", resp.StatusCode, body)
	}
}
