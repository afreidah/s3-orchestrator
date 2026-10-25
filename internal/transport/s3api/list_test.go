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
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/store/storetest"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestListObjectsV2_Success verifies the list objects v2 success contract.
// Asserts that status = , want 200.
func TestListObjectsV2_Success(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().ListObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(&core.ListObjectsResult{
				Objects: []core.ObjectLocation{
					{ObjectKey: "mybucket/file1.txt", BackendName: "b1", SizeBytes: 100, CreatedAt: now},
					{ObjectKey: "mybucket/file2.txt", BackendName: "b1", SizeBytes: 200, CreatedAt: now},
				},
			}, nil).AnyTimes()
	})
	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/?list-type=2", nil)
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
	now := time.Now()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().ListObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(&core.ListObjectsResult{
				CommonPrefixes: []string{"mybucket/photos/"},
				Objects: []core.ObjectLocation{
					{ObjectKey: "mybucket/readme.txt", BackendName: "b1", SizeBytes: 50, CreatedAt: now},
				},
			}, nil).AnyTimes()
	})
	// The store collapses photos/* into a CommonPrefix and returns readme.txt
	// as a leaf for a delimiter listing.
	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/?list-type=2&delimiter=/", nil)
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
	now := time.Now()
	marker := "mybucket/b.txt"
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().ListObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(&core.ListObjectsResult{
				Objects: []core.ObjectLocation{
					{ObjectKey: "mybucket/a.txt", BackendName: "b1", SizeBytes: 10, CreatedAt: now},
					{ObjectKey: "mybucket/b.txt", BackendName: "b1", SizeBytes: 20, CreatedAt: now},
				},
				IsTruncated:           true,
				NextContinuationToken: marker,
			}, nil).AnyTimes()
	})
	// The store truncates to maxKeys=2 and reports the continuation marker.
	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/?list-type=2&max-keys=2", nil)
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
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().ListObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(&core.ListObjectsResult{
				Objects: []core.ObjectLocation{},
			}, nil).AnyTimes()
	})

	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/?list-type=2", nil)
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
	now := time.Now()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().ListObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(&core.ListObjectsResult{
				Objects: []core.ObjectLocation{
					{ObjectKey: "mybucket/file1.txt", BackendName: "b1", SizeBytes: 100, CreatedAt: now},
					{ObjectKey: "mybucket/file2.txt", BackendName: "b1", SizeBytes: 200, CreatedAt: now},
				},
			}, nil).AnyTimes()
	})
	// V1: GET without list-type=2
	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/", nil)
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
	now := time.Now()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().ListObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(&core.ListObjectsResult{
				Objects: []core.ObjectLocation{
					{ObjectKey: "mybucket/c.txt", BackendName: "b1", SizeBytes: 30, CreatedAt: now},
				},
			}, nil).AnyTimes()
	})
	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/?marker=b.txt", nil)
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
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().ListObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(nil, &core.S3Error{
				StatusCode: 500,
				Code:       "InternalError",
				Message:    "db error",
			}).AnyTimes()
	})

	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

// TestListObjectsV1_Pagination verifies a truncated V1 page reports NextMarker,
// stripped of the bucket prefix, only when the request named a delimiter, as
// S3 does; without one the client resumes from the last Key.
func TestListObjectsV1_Pagination(t *testing.T) {
	t.Parallel()
	now := time.Now()
	marker := "mybucket/b.txt"
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().ListObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(&core.ListObjectsResult{
				Objects: []core.ObjectLocation{
					{ObjectKey: "mybucket/a.txt", BackendName: "b1", SizeBytes: 10, CreatedAt: now},
					{ObjectKey: "mybucket/b.txt", BackendName: "b1", SizeBytes: 20, CreatedAt: now},
				},
				IsTruncated:           true,
				NextContinuationToken: marker,
			}, nil).AnyTimes()
	})

	cases := []struct {
		query, wantNextMarker string
	}{
		{"?max-keys=2", ""},
		{"?max-keys=2&delimiter=/", "b.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			result := fetchListPage(t, ts, ts.URL+"/mybucket/"+tc.query, http.StatusOK)
			if !result.IsTruncated {
				t.Error("expected IsTruncated=true")
			}
			if result.NextMarker != tc.wantNextMarker {
				t.Errorf("NextMarker = %q, want %q", result.NextMarker, tc.wantNextMarker)
			}
		})
	}
}

// listPageFields is the subset of a ListBucketResult, or of the Error a refused
// listing returns, that the paging tests inspect across both list versions.
type listPageFields struct {
	Code                  string   `xml:"Code"`
	MaxKeys               int      `xml:"MaxKeys"`
	KeyCount              int      `xml:"KeyCount"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextMarker            string   `xml:"NextMarker"`
	ContinuationToken     string   `xml:"ContinuationToken"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	StartAfter            string   `xml:"StartAfter"`
	Keys                  []string `xml:"Contents>Key"`
}

// fetchListPage issues a listing request, asserts its status, and decodes the
// paging fields of whichever document came back.
func fetchListPage(t *testing.T, ts *httptest.Server, url string, wantStatus int) listPageFields {
	t.Helper()
	resp := doReq(t, ts, http.MethodGet, url, nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, wantStatus, body)
	}
	var result listPageFields
	if err := xml.Unmarshal(body, &result); err != nil {
		t.Fatalf("parse xml: %v", err)
	}
	return result
}

// listVersions are the query strings that select each ListObjects version.
var listVersions = []string{"/mybucket/?list-type=2&", "/mybucket/?"}

// TestListObjects_InvalidMaxKeysRefused verifies that a max-keys that is not an
// integer between 0 and 2147483647 is refused with InvalidArgument in both list
// versions, rather than silently listing a default-sized page.
func TestListObjects_InvalidMaxKeysRefused(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)
	for _, version := range listVersions {
		for _, value := range []string{"blah", "-1", "2147483648"} {
			t.Run(version+value, func(t *testing.T) {
				result := fetchListPage(t, ts, ts.URL+version+"max-keys="+value, http.StatusBadRequest)
				if result.Code != "InvalidArgument" {
					t.Errorf("Code = %q, want InvalidArgument", result.Code)
				}
			})
		}
	}
}

// TestListObjects_MaxKeysReachesTheStore verifies the page size each max-keys
// asks the store for: the default when absent, the value itself when in range,
// and the 1000 cap when above it, with MaxKeys echoing what was applied.
func TestListObjects_MaxKeysReachesTheStore(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query string
		want  int
	}{
		{"", listingPageLimit},
		{"max-keys=0", 0},
		{"max-keys=7", 7},
		{"max-keys=5000", listingPageLimit},
	}
	for _, version := range listVersions {
		for _, tc := range cases {
			t.Run(version+tc.query, func(t *testing.T) {
				t.Parallel()
				ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
					m.EXPECT().ListObjects(gomock.Any(), "mybucket/", "", "", tc.want).
						Return(&core.ListObjectsResult{}, nil).Times(1)
				})
				result := fetchListPage(t, ts, ts.URL+version+tc.query, http.StatusOK)
				if result.MaxKeys != tc.want {
					t.Errorf("MaxKeys = %d, want %d", result.MaxKeys, tc.want)
				}
			})
		}
	}
}

// TestListObjectsV2_EchoesStartAfterAndToken verifies V2 echoes StartAfter and
// ContinuationToken as sent, resumes from the token when both are present,
// and resumes from start-after alone otherwise.
func TestListObjectsV2_EchoesStartAfterAndToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, query, wantStartAfter, wantToken, wantStoreAfter string
	}{
		{"start-after", "start-after=b.txt", "b.txt", "", "mybucket/b.txt"},
		{"token wins", "start-after=b.txt&continuation-token=d.txt", "b.txt", "d.txt", "mybucket/d.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
				m.EXPECT().ListObjects(gomock.Any(), "mybucket/", "", tc.wantStoreAfter, listingPageLimit).
					Return(&core.ListObjectsResult{}, nil).Times(1)
			})
			result := fetchListPage(t, ts, ts.URL+"/mybucket/?list-type=2&"+tc.query, http.StatusOK)
			if result.StartAfter != tc.wantStartAfter {
				t.Errorf("StartAfter = %q, want %q", result.StartAfter, tc.wantStartAfter)
			}
			if result.ContinuationToken != tc.wantToken {
				t.Errorf("ContinuationToken = %q, want %q", result.ContinuationToken, tc.wantToken)
			}
		})
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
func fetchListResult(t *testing.T, ts *httptest.Server, url string) []listContentForAssertion {
	t.Helper()
	resp := doReq(t, ts, http.MethodGet, url, nil)
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
		t.Errorf("file1 ETag = %q, want the object's recorded ETag", got)
	}
	// file2 carries a content hash and no identity: the hash is the integrity
	// SHA-256 of the stored bytes and must never be published as an ETag,
	// which is what a listing did before objects had one of their own.
	if got := byKey["file2.txt"].ETag; got != `""` {
		t.Errorf("file2 ETag = %q, want quoted empty string rather than the content hash", got)
	}
	for _, c := range contents {
		if c.StorageClass != "STANDARD" {
			t.Errorf("%s StorageClass = %q, want STANDARD", c.Key, c.StorageClass)
		}
	}
}

// TestListObjects_IncludesETagAndStorageClass verifies that every Contents
// entry carries ETag and StorageClass, in both list versions, including a
// quoted empty ETag when no content hash is stored. Some SDK clients crash on
// a missing ETag.
func TestListObjects_IncludesETagAndStorageClass(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().ListObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(&core.ListObjectsResult{
				Objects: []core.ObjectLocation{
					{
						ObjectKey: "mybucket/file1.txt", BackendName: "b1", SizeBytes: 100, CreatedAt: now,
						Identity: &core.ObjectIdentity{ETag: `"d41d8cd98f00b204e9800998ecf8427e"`},
					},
					{
						ObjectKey: "mybucket/file2.txt", BackendName: "b1", SizeBytes: 200, CreatedAt: now,
						ContentHash: "0a9b7c6d5e4f30211122334455667788990011223344556677889900aabbccdd",
					},
				},
			}, nil).AnyTimes()
	})
	for _, path := range []string{"/mybucket/?list-type=2", "/mybucket/"} {
		t.Run(path, func(t *testing.T) {
			assertETagAndStorageClass(t, fetchListResult(t, ts, ts.URL+path))
		})
	}
}

// TestListObjectsV1_NoAuth verifies the list objects v1 no auth contract.
// Asserts that expected 403, got.
func TestListObjectsV1_NoAuth(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	getReq, _ := http.NewRequestWithContext(context.Background(), "GET", ts.URL+"/mybucket/", nil)
	resp, err := ts.Client().Do(getReq) //nolint:gosec // G704: test server URL is localhost, not tainted
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}
