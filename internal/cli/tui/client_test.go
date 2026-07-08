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
