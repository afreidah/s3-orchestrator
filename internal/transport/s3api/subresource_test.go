// -------------------------------------------------------------------------------
// Object Subresource Guard Tests
//
// Author: Alex Freidah
//
// S3 selects the operation from the query string, not the method alone:
// PUT /bucket/key uploads an object, PUT /bucket/key?tagging sets tags on it.
// The router reads the method, so an unrecognised subresource used to fall
// through to PutObject or DeleteObject and overwrite or remove the object the
// caller was asking about, returning success.
//
// These tests walk the S3 object subresources this server does not implement
// and assert none of them reach the data path, plus that the query keys real
// clients do send - multipart, presigned credentials, response overrides - are
// still routed.
// -------------------------------------------------------------------------------

package s3api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

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
		key, refused := unsupportedObjectQuery(q)
		if refused != tc.wantRefused {
			t.Errorf("query %q refused = %v (key %q), want %v", tc.query, refused, key, tc.wantRefused)
		}
	}
}
