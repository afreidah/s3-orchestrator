// -------------------------------------------------------------------------------
// Admin API - Object Listing Handler Tests
//
// Author: Alex Freidah
//
// Covers the /admin/api/objects endpoints: browsing the namespace, streaming
// one object down, storing one, and removing either a single key or a whole
// prefix. The read and write endpoints matter to an operator on a terminal,
// who has no session cookie and so cannot reach the UI API's equivalents.
// -------------------------------------------------------------------------------

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/ops/opstest"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// newObjectsHandler builds a Handler whose object operations read the given
// mock store.
func newObjectsHandler(t *testing.T, mock core.ObjectStore) *Handler {
	t.Helper()
	cb := store.NewDatabaseBreaker(config.CircuitBreakerConfig{FailureThreshold: 3})
	var lv slog.LevelVar
	return &Handler{
		log:       slog.Default().With(logfmt.Component("admin")),
		dbHealthy: cb.IsHealthy,
		objects:   objectsOver(t, mock),
		token:     "test-token",
		logLevel:  &lv,
	}
}

// TestHandleListObjects_Happy maps a delimiter-grouped store page into the DTO.
func TestHandleListObjects_Happy(t *testing.T) {
	t.Parallel()
	mock := storetest.NewMockObjectStore(gomock.NewController(t))
	mock.EXPECT().ListObjectsDelimited(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&core.ListDelimitedResult{
			CommonPrefixes:        []string{"photos/", "docs/"},
			Objects:               []core.ObjectLocation{{ObjectKey: "readme.txt", SizeBytes: 42}},
			IsTruncated:           true,
			NextContinuationToken: "readme.txt",
		}, nil).Times(1)
	mux := http.NewServeMux()
	newObjectsHandler(t, mock).Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/objects?prefix=&delimiter=/", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.ObjectListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.CommonPrefixes) != 2 || resp.CommonPrefixes[0] != "photos/" {
		t.Errorf("common_prefixes = %v", resp.CommonPrefixes)
	}
	if len(resp.Objects) != 1 || resp.Objects[0].Key != "readme.txt" || resp.Objects[0].Size != 42 {
		t.Errorf("objects = %+v", resp.Objects)
	}
	if !resp.Truncated || resp.Next != "readme.txt" {
		t.Errorf("truncated = %v, next = %q", resp.Truncated, resp.Next)
	}
}

// TestHandleListObjects_DelimiterDefaultAndFlat asserts an omitted delimiter
// browses hierarchically while an explicitly empty one lists every key flat,
// which is what a caller counting or sweeping a subtree needs.
func TestHandleListObjects_DelimiterDefaultAndFlat(t *testing.T) {
	t.Parallel()
	mock := storetest.NewMockObjectStore(gomock.NewController(t))
	mock.EXPECT().
		ListObjectsDelimited(gomock.Any(), gomock.Any(), "/", gomock.Any(), gomock.Any()).
		Return(&core.ListDelimitedResult{CommonPrefixes: []string{"photos/"}}, nil).
		Times(1)
	mock.EXPECT().
		ListObjects(gomock.Any(), "bucket/dir/", gomock.Any(), gomock.Any()).
		Return(&core.ListObjectsResult{Objects: []core.ObjectLocation{
			{ObjectKey: "bucket/dir/a"}, {ObjectKey: "bucket/dir/b"},
		}}, nil).
		Times(1)

	mux := http.NewServeMux()
	newObjectsHandler(t, mock).Register(mux)

	// omitted: hierarchical
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/objects?prefix=", ""))
	var grouped adminapi.ObjectListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &grouped); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(grouped.CommonPrefixes) != 1 {
		t.Errorf("omitting the delimiter did not group: %+v", grouped)
	}

	// present but empty: flat
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/objects?prefix=bucket/dir/&delimiter=", ""))
	var flat adminapi.ObjectListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &flat); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(flat.Objects) != 2 || len(flat.CommonPrefixes) != 0 {
		t.Errorf("an empty delimiter did not list flat: %+v", flat)
	}
}

// TestHandleListObjects_StoreError returns 500 when the store fails.
func TestHandleListObjects_StoreError(t *testing.T) {
	t.Parallel()
	mock := storetest.NewMockObjectStore(gomock.NewController(t))
	mock.EXPECT().ListObjectsDelimited(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("boom")).Times(1)
	mux := http.NewServeMux()
	newObjectsHandler(t, mock).Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/objects", ""))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// objectsAPIHandler builds a handler whose object operations move real bytes
// through a mocked object API, for the read and write endpoints.
func objectsAPIHandler(t *testing.T) (*Handler, *opstest.MockObjectAPI, *http.ServeMux) {
	t.Helper()
	api := opstest.NewMockObjectAPI(gomock.NewController(t))
	var lv slog.LevelVar
	h := &Handler{
		log: slog.Default().With(logfmt.Component("admin")),
		objects: ops.NewObjects(ops.ObjectsDeps{
			Objects: api,
			Store:   storetest.NewMockObjectStore(gomock.NewController(t)),
			Config:  ops.NewConfigStore(&config.Config{Buckets: []config.BucketConfig{{Name: "bucket"}}}),
		}),
		token:    "test-token",
		logLevel: &lv,
	}
	mux := http.NewServeMux()
	h.Register(mux)
	return h, api, mux
}

// TestHandleGetObject_StreamsBytes asserts the object reaches the caller as a
// download, with the length and filename an operator needs to save it.
func TestHandleGetObject_StreamsBytes(t *testing.T) {
	t.Parallel()
	_, api, mux := objectsAPIHandler(t)
	payload := []byte("hello world")
	api.EXPECT().GetObject(gomock.Any(), "bucket/dir/file.txt", "").
		Return(&backend.GetObjectResult{
			Body: io.NopCloser(bytes.NewReader(payload)),
			Size: int64(len(payload)),
		}, nil).Times(1)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/objects/bucket/dir/file.txt", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != string(payload) {
		t.Errorf("body = %q, want %q", got, payload)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="file.txt"` {
		t.Errorf("Content-Disposition = %q, want the base filename", cd)
	}
	if cl := w.Header().Get("Content-Length"); cl != "11" {
		t.Errorf("Content-Length = %q, want 11", cl)
	}
}

// TestHandleGetObject_NotFound keeps a missing key from reading as a server
// fault.
func TestHandleGetObject_NotFound(t *testing.T) {
	t.Parallel()
	_, api, mux := objectsAPIHandler(t)
	api.EXPECT().GetObject(gomock.Any(), "bucket/ghost", "").
		Return(nil, core.ErrObjectNotFound).Times(1)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/objects/bucket/ghost", ""))

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleGetObject_RejectsKeyOutsideBucket asserts the virtual-bucket
// requirement holds here as it does on the UI API.
func TestHandleGetObject_RejectsKeyOutsideBucket(t *testing.T) {
	t.Parallel()
	_, _, mux := objectsAPIHandler(t)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/objects/nosuchbucket/file.txt", ""))

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandlePutObject_StoresBody asserts the request body is stored verbatim
// under the path key and the ETag comes back.
func TestHandlePutObject_StoresBody(t *testing.T) {
	t.Parallel()
	_, api, mux := objectsAPIHandler(t)
	var stored []byte
	api.EXPECT().PutObject(gomock.Any(), "bucket/dir/file.txt", gomock.Any(), int64(5), "text/plain", nil).
		DoAndReturn(func(_ context.Context, _ string, body io.Reader, _ int64, _ string, _ map[string]string) (string, error) {
			var err error
			stored, err = io.ReadAll(body)
			return "etag-1", err
		}).Times(1)

	req := doAuth(http.MethodPut, "/admin/api/objects/bucket/dir/file.txt", "hello")
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if string(stored) != "hello" {
		t.Errorf("stored %q, want hello", stored)
	}
	var resp adminapi.ObjectUploadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ETag != "etag-1" {
		t.Errorf("etag = %q, want etag-1", resp.ETag)
	}
}

// TestHandlePutObject_RequiresLength asserts an upload of unknown size is
// refused rather than streamed into a backend that needs the length.
func TestHandlePutObject_RequiresLength(t *testing.T) {
	t.Parallel()
	_, _, mux := objectsAPIHandler(t)

	req := doAuth(http.MethodPut, "/admin/api/objects/bucket/file.txt", "")
	req.ContentLength = -1
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusLengthRequired {
		t.Errorf("status = %d, want 411; body=%s", w.Code, w.Body.String())
	}
}

// TestHandlePutObject_RejectsOversize asserts the same size ceiling the UI API
// enforces applies here.
func TestHandlePutObject_RejectsOversize(t *testing.T) {
	t.Parallel()
	_, _, mux := objectsAPIHandler(t)

	req := doAuth(http.MethodPut, "/admin/api/objects/bucket/file.txt", "")
	req.ContentLength = ops.MaxUploadSize + 1
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleDeleteObject_ReportsOne asserts a single delete reports what it
// removed in the same shape a prefix delete does.
func TestHandleDeleteObject_ReportsOne(t *testing.T) {
	t.Parallel()
	_, api, mux := objectsAPIHandler(t)
	api.EXPECT().DeleteObject(gomock.Any(), "bucket/file.txt").Return(nil).Times(1)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodDelete, "/admin/api/objects/bucket/file.txt", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.ObjectDeleteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", resp.Deleted)
	}
}

// TestHandleDeleteObject_BackendFailureIs500 keeps a delete that reached a
// backend and failed there from reading as a rejected request.
func TestHandleDeleteObject_BackendFailureIs500(t *testing.T) {
	t.Parallel()
	_, api, mux := objectsAPIHandler(t)
	api.EXPECT().DeleteObject(gomock.Any(), "bucket/file.txt").
		Return(errors.New("backend unavailable")).Times(1)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodDelete, "/admin/api/objects/bucket/file.txt", ""))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleDeleteObject_RejectsKeyOutsideBucket asserts the bucket
// requirement holds on the destructive endpoint too.
func TestHandleDeleteObject_RejectsKeyOutsideBucket(t *testing.T) {
	t.Parallel()
	_, _, mux := objectsAPIHandler(t)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodDelete, "/admin/api/objects/nosuchbucket/file.txt", ""))

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandlePutObject_BackendFailureIs500 asserts a failed store is a server
// fault rather than a rejected upload.
func TestHandlePutObject_BackendFailureIs500(t *testing.T) {
	t.Parallel()
	_, api, mux := objectsAPIHandler(t)
	api.EXPECT().PutObject(gomock.Any(), "bucket/file.txt", gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return("", errors.New("backend unavailable")).Times(1)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodPut, "/admin/api/objects/bucket/file.txt", "hello"))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleDeletePrefix_ListingFailureIs500 asserts a prefix delete that
// could not even enumerate the keys reports a fault rather than "deleted 0".
func TestHandleDeletePrefix_ListingFailureIs500(t *testing.T) {
	t.Parallel()
	_, api, mux := objectsAPIHandler(t)
	api.EXPECT().ListObjects(gomock.Any(), "bucket/dir/", "", "", gomock.Any()).
		Return(nil, errors.New("ledger unavailable")).Times(1)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodDelete, "/admin/api/objects?prefix=bucket/dir/", ""))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleDeletePrefix_ReportsCount asserts the count is on the wire, so a
// caller can tell a no-op from a mass removal.
func TestHandleDeletePrefix_ReportsCount(t *testing.T) {
	t.Parallel()
	_, api, mux := objectsAPIHandler(t)
	keys := []string{"bucket/dir/a", "bucket/dir/b"}
	api.EXPECT().ListObjects(gomock.Any(), "bucket/dir/", "", "", gomock.Any()).
		Return(&object.ListObjectsV2Result{Objects: []core.ObjectLocation{
			{ObjectKey: keys[0]}, {ObjectKey: keys[1]},
		}}, nil).Times(1)
	api.EXPECT().DeleteObjects(gomock.Any(), keys).
		Return([]object.DeleteObjectResult{{Key: keys[0]}, {Key: keys[1]}}).Times(1)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodDelete, "/admin/api/objects?prefix=bucket/dir/", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.ObjectDeleteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Deleted != 2 {
		t.Errorf("deleted = %d, want 2", resp.Deleted)
	}
}

// TestHandleDeletePrefix_RequiresPrefix keeps an empty prefix from being read
// as "every object".
func TestHandleDeletePrefix_RequiresPrefix(t *testing.T) {
	t.Parallel()
	_, _, mux := objectsAPIHandler(t)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodDelete, "/admin/api/objects", ""))

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleDeletePrefix_PartialFailureIsReported asserts a prefix left half
// deleted is a failure carrying what it did remove, not a success.
func TestHandleDeletePrefix_PartialFailureIsReported(t *testing.T) {
	t.Parallel()
	_, api, mux := objectsAPIHandler(t)
	keys := []string{"bucket/dir/a", "bucket/dir/b"}
	api.EXPECT().ListObjects(gomock.Any(), "bucket/dir/", "", "", gomock.Any()).
		Return(&object.ListObjectsV2Result{Objects: []core.ObjectLocation{
			{ObjectKey: keys[0]}, {ObjectKey: keys[1]},
		}}, nil).Times(1)
	api.EXPECT().DeleteObjects(gomock.Any(), keys).
		Return([]object.DeleteObjectResult{
			{Key: keys[0]},
			{Key: keys[1], Err: errors.New("backend refused")},
		}).Times(1)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodDelete, "/admin/api/objects?prefix=bucket/dir/", ""))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.ObjectDeleteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Deleted != 1 || resp.Failed != 1 || resp.Total != 2 {
		t.Errorf("resp = %+v, want deleted=1 failed=1 total=2", resp)
	}
}

// TestObjectEndpoints_RequireToken keeps the object bytes behind the same
// token gate as every other admin route.
func TestObjectEndpoints_RequireToken(t *testing.T) {
	t.Parallel()
	_, _, mux := objectsAPIHandler(t)

	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/admin/api/objects/bucket/file.txt"},
		{http.MethodPut, "/admin/api/objects/bucket/file.txt"},
		{http.MethodDelete, "/admin/api/objects/bucket/file.txt"},
		{http.MethodDelete, "/admin/api/objects?prefix=bucket/"},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), tc.method, tc.target, nil)
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d, want 401", tc.method, tc.target, w.Code)
		}
	}
}
