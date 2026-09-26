// -------------------------------------------------------------------------------
// Multipart Listing Paging Tests
//
// Author: Alex Freidah
//
// Covers the paging parameters of ListParts and ListMultipartUploads: the page
// asked of the store, the paging fields in the response, the 1000 cap, a zero
// page, empty values, and the InvalidArgument answer to a value S3 refuses.
// -------------------------------------------------------------------------------

package s3api

import (
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// uploadForPaging answers the upload lookup every ListParts request makes
// before it lists anything.
func uploadForPaging(m *storetest.MockMetadataStore) {
	m.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
		Return(&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "mybucket/testkey", BackendName: "b1"}, nil).
		AnyTimes()
}

// partsNumbered builds parts with the given numbers.
func partsNumbered(numbers ...int) []core.MultipartPart {
	now := time.Now().UTC()
	parts := make([]core.MultipartPart, len(numbers))
	for i, n := range numbers {
		parts[i] = core.MultipartPart{PartNumber: n, ETag: `"e"`, SizeBytes: 5, CreatedAt: now}
	}
	return parts
}

// listPartsPage issues a ListParts request with the given paging query and
// decodes the response.
func listPartsPage(t *testing.T, query string, store func(*storetest.MockMetadataStore)) listPartsResult {
	t.Helper()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		uploadForPaging(m)
		store(m)
	})
	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/testkey?uploadId=upload-1"+query, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200. body: %s", resp.StatusCode, body)
	}
	var result listPartsResult
	if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return result
}

// partNumbers lists the part numbers a response returned.
func partNumbers(result *listPartsResult) []int {
	out := make([]int, len(result.Parts))
	for i := range result.Parts {
		out[i] = result.Parts[i].PartNumber
	}
	return out
}

// -------------------------------------------------------------------------
// LIST PARTS
// -------------------------------------------------------------------------

// TestListParts_Paging asks the store for the page after the marker plus one
// row, and reports the page the way S3 does.
func TestListParts_Paging(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		query      string
		wantAfter  int
		wantLimit  int
		stored     []core.MultipartPart
		wantParts  []int
		wantMax    int
		wantMarker int
		wantNext   int
		wantTrunc  bool
	}{
		{
			name: "marker and max-parts truncate the page", query: "&part-number-marker=2&max-parts=2",
			wantAfter: 2, wantLimit: 3, stored: partsNumbered(3, 4, 5),
			wantParts: []int{3, 4}, wantMax: 2, wantMarker: 2, wantNext: 4, wantTrunc: true,
		},
		{
			name: "last page is not truncated", query: "&part-number-marker=4&max-parts=2",
			wantAfter: 4, wantLimit: 3, stored: partsNumbered(5),
			wantParts: []int{5}, wantMax: 2, wantMarker: 4, wantNext: 5,
		},
		{
			name: "max-parts above 1000 is capped", query: "&max-parts=5000",
			wantAfter: 0, wantLimit: 1001, stored: partsNumbered(1),
			wantParts: []int{1}, wantMax: 1000, wantNext: 1,
		},
		{
			name: "empty values mean the defaults", query: "&max-parts=&part-number-marker=",
			wantAfter: 0, wantLimit: 1001, stored: partsNumbered(1, 2),
			wantParts: []int{1, 2}, wantMax: 1000, wantNext: 2,
		},
		{
			name: "marker past the last part returns an empty page", query: "&part-number-marker=10",
			wantAfter: 10, wantLimit: 1001,
			wantParts: []int{}, wantMax: 1000, wantMarker: 10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := listPartsPage(t, tt.query, func(m *storetest.MockMetadataStore) {
				m.EXPECT().ListParts(gomock.Any(), "upload-1", tt.wantAfter, tt.wantLimit).Return(tt.stored, nil)
			})
			got := partNumbers(&result)
			if len(got) != len(tt.wantParts) {
				t.Fatalf("parts = %v, want %v", got, tt.wantParts)
			}
			for i := range got {
				if got[i] != tt.wantParts[i] {
					t.Fatalf("parts = %v, want %v", got, tt.wantParts)
				}
			}
			if result.MaxParts != tt.wantMax || result.PartNumberMarker != tt.wantMarker ||
				result.NextPartNumberMarker != tt.wantNext || result.IsTruncated != tt.wantTrunc {
				t.Errorf("MaxParts %d, PartNumberMarker %d, NextPartNumberMarker %d, IsTruncated %v; want %d, %d, %d, %v",
					result.MaxParts, result.PartNumberMarker, result.NextPartNumberMarker, result.IsTruncated,
					tt.wantMax, tt.wantMarker, tt.wantNext, tt.wantTrunc)
			}
		})
	}
}

// TestListParts_ZeroMaxParts returns no parts and reports whether any lie
// past the marker, which one row from the store answers.
func TestListParts_ZeroMaxParts(t *testing.T) {
	t.Parallel()
	result := listPartsPage(t, "&max-parts=0", func(m *storetest.MockMetadataStore) {
		m.EXPECT().ListParts(gomock.Any(), "upload-1", 0, 1).Return(partsNumbered(1), nil)
	})
	if len(result.Parts) != 0 || result.MaxParts != 0 || !result.IsTruncated || result.NextPartNumberMarker != 0 {
		t.Errorf("got %d parts, MaxParts %d, IsTruncated %v, NextPartNumberMarker %d; want 0, 0, true, 0",
			len(result.Parts), result.MaxParts, result.IsTruncated, result.NextPartNumberMarker)
	}
}

// TestListingParams_InvalidArgument refuses a paging value that is not an
// integer between 0 and 2147483647 before the store is asked anything.
func TestListingParams_InvalidArgument(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		path  string
		param string
	}{
		{"max-parts not a number", "/mybucket/testkey?uploadId=upload-1&max-parts=abc", "max-parts"},
		{"max-parts negative", "/mybucket/testkey?uploadId=upload-1&max-parts=-1", "max-parts"},
		{"marker negative", "/mybucket/testkey?uploadId=upload-1&part-number-marker=-5", "part-number-marker"},
		{"marker past 32 bits", "/mybucket/testkey?uploadId=upload-1&part-number-marker=2147483648", "part-number-marker"},
		{"max-uploads not a number", "/mybucket/?uploads&max-uploads=abc", "max-uploads"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts, _, _ := newTestServer(t, uploadForPaging)
			resp := doReq(t, ts, http.MethodGet, ts.URL+tt.path, nil)
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400. body: %s", resp.StatusCode, body)
			}
			want := "Argument " + tt.param + " must be an integer between 0 and 2147483647"
			if !strings.Contains(string(body), "<Code>InvalidArgument</Code>") || !strings.Contains(string(body), want) {
				t.Errorf("body = %s, want InvalidArgument with %q", body, want)
			}
		})
	}
}

// -------------------------------------------------------------------------
// LIST MULTIPART UPLOADS
// -------------------------------------------------------------------------

// TestListMultipartUploads_ZeroMaxUploads returns no uploads and reports
// whether any exist.
func TestListMultipartUploads_ZeroMaxUploads(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().ListMultipartUploads(gomock.Any(), gomock.Any(), 1).
			Return([]core.MultipartUpload{{UploadID: "u1", ObjectKey: "mybucket/a.txt", CreatedAt: time.Now()}}, nil)
	})
	resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket/?uploads&max-uploads=0", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var result xmlListMultipartUploadsResult
	if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Upload) != 0 || result.MaxUploads != 0 || !result.IsTruncated {
		t.Errorf("got %d uploads, MaxUploads %d, IsTruncated %v; want 0, 0, true",
			len(result.Upload), result.MaxUploads, result.IsTruncated)
	}
}
