// -------------------------------------------------------------------------------
// Admin API - Object Listing Handler Tests
//
// Author: Alex Freidah
//
// Covers the /admin/api/objects browse endpoint: the happy path mapping a
// store page into the shared DTO, and the store-error path returning 500.
// -------------------------------------------------------------------------------

package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// newObjectsHandler builds a Handler backed by the given mock store.
func newObjectsHandler(mock core.ObjectStore) *Handler {
	cb := store.NewDatabaseBreaker(config.CircuitBreakerConfig{FailureThreshold: 3})
	var lv slog.LevelVar
	return &Handler{
		log:       slog.Default().With(logfmt.Component("admin")),
		dbHealthy: cb.IsHealthy,
		objects:   mock,
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
	newObjectsHandler(mock).Register(mux)

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

// TestHandleListObjects_StoreError returns 500 when the store fails.
func TestHandleListObjects_StoreError(t *testing.T) {
	t.Parallel()
	mock := storetest.NewMockObjectStore(gomock.NewController(t))
	mock.EXPECT().ListObjectsDelimited(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, errors.New("boom")).Times(1)
	mux := http.NewServeMux()
	newObjectsHandler(mock).Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(http.MethodGet, "/admin/api/objects", ""))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}
