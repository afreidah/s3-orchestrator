// -------------------------------------------------------------------------------
// TUI - Admin API Client Tests
//
// Author: Alex Freidah
//
// Exercises apiClient.ListObjects against an httptest server: the request shape
// (path, token, query), a decoded success response, and the transport/decoding
// error paths.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAPIClient_ListObjects_Success asserts the request shape and decoded page.
func TestAPIClient_ListObjects_Success(t *testing.T) {
	t.Parallel()
	var gotPath, gotToken, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotToken, gotQuery = r.URL.Path, r.Header.Get("X-Admin-Token"), r.URL.RawQuery
		_, _ = w.Write([]byte(`{"common_prefixes":["photos/"],"objects":[{"key":"a","size":7}],"truncated":true,"next":"a"}`))
	}))
	defer srv.Close()

	page, err := newAPIClient(srv.URL, "tok").ListObjects(context.Background(), "p/", "cont")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if gotPath != "/admin/api/objects" {
		t.Errorf("path = %q, want /admin/api/objects", gotPath)
	}
	if gotToken != "tok" {
		t.Errorf("token = %q, want tok", gotToken)
	}
	for _, want := range []string{"prefix=p%2F", "delimiter=%2F", "continuation=cont"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
	if len(page.CommonPrefixes) != 1 || page.CommonPrefixes[0] != "photos/" {
		t.Errorf("common_prefixes = %v", page.CommonPrefixes)
	}
	if len(page.Objects) != 1 || page.Objects[0].Key != "a" || page.Objects[0].Size != 7 {
		t.Errorf("objects = %+v", page.Objects)
	}
	if !page.Truncated || page.Next != "a" {
		t.Errorf("truncated = %v, next = %q", page.Truncated, page.Next)
	}
}

// TestAPIClient_ListObjects_OmitsEmptyContinuation asserts no continuation param
// is sent when the token is empty.
func TestAPIClient_ListObjects_OmitsEmptyContinuation(t *testing.T) {
	t.Parallel()
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, err := newAPIClient(srv.URL, "tok").ListObjects(context.Background(), "", ""); err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if strings.Contains(gotQuery, "continuation=") {
		t.Errorf("query %q should omit continuation", gotQuery)
	}
}

// TestAPIClient_GetObjectLocations_Success asserts the request shape and the
// decoded location ledger.
func TestAPIClient_GetObjectLocations_Success(t *testing.T) {
	t.Parallel()
	var gotPath, gotToken, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotToken, gotQuery = r.URL.Path, r.Header.Get("X-Admin-Token"), r.URL.RawQuery
		_, _ = w.Write([]byte(`{"key":"photos/a","locations":[{"backend":"b1","size_bytes":9,"encrypted":true,"key_id":"kid"}]}`))
	}))
	defer srv.Close()

	resp, err := newAPIClient(srv.URL, "tok").GetObjectLocations(context.Background(), "photos/a")
	if err != nil {
		t.Fatalf("GetObjectLocations: %v", err)
	}
	if gotPath != "/admin/api/object-locations" {
		t.Errorf("path = %q, want /admin/api/object-locations", gotPath)
	}
	if gotToken != "tok" {
		t.Errorf("token = %q, want tok", gotToken)
	}
	if !strings.Contains(gotQuery, "key=photos%2Fa") {
		t.Errorf("query %q missing key", gotQuery)
	}
	if resp.Key != "photos/a" || len(resp.Locations) != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	if l := resp.Locations[0]; l.Backend != "b1" || l.SizeBytes != 9 || !l.Encrypted || l.KeyID != "kid" {
		t.Errorf("location = %+v", resp.Locations[0])
	}
}

// TestAPIClient_GetReplicationStatus_Success asserts the request shape and the
// decoded replication snapshot.
func TestAPIClient_GetReplicationStatus_Success(t *testing.T) {
	t.Parallel()
	var gotPath, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotToken = r.URL.Path, r.Header.Get("X-Admin-Token")
		_, _ = w.Write([]byte(`{"factor":2,"under_replicated":143,"over_replicated":12,"computed_at":"2026-07-21T14:03:22Z"}`))
	}))
	defer srv.Close()

	resp, err := newAPIClient(srv.URL, "tok").GetReplicationStatus(context.Background())
	if err != nil {
		t.Fatalf("GetReplicationStatus: %v", err)
	}
	if gotPath != "/admin/api/replication" {
		t.Errorf("path = %q, want /admin/api/replication", gotPath)
	}
	if gotToken != "tok" {
		t.Errorf("token = %q, want tok", gotToken)
	}
	if resp.Factor != 2 || resp.UnderReplicated != 143 || resp.OverReplicated != 12 {
		t.Errorf("resp = %+v", resp)
	}
}

// TestAPIClient_ListObjects_ErrorStatus surfaces a >= 400 response as an error.
func TestAPIClient_ListObjects_ErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("denied"))
	}))
	defer srv.Close()

	_, err := newAPIClient(srv.URL, "tok").ListObjects(context.Background(), "", "")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want to mention 403", err)
	}
}

// TestAPIClient_ListObjects_BadJSON surfaces a decode failure as an error.
func TestAPIClient_ListObjects_BadJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	if _, err := newAPIClient(srv.URL, "tok").ListObjects(context.Background(), "", ""); err == nil {
		t.Error("expected a decode error")
	}
}

// TestAPIClient_WriteActions asserts the instance-action writes POST to the
// right path with the token.
func TestAPIClient_WriteActions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, wantPath string
		call           func(*apiClient) error
	}{
		{"reconcile-usage", "/admin/api/usage-reconcile", func(c *apiClient) error { return c.ReconcileUsage(context.Background()) }},
		{"cache-flush", "/admin/api/cache/flush", func(c *apiClient) error { return c.FlushCache(context.Background()) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotMethod, gotPath, gotToken string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath, gotToken = r.Method, r.URL.Path, r.Header.Get("X-Admin-Token")
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			if err := tc.call(newAPIClient(srv.URL, "tok")); err != nil {
				t.Fatalf("call: %v", err)
			}
			if gotMethod != http.MethodPost {
				t.Errorf("method = %s, want POST", gotMethod)
			}
			if gotPath != tc.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tc.wantPath)
			}
			if gotToken != "tok" {
				t.Errorf("token = %q, want tok", gotToken)
			}
		})
	}
}

// TestAPIClient_DoAdmin_ErrorStatus surfaces a >= 400 write response as an error.
func TestAPIClient_DoAdmin_ErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("denied"))
	}))
	defer srv.Close()

	err := newAPIClient(srv.URL, "tok").ReconcileUsage(context.Background())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want to mention 403", err)
	}
}
