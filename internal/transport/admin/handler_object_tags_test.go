// -------------------------------------------------------------------------------
// Admin API - Object Tag Handler Tests
//
// Author: Alex Freidah
//
// The handlers own the wire shape and the status codes; validation and the
// missing-object refusal come from the store through ops. These cover the
// round trip, that an untagged object serialises as [] rather than null, and
// that each refusal lands on the status a caller can act on.
// -------------------------------------------------------------------------------

package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/ops/opstest"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newObjectsHandlerWithAPI builds a handler over a mocked ObjectAPI and
// registers it, so a test states the one ops call it cares about.
func newObjectsHandlerWithAPI(t *testing.T) (*opstest.MockObjectAPI, http.Handler) {
	t.Helper()
	api := opstest.NewMockObjectAPI(gomock.NewController(t))
	cb := store.NewDatabaseBreaker(config.CircuitBreakerConfig{FailureThreshold: 3})
	var lv slog.LevelVar
	h := &Handler{
		log:       slog.Default().With(logfmt.Component("admin")),
		dbHealthy: cb.IsHealthy,
		objects: ops.NewObjects(ops.ObjectsDeps{
			Objects: api,
			Store:   storetest.NewMockObjectStore(gomock.NewController(t)),
			Config: ops.NewConfigStore(&config.Config{
				Buckets: []config.BucketConfig{{Name: "bucket"}},
			}),
		}),
		token:    "test-token",
		logLevel: &lv,
	}
	mux := http.NewServeMux()
	h.Register(mux)
	return api, mux
}

// tagsRequest issues an authenticated tag request against the admin mux.
func tagsRequest(t *testing.T, mux http.Handler, method, key, body string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, doAuth(method, "/admin/api/objects/tags/"+key, body))
	return w.Code, w.Body.String()
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestAdminGetObjectTags_ReturnsSet verifies a stored set renders as JSON.
func TestAdminGetObjectTags_ReturnsSet(t *testing.T) {
	api, mux := newObjectsHandlerWithAPI(t)
	api.EXPECT().GetObjectTags(gomock.Any(), "bucket/k").
		Return([]core.Tag{{Key: "retain", Value: "30d"}}, nil)

	code, body := tagsRequest(t, mux, http.MethodGet, "bucket/k", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, body)
	}

	var got adminapi.ObjectTagsResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if len(got.Tags) != 1 || got.Tags[0].Key != "retain" || got.Tags[0].Value != "30d" {
		t.Errorf("tags = %+v, want the stored set", got.Tags)
	}
}

// TestAdminGetObjectTags_UntaggedIsEmptyList verifies an object carrying no
// tags serialises as [] rather than null, so a caller can render it without a
// nil check.
func TestAdminGetObjectTags_UntaggedIsEmptyList(t *testing.T) {
	api, mux := newObjectsHandlerWithAPI(t)
	api.EXPECT().GetObjectTags(gomock.Any(), "bucket/k").Return([]core.Tag{}, nil)

	code, body := tagsRequest(t, mux, http.MethodGet, "bucket/k", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, body)
	}
	if !strings.Contains(body, `"tags":[]`) {
		t.Errorf("expected an empty list, got %s", body)
	}
}

// TestAdminGetObjectTags_MissingObject verifies a key holding no copies
// answers 404 rather than an empty set.
func TestAdminGetObjectTags_MissingObject(t *testing.T) {
	api, mux := newObjectsHandlerWithAPI(t)
	api.EXPECT().GetObjectTags(gomock.Any(), "bucket/gone").
		Return(nil, core.ErrObjectNotFound)

	code, body := tagsRequest(t, mux, http.MethodGet, "bucket/gone", "")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", code, body)
	}
}

// TestAdminPutObjectTags_ReplacesSet verifies the decoded set reaches the
// store and is echoed back.
func TestAdminPutObjectTags_ReplacesSet(t *testing.T) {
	api, mux := newObjectsHandlerWithAPI(t)
	api.EXPECT().PutObjectTags(gomock.Any(), "bucket/k",
		[]core.Tag{{Key: "a", Value: "1"}}).Return(nil)

	code, body := tagsRequest(t, mux, http.MethodPut, "bucket/k",
		`{"tags":[{"key":"a","value":"1"}]}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, body)
	}
	if !strings.Contains(body, `"key":"a"`) {
		t.Errorf("expected the set echoed, got %s", body)
	}
}

// TestAdminPutObjectTags_ValidationRefusal verifies a store-side tag-shape
// refusal renders as 400 with the offending measurement, not a 500.
func TestAdminPutObjectTags_ValidationRefusal(t *testing.T) {
	api, mux := newObjectsHandlerWithAPI(t)
	api.EXPECT().PutObjectTags(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(core.ErrTooManyTags)

	code, body := tagsRequest(t, mux, http.MethodPut, "bucket/k",
		`{"tags":[{"key":"a","value":"1"}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", code, body)
	}
}

// TestAdminPutObjectTags_MalformedBody verifies a body that is not a tag set
// is refused before anything reaches the store.
func TestAdminPutObjectTags_MalformedBody(t *testing.T) {
	_, mux := newObjectsHandlerWithAPI(t)

	code, body := tagsRequest(t, mux, http.MethodPut, "bucket/k", "not json")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", code, body)
	}
}

// TestAdminDeleteObjectTags_ClearsSet verifies the delete reports an empty set
// so a caller can render the result without a second read.
func TestAdminDeleteObjectTags_ClearsSet(t *testing.T) {
	api, mux := newObjectsHandlerWithAPI(t)
	api.EXPECT().DeleteObjectTags(gomock.Any(), "bucket/k").Return(nil)

	code, body := tagsRequest(t, mux, http.MethodDelete, "bucket/k", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, body)
	}
	if !strings.Contains(body, `"tags":[]`) {
		t.Errorf("expected an empty list, got %s", body)
	}
}

// TestAdminDeleteObjectTags_StoreFailure verifies an unrecognised failure
// still renders as JSON rather than leaking a Go error to the caller.
func TestAdminDeleteObjectTags_StoreFailure(t *testing.T) {
	api, mux := newObjectsHandlerWithAPI(t)
	api.EXPECT().DeleteObjectTags(gomock.Any(), gomock.Any()).
		Return(errors.New("database on fire"))

	code, _ := tagsRequest(t, mux, http.MethodDelete, "bucket/k", "")
	if code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", code)
	}
}
