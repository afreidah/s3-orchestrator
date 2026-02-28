// -------------------------------------------------------------------------------
// List Handler Tests
//
// Author: Alex Freidah
//
// Tests for S3 ListObjectsV1 and ListObjectsV2 handlers. Validates XML response
// formatting, prefix filtering, marker/continuation-token pagination, and
// delimiter-based common prefix grouping.
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

func TestListObjectsV2_Success(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	now := time.Now()

	mockStore.ListObjectsResp = &storage.ListObjectsResult{
		Objects: []storage.ObjectLocation{
			{ObjectKey: "mybucket/file1.txt", BackendName: "b1", SizeBytes: 100, CreatedAt: now},
			{ObjectKey: "mybucket/file2.txt", BackendName: "b1", SizeBytes: 200, CreatedAt: now},
		},
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/?list-type=2", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	xmlBody := string(body)

	// Verify XML structure
	if !strings.Contains(xmlBody, "<ListBucketResult") {
		t.Error("response missing ListBucketResult element")
	}

	// Verify keys have bucket prefix stripped
	if strings.Contains(xmlBody, "mybucket/file1.txt") {
		t.Error("keys should have bucket prefix stripped")
	}
	if !strings.Contains(xmlBody, "<Key>file1.txt</Key>") {
		t.Error("expected stripped key file1.txt")
	}
	if !strings.Contains(xmlBody, "<Key>file2.txt</Key>") {
		t.Error("expected stripped key file2.txt")
	}
}

func TestListObjectsV2_WithDelimiter(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	now := time.Now()

	// Return objects with a common directory prefix
	mockStore.ListObjectsResp = &storage.ListObjectsResult{
		Objects: []storage.ObjectLocation{
			{ObjectKey: "mybucket/photos/a.jpg", BackendName: "b1", SizeBytes: 100, CreatedAt: now},
			{ObjectKey: "mybucket/photos/b.jpg", BackendName: "b1", SizeBytes: 200, CreatedAt: now},
			{ObjectKey: "mybucket/readme.txt", BackendName: "b1", SizeBytes: 50, CreatedAt: now},
		},
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/?list-type=2&delimiter=/", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	xmlBody := string(body)

	// Should have common prefix for photos/
	if !strings.Contains(xmlBody, "<Prefix>photos/</Prefix>") {
		t.Error("expected common prefix photos/")
	}
	// Should have readme.txt as content
	if !strings.Contains(xmlBody, "<Key>readme.txt</Key>") {
		t.Error("expected key readme.txt")
	}
}

func TestListObjectsV2_Pagination(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	now := time.Now()

	// Return 3 objects when maxKeys=2. The manager will take the first 2 and
	// set IsTruncated=true with a NextContinuationToken.
	mockStore.ListObjectsResp = &storage.ListObjectsResult{
		Objects: []storage.ObjectLocation{
			{ObjectKey: "mybucket/a.txt", BackendName: "b1", SizeBytes: 10, CreatedAt: now},
			{ObjectKey: "mybucket/b.txt", BackendName: "b1", SizeBytes: 20, CreatedAt: now},
			{ObjectKey: "mybucket/c.txt", BackendName: "b1", SizeBytes: 30, CreatedAt: now},
		},
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/?list-type=2&max-keys=2", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)

	type listResult struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		IsTruncated           bool     `xml:"IsTruncated"`
		NextContinuationToken string   `xml:"NextContinuationToken"`
		KeyCount              int      `xml:"KeyCount"`
	}
	var result listResult
	if err := xml.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse XML: %v", err)
	}
	if !result.IsTruncated {
		t.Error("expected IsTruncated=true")
	}
	if result.KeyCount != 2 {
		t.Errorf("KeyCount = %d, want 2", result.KeyCount)
	}
	// NextContinuationToken should have bucket prefix stripped
	if strings.HasPrefix(result.NextContinuationToken, "mybucket/") {
		t.Error("NextContinuationToken should have bucket prefix stripped")
	}
}

func TestListObjectsV2_Empty(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)

	mockStore.ListObjectsResp = &storage.ListObjectsResult{
		Objects: []storage.ObjectLocation{},
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/?list-type=2", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)

	type listResult struct {
		XMLName     xml.Name `xml:"ListBucketResult"`
		IsTruncated bool     `xml:"IsTruncated"`
		KeyCount    int      `xml:"KeyCount"`
	}
	var result listResult
	if err := xml.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse XML: %v", err)
	}
	if result.IsTruncated {
		t.Error("expected IsTruncated=false for empty result")
	}
	if result.KeyCount != 0 {
		t.Errorf("KeyCount = %d, want 0", result.KeyCount)
	}
}

// -------------------------------------------------------------------------
// ListObjectsV1
// -------------------------------------------------------------------------

func TestListObjectsV1_Success(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	now := time.Now()

	mockStore.ListObjectsResp = &storage.ListObjectsResult{
		Objects: []storage.ObjectLocation{
			{ObjectKey: "mybucket/file1.txt", BackendName: "b1", SizeBytes: 100, CreatedAt: now},
			{ObjectKey: "mybucket/file2.txt", BackendName: "b1", SizeBytes: 200, CreatedAt: now},
		},
	}

	// V1: GET without list-type=2
	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	xmlBody := string(body)

	if !strings.Contains(xmlBody, "<ListBucketResult") {
		t.Error("response missing ListBucketResult element")
	}
	// V1 uses Marker, not ContinuationToken
	if strings.Contains(xmlBody, "<ContinuationToken") {
		t.Error("V1 response should not contain ContinuationToken")
	}
	if !strings.Contains(xmlBody, "<Marker>") {
		t.Error("V1 response should contain Marker element")
	}
	if !strings.Contains(xmlBody, "<Key>file1.txt</Key>") {
		t.Error("expected stripped key file1.txt")
	}
	if !strings.Contains(xmlBody, "<Key>file2.txt</Key>") {
		t.Error("expected stripped key file2.txt")
	}
}

func TestListObjectsV1_WithMarker(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	now := time.Now()

	mockStore.ListObjectsResp = &storage.ListObjectsResult{
		Objects: []storage.ObjectLocation{
			{ObjectKey: "mybucket/c.txt", BackendName: "b1", SizeBytes: 30, CreatedAt: now},
		},
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/?marker=b.txt", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	xmlBody := string(body)

	if !strings.Contains(xmlBody, "<Marker>b.txt</Marker>") {
		t.Errorf("expected Marker=b.txt in response: %s", xmlBody)
	}
	if !strings.Contains(xmlBody, "<Key>c.txt</Key>") {
		t.Error("expected key c.txt")
	}
}

func TestListObjectsV1_StoreError(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	mockStore.ListObjectsErr = &storage.S3Error{
		StatusCode: 500,
		Code:       "InternalError",
		Message:    "db error",
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestListObjectsV1_Pagination(t *testing.T) {
	ts, mockStore, _ := newTestServer(t)
	now := time.Now()

	// Return 3 objects when maxKeys=2. The manager will take the first 2 and
	// set IsTruncated=true with a NextContinuationToken (mapped to NextMarker).
	mockStore.ListObjectsResp = &storage.ListObjectsResult{
		Objects: []storage.ObjectLocation{
			{ObjectKey: "mybucket/a.txt", BackendName: "b1", SizeBytes: 10, CreatedAt: now},
			{ObjectKey: "mybucket/b.txt", BackendName: "b1", SizeBytes: 20, CreatedAt: now},
			{ObjectKey: "mybucket/c.txt", BackendName: "b1", SizeBytes: 30, CreatedAt: now},
		},
	}

	resp := doReq(t, http.MethodGet, ts.URL+"/mybucket/?max-keys=2", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)

	type listResult struct {
		XMLName     xml.Name `xml:"ListBucketResult"`
		IsTruncated bool     `xml:"IsTruncated"`
		NextMarker  string   `xml:"NextMarker"`
		MaxKeys     int      `xml:"MaxKeys"`
	}
	var result listResult
	if err := xml.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse XML: %v", err)
	}
	if !result.IsTruncated {
		t.Error("expected IsTruncated=true")
	}
	// NextMarker should have bucket prefix stripped
	if strings.HasPrefix(result.NextMarker, "mybucket/") {
		t.Error("NextMarker should have bucket prefix stripped")
	}
}

func TestListObjectsV1_NoAuth(t *testing.T) {
	ts, _, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/mybucket/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}
