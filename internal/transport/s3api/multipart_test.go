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
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"

	"go.uber.org/mock/gomock"
)

// -------------------------------------------------------------------------
// CreateMultipartUpload
// -------------------------------------------------------------------------

// TestCreateMultipartUpload_Success verifies the create multipart upload success contract.
// Asserts that status = , want 200. body:.
func TestCreateMultipartUpload_Success(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().CreateMultipartUpload(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		m.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
			Return(&core.MultipartUpload{
				UploadID:    "test-upload-id",
				ObjectKey:   "mybucket/testkey",
				BackendName: "b1",
				ContentType: "text/plain",
			}, nil).AnyTimes()
	})

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
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().CreateMultipartUpload(gomock.Any(), gomock.Any()).
			Return(&core.S3Error{
				StatusCode: 500,
				Code:       "InternalError",
				Message:    "db error",
			}).AnyTimes()
	})

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
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().CreateMultipartUpload(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		m.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
			Return(&core.MultipartUpload{
				UploadID:    "test-upload-id",
				ObjectKey:   "mybucket/testkey",
				BackendName: "b1",
				ContentType: "application/octet-stream",
			}, nil).AnyTimes()
	})

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
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
			Return(&core.MultipartUpload{
				UploadID:    "upload-1",
				ObjectKey:   "mybucket/testkey",
				BackendName: "b1",
				ContentType: "application/octet-stream",
			}, nil).AnyTimes()
	})

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
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
			Return(&core.MultipartUpload{
				UploadID:    "upload-1",
				ObjectKey:   "mybucket/testkey",
				BackendName: "b1",
				ContentType: "text/plain",
			}, nil).AnyTimes()
		m.EXPECT().GetParts(gomock.Any(), gomock.Any()).
			Return([]core.MultipartPart{
				{PartNumber: 1, ETag: `"part1"`, SizeBytes: 4},
			}, nil).AnyTimes()
	})

	// Store has a multipart upload with one part
	// Pre-store the part object on the backend at the internal part key
	backend.Objects["__multipart/upload-1/1"] = backendtest.Object{
		Data: []byte("data"), ContentType: "application/octet-stream", ETag: `"part1"`,
	}
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
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
			Return(&core.MultipartUpload{
				UploadID:    "upload-1",
				ObjectKey:   "mybucket/testkey",
				BackendName: "b1",
				ContentType: "text/plain",
			}, nil).AnyTimes()
		m.EXPECT().GetParts(gomock.Any(), gomock.Any()).
			Return(nil, nil).AnyTimes() // no parts to clean up
	})

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
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
			Return(nil, core.ErrObjectNotFound).AnyTimes()
	})

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
	now := time.Now().UTC()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
			Return(&core.MultipartUpload{
				UploadID:    "upload-1",
				ObjectKey:   "mybucket/testkey",
				BackendName: "b1",
			}, nil).AnyTimes()
		m.EXPECT().GetParts(gomock.Any(), gomock.Any()).
			Return([]core.MultipartPart{
				{PartNumber: 1, ETag: `"aaa"`, SizeBytes: 100, CreatedAt: now},
				{PartNumber: 2, ETag: `"bbb"`, SizeBytes: 200, CreatedAt: now},
			}, nil).AnyTimes()
	})

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
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
			Return(&core.MultipartUpload{
				UploadID:    "upload-1",
				ObjectKey:   "mybucket/testkey",
				BackendName: "b1",
			}, nil).AnyTimes()
		m.EXPECT().GetParts(gomock.Any(), gomock.Any()).
			Return(nil, &core.S3Error{
				StatusCode: 500,
				Code:       "InternalError",
				Message:    "db error",
			}).AnyTimes()
	})

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
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
			Return(&core.MultipartUpload{
				UploadID:    "upload-1",
				ObjectKey:   "mybucket/testkey",
				BackendName: "b1",
			}, nil).AnyTimes()
		m.EXPECT().GetParts(gomock.Any(), gomock.Any()).
			Return(nil, nil).AnyTimes() // no parts
	})

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
	now := time.Now().UTC()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().ListMultipartUploads(gomock.Any(), gomock.Any(), gomock.Any()).
			Return([]core.MultipartUpload{
				{UploadID: "upload-1", ObjectKey: "mybucket/file1.txt", ContentType: "text/plain", CreatedAt: now},
				{UploadID: "upload-2", ObjectKey: "mybucket/file2.txt", ContentType: "text/plain", CreatedAt: now},
			}, nil).AnyTimes()
	})
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
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().ListMultipartUploads(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(nil, nil).AnyTimes()
	})

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
	now := time.Now().UTC()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().ListMultipartUploads(gomock.Any(), gomock.Any(), gomock.Any()).
			Return([]core.MultipartUpload{
				{UploadID: "u1", ObjectKey: "mybucket/a.txt", ContentType: "text/plain", CreatedAt: now},
				{UploadID: "u2", ObjectKey: "mybucket/b.txt", ContentType: "text/plain", CreatedAt: now},
				{UploadID: "u3", ObjectKey: "mybucket/c.txt", ContentType: "text/plain", CreatedAt: now},
			}, nil).AnyTimes()
	})
	// Return 3 uploads when max-uploads=2; handler fetches maxUploads+1 to
	// detect truncation, so the mock needs to return 3.
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
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().ListMultipartUploads(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(nil, &core.S3Error{
				StatusCode: 500,
				Code:       "InternalError",
				Message:    "db error",
			}).AnyTimes()
	})

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
func newTestServerWithMultipartLimit(t *testing.T, maxUploads int, opts ...func(*storetest.MockMetadataStore)) (*httptest.Server, *storetest.MockMetadataStore) {
	t.Helper()

	backend := backendtest.NewInMemory()
	mockStore := storetest.NewMockMetadataStore(gomock.NewController(t))
	for _, opt := range opts {
		opt(mockStore)
	}
	storetest.Permissive(mockStore)

	st := proxytest.New(t, mockStore, &proxytest.StackOptions{
		Runtime: proxytest.NewRuntime(&proxytest.RuntimeOptions{
			Backends:        map[string]s3be.ObjectBackend{"b1": backend},
			Order:           []string{"b1"},
			RoutingStrategy: config.RoutingPack,
			Metrics:         mockStore,
		}),
	})
	_ = proxytest.BuildWorkers(st, mockStore)

	srv := &Server{Objects: st.Objects,
		Multipart: st.Multipart, MaxObjectSize: 10 * 1024 * 1024}
	buckets := []config.BucketConfig{{
		Name:                "mybucket",
		MaxMultipartUploads: maxUploads,
		Credentials:         []config.CredentialConfig{{Token: "test-token"}},
	}}
	srv.SetBucketAuth(mustBucketRegistry(t, buckets))

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, mockStore
}

// TestCreateMultipartUpload_PerBucketLimit_Exceeded pins the 503 response
// when the active-upload count is at or above the per-bucket cap.
func TestCreateMultipartUpload_PerBucketLimit_Exceeded(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServerWithMultipartLimit(t, 2, func(m *storetest.MockMetadataStore) {
		m.EXPECT().CountActiveMultipartUploads(gomock.Any(), gomock.Any()).
			Return(int64(2), nil).AnyTimes() // already at limit
	})

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
	ts, _ := newTestServerWithMultipartLimit(t, 1, func(m *storetest.MockMetadataStore) {
		m.EXPECT().CountActiveMultipartUploads(gomock.Any(), gomock.Any()).
			Return(int64(0), errors.New("count failed")).AnyTimes()
	})

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
	ts, _ := newTestServerWithMultipartLimit(t, 5, func(m *storetest.MockMetadataStore) {
		m.EXPECT().CountActiveMultipartUploads(gomock.Any(), gomock.Any()).
			Return(int64(1), nil).AnyTimes()
		m.EXPECT().CreateMultipartUpload(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		m.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
			Return(&core.MultipartUpload{
				UploadID:    "upl-1",
				ObjectKey:   "mybucket/k",
				BackendName: "b1",
				ContentType: "application/octet-stream",
			}, nil).AnyTimes()
	})

	resp := doReq(t, ts, http.MethodPost, ts.URL+"/mybucket/k?uploads", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200. body: %s", resp.StatusCode, body)
	}
}

// -------------------------------------------------------------------------
// UploadPartCopy
// -------------------------------------------------------------------------

// The object every UploadPartCopy test copies from, how a client names it on
// the wire, the request line that copies it into part 1, and the internal key
// that part lands on.
const (
	copySourceKey    = "mybucket/source-key"
	copySourceHeader = "/mybucket/source-key"
	copyPartTarget   = "/mybucket/dest-key?uploadId=upload-1&partNumber=1"
	copyPartKey      = "__multipart/upload-1/1"
)

// newCopyPartServer builds a server whose bucket already holds the source
// object and an open upload, which is the state every UploadPartCopy test
// starts from.
func newCopyPartServer(t *testing.T, sourceData string) (*httptest.Server, *backendtest.InMemory) {
	t.Helper()
	ts, _, backend := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return([]core.ObjectLocation{
				{ObjectKey: copySourceKey, BackendName: "b1", SizeBytes: int64(len(sourceData))},
			}, nil).AnyTimes()
		m.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
			Return(&core.MultipartUpload{
				UploadID:    "upload-1",
				ObjectKey:   "mybucket/dest-key",
				BackendName: "b1",
				ContentType: "text/plain",
			}, nil).AnyTimes()
	})
	backend.Objects[copySourceKey] = backendtest.Object{
		Data: []byte(sourceData), ContentType: "text/plain", ETag: `"src"`,
	}
	return ts, backend
}

// uploadPartCopy issues one UploadPartCopy. An empty copyRange leaves the
// header off, which is how a whole-object part copy is expressed.
func uploadPartCopy(t *testing.T, ts *httptest.Server, source, copyRange string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+copyPartTarget, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("X-Amz-Copy-Source", source)
	if copyRange != "" {
		req.Header.Set("x-amz-copy-source-range", copyRange)
	}
	req.ContentLength = 0
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestUploadPartCopy_StoresTheSourceBytes is the regression this handler
// exists for: the request carries no body, so routing it to UploadPart stored
// an empty part and reported success, and the assembled object came out short.
func TestUploadPartCopy_StoresTheSourceBytes(t *testing.T) {
	t.Parallel()
	const sourceData = "the source object bytes"
	ts, backend := newCopyPartServer(t, sourceData)

	resp := uploadPartCopy(t, ts, copySourceHeader, "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200. body: %s", resp.StatusCode, body)
	}

	var result copyPartResult
	body, _ := io.ReadAll(resp.Body)
	if err := xml.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to decode CopyPartResult: %v. body: %s", err, body)
	}
	if result.ETag == "" {
		t.Error("CopyPartResult carries no ETag")
	}

	part, ok := backend.Get(copyPartKey)
	if !ok {
		t.Fatal("no part was stored")
	}
	if string(part.Data) != sourceData {
		t.Errorf("part = %q, want %q", string(part.Data), sourceData)
	}
}

// TestUploadPartCopy_HonoursCopySourceRange pins the ranged form, which is
// what a client uses to split one large source across several parts.
func TestUploadPartCopy_HonoursCopySourceRange(t *testing.T) {
	t.Parallel()
	const sourceData = "0123456789"
	ts, backend := newCopyPartServer(t, sourceData)

	resp := uploadPartCopy(t, ts, copySourceHeader, "bytes=2-5")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200. body: %s", resp.StatusCode, body)
	}

	part, ok := backend.Get(copyPartKey)
	if !ok {
		t.Fatal("no part was stored")
	}
	if string(part.Data) != "2345" {
		t.Errorf("part = %q, want %q", string(part.Data), "2345")
	}
}

// TestUploadPartCopy_RejectsUnusableRanges asserts a range this server cannot
// read and one the source cannot satisfy are answered apart, and that neither
// stores a part.
func TestUploadPartCopy_RejectsUnusableRanges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		copyRange  string
		wantStatus int
	}{
		{"malformed", "bytes=abc", http.StatusBadRequest},
		{"missing unit", "2-5", http.StatusBadRequest},
		{"reversed", "bytes=5-2", http.StatusBadRequest},
		{"past the end", "bytes=0-99", http.StatusRequestedRangeNotSatisfiable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts, backend := newCopyPartServer(t, "0123456789")

			resp := uploadPartCopy(t, ts, copySourceHeader, tc.copyRange)
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if _, ok := backend.Get(copyPartKey); ok {
				t.Error("a part was stored for a refused range")
			}
		})
	}
}

// TestUploadPartCopy_CrossBucketDenied asserts the copy form is held to the
// same bucket scope as CopyObject: a credential authorizes one bucket.
func TestUploadPartCopy_CrossBucketDenied(t *testing.T) {
	t.Parallel()
	ts, _ := newCopyPartServer(t, "bytes")

	resp := uploadPartCopy(t, ts, "/otherbucket/source-key", "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// TestUploadPartCopy_SourceNotFound asserts a missing source is a 404 rather
// than an empty part.
func TestUploadPartCopy_SourceNotFound(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
			Return(nil, core.ErrObjectNotFound).AnyTimes()
	})

	resp := uploadPartCopy(t, ts, copySourceHeader, "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestUploadPartCopy_RejectsUnusableRequests covers the refusals that precede
// the copy: a part number outside the multipart range, and the two copy-source
// headers that name nothing this server can read.
func TestUploadPartCopy_RejectsUnusableRequests(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		target     string
		source     string
		wantStatus int
	}{
		{"part number zero", "/mybucket/dest-key?uploadId=upload-1&partNumber=0", copySourceHeader, http.StatusBadRequest},
		{"part number absent", "/mybucket/dest-key?uploadId=upload-1", copySourceHeader, http.StatusBadRequest},
		{"undecodable source", copyPartTarget, "/mybucket/bad%zz", http.StatusBadRequest},
		{"source names no key", copyPartTarget, "/mybucket", http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts, backend := newCopyPartServer(t, "0123456789")

			req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+tc.target, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Proxy-Token", "test-token")
			req.Header.Set("X-Amz-Copy-Source", tc.source)
			req.ContentLength = 0
			resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if _, ok := backend.Get(copyPartKey); ok {
				t.Error("a part was stored for a refused request")
			}
		})
	}
}

// TestUploadPartCopy_SourceReadFails asserts a source that heads but will not
// read is an error rather than a short part, which is the failure mode that
// made this operation worth implementing.
func TestUploadPartCopy_SourceReadFails(t *testing.T) {
	t.Parallel()
	ts, backend := newCopyPartServer(t, "0123456789")
	backend.SetGetErr(errors.New("backend read failed"))

	resp := uploadPartCopy(t, ts, copySourceHeader, "")
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Errorf("status = 200, want a failure")
	}
	if _, ok := backend.Get(copyPartKey); ok {
		t.Error("a part was stored despite the source read failing")
	}
}

// TestParseCopySourceRange_Classification pins the parser directly, so the
// closed-form-only rule fails here rather than only through a handler.
func TestParseCopySourceRange_Classification(t *testing.T) {
	t.Parallel()
	const sourceSize = 10
	cases := []struct {
		spec     string
		wantHdr  string
		wantSize int64
		wantErr  error
	}{
		{"", "", sourceSize, nil},
		{"bytes=0-9", "bytes=0-9", 10, nil},
		{"bytes=2-5", "bytes=2-5", 4, nil},
		{"bytes=7-7", "bytes=7-7", 1, nil},
		{"bytes=0-10", "", 0, errCopyRangeUnsatisfiable},
		{"bytes=10-12", "", 0, errCopyRangeUnsatisfiable},
		{"bytes=-5", "", 0, errCopyRangeMalformed},
		{"bytes=5-", "", 0, errCopyRangeMalformed},
		{"bytes=5", "", 0, errCopyRangeMalformed},
		{"0-5", "", 0, errCopyRangeMalformed},
		{"bytes=1-2,4-5", "", 0, errCopyRangeMalformed},
	}

	for _, tc := range cases {
		hdr, size, err := parseCopySourceRange(tc.spec, sourceSize)
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("parseCopySourceRange(%q) err = %v, want %v", tc.spec, err, tc.wantErr)
			continue
		}
		if hdr != tc.wantHdr || size != tc.wantSize {
			t.Errorf("parseCopySourceRange(%q) = (%q, %d), want (%q, %d)",
				tc.spec, hdr, size, tc.wantHdr, tc.wantSize)
		}
	}
}
