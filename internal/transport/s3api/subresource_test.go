// -------------------------------------------------------------------------------
// Subresource Guard Tests
//
// Author: Alex Freidah
//
// S3 selects the operation from the query string, not the method alone:
// PUT /bucket/key uploads an object, PUT /bucket/key?tagging sets tags on it.
// The router reads the method, so an unrecognised subresource used to fall
// through to PutObject or DeleteObject and overwrite or remove the object the
// caller was asking about, returning success. At the bucket level the same
// gap answered every unrecognised GET with a ListBucketResult, which a client
// asking for versions or a policy reads as "there are none".
//
// These tests walk the S3 subresources this server does not implement, at both
// levels, and assert none of them reach the data path or a listing, plus that
// the query keys real clients do send - multipart, listing parameters,
// presigned credentials, response overrides - are still routed.
// -------------------------------------------------------------------------------

package s3api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"

	"go.uber.org/mock/gomock"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// unimplementedSubresources are the object-level query subresources S3 defines
// that this server does not serve. Each one shares a method and a path with a
// data-path operation, which is what made them destructive.
var unimplementedSubresources = []string{
	"acl",
	"retention",
	"legal-hold",
	"object-lock",
	"restore",
	"torrent",
	"select",
	"attributes",
	"versionId",
	"versions",
	"policy",
}

// unimplementedBucketSubresources are the bucket-level query subresources S3
// defines that this server does not serve. Each one shares its method and path
// with a listing, which is what made a wrong answer look like a valid one.
var unimplementedBucketSubresources = []string{
	"versions",
	"acl",
	"policy",
	"cors",
	"lifecycle",
	"notification",
	"object-lock",
	"replication",
	"tagging",
	"website",
	"logging",
	"encryption",
	"accelerate",
	"requestPayment",
	"publicAccessBlock",
	"ownershipControls",
	"intelligent-tiering",
	"inventory",
	"metrics",
	"analytics",
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// statusOf issues one request and returns its status, closing the body. The
// response never escapes, which keeps the close adjacent to it.
func statusOf(t *testing.T, ts *httptest.Server, method, url string, body io.Reader) int {
	t.Helper()
	resp := doReq(t, ts, method, url, body)
	defer resp.Body.Close()
	return resp.StatusCode
}

// assertSubresourceRefused drives one subresource through the two verbs that
// used to be destructive and asserts the object came through untouched.
func assertSubresourceRefused(t *testing.T, sub string) {
	t.Helper()
	ts, _, backend := newTestServer(t)

	const key = "mybucket/doc.txt"
	const original = "the real object bytes"
	if got := statusOf(t, ts, http.MethodPut, ts.URL+"/"+key,
		strings.NewReader(original)); got != http.StatusOK {
		t.Fatalf("seed put status = %d, want 200", got)
	}

	// The shape that used to overwrite the object with its own body.
	if got := statusOf(t, ts, http.MethodPut, ts.URL+"/"+key+"?"+sub,
		strings.NewReader("<Tagging><TagSet/></Tagging>")); got != http.StatusNotImplemented {
		t.Errorf("PUT ?%s status = %d, want 501", sub, got)
	}

	// The shape that used to delete the object outright.
	if got := statusOf(t, ts, http.MethodDelete,
		ts.URL+"/"+key+"?"+sub, nil); got != http.StatusNotImplemented {
		t.Errorf("DELETE ?%s status = %d, want 501", sub, got)
	}

	// Whatever the status codes said, the object is what matters: it must
	// still be exactly what was written.
	obj, ok := backend.Get(key)
	if !ok {
		t.Fatalf("object was removed by ?%s", sub)
	}
	if string(obj.Data) != original {
		t.Errorf("object was overwritten by ?%s: %q", sub, string(obj.Data))
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestObjectSubresource_NeverReachesTheDataPath is the regression this guard
// exists for. A PUT carrying an unimplemented subresource must not store its
// body as the object, and a DELETE carrying one must not remove the object.
func TestObjectSubresource_NeverReachesTheDataPath(t *testing.T) {
	t.Parallel()
	for _, sub := range unimplementedSubresources {
		t.Run(sub, func(t *testing.T) {
			t.Parallel()
			assertSubresourceRefused(t, sub)
		})
	}
}

// TestObjectSubresource_GetDoesNotServeTheObject asserts a GET for a document
// this server does not produce is refused rather than answered with the
// object's bytes, which a client would then try to parse as XML.
func TestObjectSubresource_GetDoesNotServeTheObject(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t)

	const key = "mybucket/doc.txt"
	_ = statusOf(t, ts, http.MethodPut, ts.URL+"/"+key, strings.NewReader("object bytes"))

	get := doReq(t, ts, http.MethodGet, ts.URL+"/"+key+"?acl", nil)
	defer get.Body.Close()
	if get.StatusCode != http.StatusNotImplemented {
		t.Fatalf("GET ?acl status = %d, want 501", get.StatusCode)
	}
	body, _ := io.ReadAll(get.Body)
	if strings.Contains(string(body), "object bytes") {
		t.Error("GET ?acl served the object's contents")
	}
}

// TestObjectSubresource_AllowsWhatClientsActuallySend guards the other
// direction: the allow-list must not refuse the query keys real requests
// carry. A presigned URL puts its credentials in the query string, and
// response-content-disposition rides on most presigned download links.
func TestObjectSubresource_AllowsWhatClientsActuallySend(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		query string
	}{
		{"presigned credentials", "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Expires=900"},
		{"security token", "X-Amz-Security-Token=abc123"},
		{"response override", "response-content-disposition=attachment%3B%20filename%3Da.txt"},
		{"response content type", "response-content-type=text%2Fplain"},
		{"no query at all", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts, _, _ := newTestServer(t)

			const key = "mybucket/doc.txt"
			_ = statusOf(t, ts, http.MethodPut, ts.URL+"/"+key, strings.NewReader("bytes"))

			url := ts.URL + "/" + key
			if tc.query != "" {
				url += "?" + tc.query
			}
			if statusOf(t, ts, http.MethodGet, url, nil) == http.StatusNotImplemented {
				t.Errorf("GET with %s was refused; the allow-list is too narrow", tc.query)
			}
		})
	}
}

// TestUnsupportedObjectQuery_Classification pins the allow-list directly, so a
// future change to it fails here rather than only in a routing test.
func TestUnsupportedObjectQuery_Classification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query       string
		wantRefused bool
	}{
		{"", false},
		{"uploads=", false},
		{"uploadId=abc", false},
		{"partNumber=1", false},
		{"uploadId=abc&partNumber=1", false},
		{"X-Amz-Algorithm=AWS4-HMAC-SHA256", false},
		{"X-Amz-Signature=deadbeef", false},
		{"response-content-disposition=attachment", false},
		// The AWS SDKs append x-id to ordinary data-path calls. Refusing it
		// rejects every SDK write; the integration suite caught this.
		{"x-id=PutObject", false},
		{"x-id=GetObject", false},
		// tagging is implemented, so it belongs on the allowed side; the
		// subresources below it are still refused.
		{"tagging=", false},
		{"acl=", true},
		{"versionId=null", true},
		{"uploadId=abc&acl=", true}, // a known key does not excuse an unknown one
	}

	for _, tc := range cases {
		q, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatalf("ParseQuery(%q): %v", tc.query, err)
		}
		key, refused := unsupportedQuery(q, supportedObjectQueryKeys, supportedObjectQueryPrefixes)
		if refused != tc.wantRefused {
			t.Errorf("query %q refused = %v (key %q), want %v", tc.query, refused, key, tc.wantRefused)
		}
	}
}

// TestBucketSubresource_NeverAnswersAListing is the regression the bucket
// guard exists for. A GET for a document this server does not produce must be
// refused, not answered with a ListBucketResult the caller then reads as an
// empty version list, an absent policy or no lifecycle rules.
func TestBucketSubresource_NeverAnswersAListing(t *testing.T) {
	t.Parallel()
	for _, sub := range unimplementedBucketSubresources {
		t.Run(sub, func(t *testing.T) {
			t.Parallel()
			ts, _, _ := newTestServer(t)

			resp := doReq(t, ts, http.MethodGet, ts.URL+"/mybucket?"+sub, nil)
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusNotImplemented {
				t.Errorf("GET ?%s status = %d, want 501", sub, resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if strings.Contains(string(body), "ListBucketResult") {
				t.Errorf("GET ?%s was answered with a listing", sub)
			}
		})
	}
}

// TestBucketSubresource_AllowsWhatClientsActuallySend guards the other
// direction: the allow-list must pass the parameters an ordinary listing
// carries, the paging keys ListMultipartUploads reads, and the presigned
// credentials a signed listing puts in the query string.
func TestBucketSubresource_AllowsWhatClientsActuallySend(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		query string
	}{
		{"bare listing", ""},
		{"v1 paging", "prefix=logs%2F&marker=logs%2Fa&max-keys=10"},
		{"v2 paging", "list-type=2&continuation-token=tok&start-after=logs%2Fa&fetch-owner=true"},
		{"delimiter listing", "delimiter=%2F&encoding-type=url"},
		{"multipart uploads", "uploads=&key-marker=k&upload-id-marker=u&max-uploads=100"},
		{"presigned credentials", "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Expires=900"},
		{"sdk operation name", "x-id=ListObjectsV2"},
		{"bucket location", "location="},
		{"bucket versioning", "versioning="},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The listings have to reach the store for the guard to be what
			// the status reports, so both listing shapes answer empty.
			ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
				m.EXPECT().ListObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
					Return(&core.ListObjectsResult{}, nil).AnyTimes()
				m.EXPECT().ListObjectsDelimited(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
					Return(&core.ListDelimitedResult{}, nil).AnyTimes()
			})

			url := ts.URL + "/mybucket"
			if tc.query != "" {
				url += "?" + tc.query
			}
			if statusOf(t, ts, http.MethodGet, url, nil) == http.StatusNotImplemented {
				t.Errorf("GET with %s was refused; the allow-list is too narrow", tc.query)
			}
		})
	}
}

// TestUnsupportedBucketQuery_Classification pins the bucket allow-list
// directly, so a future change to it fails here rather than only in a routing
// test.
func TestUnsupportedBucketQuery_Classification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query       string
		wantRefused bool
	}{
		{"", false},
		{"delete=", false},
		{"location=", false},
		{"uploads=", false},
		{"versioning=", false},
		{"list-type=2", false},
		{"prefix=a&delimiter=%2F&max-keys=5", false},
		{"continuation-token=tok&start-after=a&fetch-owner=true", false},
		{"key-marker=k&upload-id-marker=u&max-uploads=7", false},
		{"encoding-type=url", false},
		{"X-Amz-Signature=deadbeef", false},
		{"x-id=ListObjectsV2", false},
		{"acl=", true},
		{"versions=", true},
		{"lifecycle=", true},
		{"cors=", true},
		// The object path's response overrides have no bucket-level meaning.
		{"response-content-type=text%2Fplain", true},
		{"prefix=a&policy=", true}, // a known key does not excuse an unknown one
	}

	for _, tc := range cases {
		q, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatalf("ParseQuery(%q): %v", tc.query, err)
		}
		key, refused := unsupportedQuery(q, supportedBucketQueryKeys, supportedBucketQueryPrefixes)
		if refused != tc.wantRefused {
			t.Errorf("query %q refused = %v (key %q), want %v", tc.query, refused, key, tc.wantRefused)
		}
	}
}
