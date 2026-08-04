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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
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

// The two action shapes RunOp branches on: a long-running one that streams, and
// a short one that decodes a summary.
var (
	streamingTestAction = opsAction{method: http.MethodPost, path: "/admin/api/scrub"}
	oneShotTestAction   = opsAction{
		method: http.MethodPost,
		path:   "/admin/api/cache/flush",
		result: decodeOneShot[cacheFlushResult],
	}
)

// TestAPIClient_RunOp_Streaming asserts a streaming action opts into NDJSON and
// yields the server's events in order.
func TestAPIClient_RunOp_Streaming(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAccept = r.Method, r.URL.Path, r.Header.Get("Accept")
		_, _ = w.Write([]byte(`{"event":"start","op":"scrub"}` + "\n" + `{"event":"result","outcome":"ok","message":"12 checked"}` + "\n"))
	}))
	defer srv.Close()

	s, err := newAPIClient(srv.URL, "tok").RunOp(context.Background(), streamingTestAction)
	if err != nil {
		t.Fatalf("RunOp: %v", err)
	}
	defer s.Close()
	if gotMethod != http.MethodPost || gotPath != "/admin/api/scrub" {
		t.Errorf("method=%s path=%q", gotMethod, gotPath)
	}
	if gotAccept != adminstream.ContentType {
		t.Errorf("Accept = %q, want %q", gotAccept, adminstream.ContentType)
	}
	first, _ := s.Next()
	if first.Kind != adminstream.KindStart || first.Op != "scrub" {
		t.Errorf("first event = %+v", first)
	}
	second, _ := s.Next()
	if second.Kind != adminstream.KindResult || second.Message != "12 checked" {
		t.Errorf("second event = %+v", second)
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("end err = %v, want EOF", err)
	}
}

// TestAPIClient_RunOp_OneShot asserts a non-streaming action POSTs without the
// NDJSON opt-in and synthesizes a single terminal result from the JSON summary.
func TestAPIClient_RunOp_OneShot(t *testing.T) {
	t.Parallel()
	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		_, _ = w.Write([]byte(`{"status":"flushed","entries_dropped":12}`))
	}))
	defer srv.Close()

	s, err := newAPIClient(srv.URL, "tok").RunOp(context.Background(), oneShotTestAction)
	if err != nil {
		t.Fatalf("RunOp: %v", err)
	}
	defer s.Close()
	if strings.Contains(gotAccept, adminstream.ContentType) {
		t.Errorf("one-shot should not opt into streaming; Accept=%q", gotAccept)
	}
	e, _ := s.Next()
	if e.Kind != adminstream.KindResult || e.Outcome != adminstream.OutcomeOK || e.Message != "dropped 12 cache entries" {
		t.Errorf("result event = %+v", e)
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("end err = %v, want EOF", err)
	}
}

// TestAPIClient_RunOp_ErrorStatus surfaces a >= 400 response as an error.
func TestAPIClient_RunOp_ErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("denied"))
	}))
	defer srv.Close()

	if _, err := newAPIClient(srv.URL, "tok").RunOp(context.Background(), streamingTestAction); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want to mention 403", err)
	}
}

// TestAPIClient_GetCacheStats_Success asserts the hit/miss counters decode, so
// the pane can compute a rate without scraping Prometheus.
func TestAPIClient_GetCacheStats_Success(t *testing.T) {
	t.Parallel()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"entries":3,"size_bytes":2048,"max_bytes":4096,"hits":30,"misses":10}`))
	}))
	defer srv.Close()

	stats, err := newAPIClient(srv.URL, "tok").GetCacheStats(context.Background())
	if err != nil {
		t.Fatalf("GetCacheStats: %v", err)
	}
	if gotPath != "/admin/api/cache" {
		t.Errorf("path = %q, want /admin/api/cache", gotPath)
	}
	if stats.Entries != 3 || stats.SizeBytes != 2048 || stats.MaxBytes != 4096 || stats.Hits != 30 || stats.Misses != 10 {
		t.Errorf("stats = %+v", stats)
	}
}

// TestAPIClient_RequeueCleanupDLQ asserts the requeue POSTs with the backend
// scope in the query string.
func TestAPIClient_RequeueCleanupDLQ(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		_, _ = w.Write([]byte(`{"backend":"b1","requeued":4}`))
	}))
	defer srv.Close()

	resp, err := newAPIClient(srv.URL, "tok").RequeueCleanupDLQ(context.Background(), "b1")
	if err != nil {
		t.Fatalf("RequeueCleanupDLQ: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/admin/api/cleanup-dlq/requeue" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if gotQuery != "backend=b1" {
		t.Errorf("query = %q, want backend=b1", gotQuery)
	}
	if resp.Backend != "b1" || resp.Requeued != 4 {
		t.Errorf("resp = %+v", resp)
	}
}

// TestAPIClient_RequeueCleanupDLQ_OmitsEmptyBackend asserts an unscoped requeue
// sends no backend parameter rather than an empty one.
func TestAPIClient_RequeueCleanupDLQ_OmitsEmptyBackend(t *testing.T) {
	t.Parallel()
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"requeued":0}`))
	}))
	defer srv.Close()

	if _, err := newAPIClient(srv.URL, "tok").RequeueCleanupDLQ(context.Background(), ""); err != nil {
		t.Fatalf("RequeueCleanupDLQ: %v", err)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty", gotQuery)
	}
}
