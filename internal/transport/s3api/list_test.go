// -------------------------------------------------------------------------------
// List Handler Tests
//
// Author: Alex Freidah
//
// Tests for S3 ListObjectsV1 and ListObjectsV2 handlers. Validates XML response
// formatting, prefix filtering, marker/continuation-token pagination, and
// delimiter-based common prefix grouping.
// -------------------------------------------------------------------------------

package s3api

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestListObjectsV2_Success verifies the list objects v2 success contract.
// Asserts that status = , want 200.
func TestListObjectsV2_Success(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	now := time.Now()

	mockStore.ListObjectsResp = &core.ListObjectsResult{
		Objects: []core.ObjectLocation{
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

// TestListObjectsV2_WithDelimiter verifies the list objects v2 with delimiter contract.
// Asserts that status = , want 200.
func TestListObjectsV2_WithDelimiter(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	now := time.Now()

	// Return objects with a common directory prefix
	mockStore.ListObjectsResp = &core.ListObjectsResult{
		Objects: []core.ObjectLocation{
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

// TestListObjectsV2_Pagination verifies the list objects v2 pagination contract.
// Asserts that status = , want 200.
func TestListObjectsV2_Pagination(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	now := time.Now()

	// Return 3 objects when maxKeys=2. The manager will take the first 2 and
	// set IsTruncated=true with a NextContinuationToken.
	mockStore.ListObjectsResp = &core.ListObjectsResult{
		Objects: []core.ObjectLocation{
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

// TestListObjectsV2_Empty verifies the list objects v2 empty contract.
// Asserts that status = , want 200.
func TestListObjectsV2_Empty(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)

	mockStore.ListObjectsResp = &core.ListObjectsResult{
		Objects: []core.ObjectLocation{},
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

// TestListObjectsV1_Success verifies the list objects v1 success contract.
// Asserts that status = , want 200.
func TestListObjectsV1_Success(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	now := time.Now()

	mockStore.ListObjectsResp = &core.ListObjectsResult{
		Objects: []core.ObjectLocation{
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

// TestListObjectsV1_WithMarker verifies the list objects v1 with marker contract.
// Asserts that status = , want 200.
func TestListObjectsV1_WithMarker(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	now := time.Now()

	mockStore.ListObjectsResp = &core.ListObjectsResult{
		Objects: []core.ObjectLocation{
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

// TestListObjectsV1_StoreError verifies the list objects v1 store error contract.
// Asserts that status = , want 500.
func TestListObjectsV1_StoreError(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	mockStore.ListObjectsErr = &core.S3Error{
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

// TestListObjectsV1_Pagination verifies the list objects v1 pagination contract.
// Asserts that status = , want 200.
func TestListObjectsV1_Pagination(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	now := time.Now()

	// Return 3 objects when maxKeys=2. The manager will take the first 2 and
	// set IsTruncated=true with a NextContinuationToken (mapped to NextMarker).
	mockStore.ListObjectsResp = &core.ListObjectsResult{
		Objects: []core.ObjectLocation{
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

// listContentForAssertion mirrors the subset of <Contents> fields the
// ETag/StorageClass contract test inspects. Lifted out of the test body
// so the subtest helper can refer to a package-level type.
type listContentForAssertion struct {
	Key          string `xml:"Key"`
	ETag         string `xml:"ETag"`
	StorageClass string `xml:"StorageClass"`
}

// fetchListResult issues a list request against the given path and
// decodes the response. Extracted so the contract test body stays a
// flat assertion sequence.
func fetchListResult(t *testing.T, url string) []listContentForAssertion {
	t.Helper()
	resp := doReq(t, http.MethodGet, url, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		XMLName  xml.Name                  `xml:"ListBucketResult"`
		Contents []listContentForAssertion `xml:"Contents"`
	}
	if err := xml.Unmarshal(body, &result); err != nil {
		t.Fatalf("parse xml: %v", err)
	}
	return result.Contents
}

// assertETagAndStorageClass pins the two contract invariants per
// rendered Contents entry. Hoisted so the subtest body collapses to one
// fetch + one assertion call.
func assertETagAndStorageClass(t *testing.T, contents []listContentForAssertion) {
	t.Helper()
	if len(contents) != 2 {
		t.Fatalf("Contents len = %d, want 2", len(contents))
	}
	byKey := map[string]listContentForAssertion{}
	for _, c := range contents {
		byKey[c.Key] = c
	}
	if got := byKey["file1.txt"].ETag; got != `"d41d8cd98f00b204e9800998ecf8427e"` {
		t.Errorf("file1 ETag = %q, want quoted hash", got)
	}
	if got := byKey["file2.txt"].ETag; got != `""` {
		t.Errorf("file2 ETag = %q, want quoted empty string", got)
	}
	for _, c := range contents {
		if c.StorageClass != "STANDARD" {
			t.Errorf("%s StorageClass = %q, want STANDARD", c.Key, c.StorageClass)
		}
	}
}

// TestListObjects_IncludesETagAndStorageClass pins the S3-spec contract
// that every Contents entry carries ETag and StorageClass. aws-sdk-go-v2
// models ETag as *string and dereferences without a nil-check, so a
// missing element crashes clients like aptly mid-list with SIGSEGV.
// Covers both list versions, the populated-ContentHash branch (yields a
// quoted hash), and the empty-ContentHash branch (yields a quoted empty
// string  -  still a valid string, no nil deref).
func TestListObjects_IncludesETagAndStorageClass(t *testing.T) {
	t.Parallel()
	ts, mockStore, _ := newTestServer(t)
	now := time.Now()

	mockStore.ListObjectsResp = &core.ListObjectsResult{
		Objects: []core.ObjectLocation{
			{ObjectKey: "mybucket/file1.txt", BackendName: "b1", SizeBytes: 100, CreatedAt: now, ContentHash: "d41d8cd98f00b204e9800998ecf8427e"},
			{ObjectKey: "mybucket/file2.txt", BackendName: "b1", SizeBytes: 200, CreatedAt: now},
		},
	}

	for _, path := range []string{"/mybucket/?list-type=2", "/mybucket/"} {
		t.Run(path, func(t *testing.T) {
			assertETagAndStorageClass(t, fetchListResult(t, ts.URL+path))
		})
	}
}

// TestListObjectsV1_NoAuth verifies the list objects v1 no auth contract.
// Asserts that expected 403, got.
func TestListObjectsV1_NoAuth(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	getReq, _ := http.NewRequestWithContext(context.Background(), "GET", ts.URL+"/mybucket/", nil)
	resp, err := http.DefaultClient.Do(getReq) //nolint:gosec // G704: test server URL is localhost, not tainted
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}
