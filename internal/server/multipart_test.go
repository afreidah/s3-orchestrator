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

package server

import (
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/storage"
)

// -------------------------------------------------------------------------
// CreateMultipartUpload
// -------------------------------------------------------------------------

func TestCreateMultipartUpload_Success(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)

	mockStore.GetMultipartResp = &storage.MultipartUpload{
		UploadID:    "test-upload-id",
		ObjectKey:   "mybucket/testkey",
		BackendName: "b1",
		ContentType: "text/plain",
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mybucket/testkey?uploads", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
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

func TestCreateMultipartUpload_StoreError(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	mockStore.CreateMultipartErr = &storage.S3Error{
		StatusCode: 500,
		Code:       "InternalError",
		Message:    "db error",
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mybucket/testkey?uploads", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestCreateMultipartUpload_DefaultContentType(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)

	mockStore.GetMultipartResp = &storage.MultipartUpload{
		UploadID:    "test-upload-id",
		ObjectKey:   "mybucket/testkey",
		BackendName: "b1",
		ContentType: "application/octet-stream",
	}

	// No Content-Type header — should default to application/octet-stream
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mybucket/testkey?uploads", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// -------------------------------------------------------------------------
// UploadPart
// -------------------------------------------------------------------------

func TestUploadPart_Success(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)

	mockStore.GetMultipartResp = &storage.MultipartUpload{
		UploadID:    "upload-1",
		ObjectKey:   "mybucket/testkey",
		BackendName: "b1",
		ContentType: "application/octet-stream",
	}

	body := strings.NewReader("part-data")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket/testkey?uploadId=upload-1&partNumber=1", body)
	req.Header.Set("X-Proxy-Token", "test-token")
	req.ContentLength = int64(len("part-data"))
	resp, err := http.DefaultClient.Do(req)
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

func TestUploadPart_InvalidPartNumber(t *testing.T) {
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket/testkey?uploadId=upload-1&partNumber=abc", strings.NewReader("data"))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.ContentLength = 4
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestUploadPart_ZeroPartNumber(t *testing.T) {
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket/testkey?uploadId=upload-1&partNumber=0", strings.NewReader("data"))
	req.Header.Set("X-Proxy-Token", "test-token")
	req.ContentLength = 4
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestUploadPart_MissingContentLength(t *testing.T) {
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket/testkey?uploadId=upload-1&partNumber=1", strings.NewReader("data"))
	req.Header.Set("X-Proxy-Token", "test-token")
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

func TestUploadPart_EntityTooLarge(t *testing.T) {
	ts, _, _ := newTestServer(t)

	bigSize := int64(20 * 1024 * 1024)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/mybucket/testkey?uploadId=upload-1&partNumber=1", io.LimitReader(neverEndingReader{}, bigSize))
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

// -------------------------------------------------------------------------
// CompleteMultipartUpload
// -------------------------------------------------------------------------

func TestCompleteMultipartUpload_Success(t *testing.T) {
	ts, mockStore, backend := newTestServer(t)

	// Store has a multipart upload with one part
	mockStore.GetMultipartResp = &storage.MultipartUpload{
		UploadID:    "upload-1",
		ObjectKey:   "mybucket/testkey",
		BackendName: "b1",
		ContentType: "text/plain",
	}
	mockStore.GetPartsResp = []storage.MultipartPart{
		{PartNumber: 1, ETag: `"part1"`, SizeBytes: 4},
	}
	// Pre-store the part object on the backend at the internal part key
	backend.objects["__multipart/upload-1/1"] = serverMockObj{
		data: []byte("data"), contentType: "application/octet-stream", etag: `"part1"`,
	}
	mockStore.GetBackendResp = "b1"

	xmlBody := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"part1"</ETag></Part></CompleteMultipartUpload>`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mybucket/testkey?uploadId=upload-1", strings.NewReader(xmlBody))
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := http.DefaultClient.Do(req)
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

func TestCompleteMultipartUpload_MalformedXML(t *testing.T) {
	ts, _, _ := newTestServer(t)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mybucket/testkey?uploadId=upload-1", strings.NewReader("not xml"))
	req.Header.Set("X-Proxy-Token", "test-token")
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
// AbortMultipartUpload
// -------------------------------------------------------------------------

func TestAbortMultipartUpload_Success(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)

	mockStore.GetMultipartResp = &storage.MultipartUpload{
		UploadID:    "upload-1",
		ObjectKey:   "mybucket/testkey",
		BackendName: "b1",
		ContentType: "text/plain",
	}
	mockStore.GetPartsResp = nil // no parts to clean up

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/mybucket/testkey?uploadId=upload-1", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 204. body: %s", resp.StatusCode, body)
	}
}

func TestAbortMultipartUpload_NotFound(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	mockStore.GetMultipartErr = storage.ErrObjectNotFound

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/mybucket/testkey?uploadId=nonexistent", nil)
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := http.DefaultClient.Do(req)
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

func TestListParts_Success(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)

	now := time.Now().UTC()
	mockStore.GetPartsResp = []storage.MultipartPart{
		{PartNumber: 1, ETag: `"aaa"`, SizeBytes: 100, CreatedAt: now},
		{PartNumber: 2, ETag: `"bbb"`, SizeBytes: 200, CreatedAt: now},
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/testkey?uploadId=upload-1", nil)
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

func TestListParts_StoreError(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	mockStore.GetPartsErr = &storage.S3Error{
		StatusCode: 500,
		Code:       "InternalError",
		Message:    "db error",
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/testkey?uploadId=upload-1", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestListParts_EmptyParts(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	mockStore.GetPartsResp = nil // no parts

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/testkey?uploadId=upload-1", nil)
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

func TestListMultipartUploads_Success(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	now := time.Now().UTC()

	mockStore.ListMultipartUploadsResp = []storage.MultipartUpload{
		{UploadID: "upload-1", ObjectKey: "mybucket/file1.txt", ContentType: "text/plain", CreatedAt: now},
		{UploadID: "upload-2", ObjectKey: "mybucket/file2.txt", ContentType: "text/plain", CreatedAt: now},
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/?uploads", nil)
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

func TestListMultipartUploads_Empty(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	mockStore.ListMultipartUploadsResp = nil

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/?uploads", nil)
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

func TestListMultipartUploads_Truncated(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	now := time.Now().UTC()

	// Return 3 uploads when max-uploads=2; handler fetches maxUploads+1 to
	// detect truncation, so the mock needs to return 3.
	mockStore.ListMultipartUploadsResp = []storage.MultipartUpload{
		{UploadID: "u1", ObjectKey: "mybucket/a.txt", ContentType: "text/plain", CreatedAt: now},
		{UploadID: "u2", ObjectKey: "mybucket/b.txt", ContentType: "text/plain", CreatedAt: now},
		{UploadID: "u3", ObjectKey: "mybucket/c.txt", ContentType: "text/plain", CreatedAt: now},
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/?uploads&max-uploads=2", nil)
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

func TestListMultipartUploads_StoreError(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	mockStore.ListMultipartUploadsErr = &storage.S3Error{
		StatusCode: 500,
		Code:       "InternalError",
		Message:    "db error",
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/?uploads", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestListMultipartUploads_NoAuth(t *testing.T) {
	ts, _, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/mybucket/?uploads")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}
