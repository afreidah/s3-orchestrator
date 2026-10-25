// -------------------------------------------------------------------------------
// ListObjects Tests - Store Pass-Through
//
// Author: Alex Freidah
//
// Verifies Manager.ListObjects hands the store the request as given and
// returns its page. The grouping and pagination logic itself lives in and is
// tested at the store layer (sqlite unit tests + the Postgres integration
// parity test).
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newListTestManager wires a Manager with only what ListObjects touches: the
// store and a no-op accounting recorder.
func newListTestManager(store Stores) *Manager {
	rec := accounting.New(nil, func(_, _ string, _ time.Time, _ error) {})
	return &Manager{stores: store, core: listTestRuntime{acct: rec}, log: slog.Default()}
}

// listTestRuntime is a minimal Runtime: ListObjects only reaches the
// runtime through Acct().Operation.
type listTestRuntime struct {
	Runtime // embedded nil; only Acct is implemented

	acct *accounting.Recorder
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

func (r listTestRuntime) Acct() *accounting.Recorder { return r.acct }

// TestListObjects_PassesThePageThrough verifies the manager hands the store
// the request as given and returns its page untouched, with KeyCount counting
// each common prefix as one entry.
func TestListObjects_PassesThePageThrough(t *testing.T) {
	page := &core.ListObjectsResult{
		CommonPrefixes:        []string{"p/a/", "p/b/"},
		Objects:               []core.ObjectLocation{{ObjectKey: "p/file.txt", BackendName: "b1"}},
		IsTruncated:           true,
		NextContinuationToken: "p/b0",
	}
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().ListObjects(gomock.Any(), "p/", "/", "p/0", 3).Return(page, nil).Times(1)
	m := newListTestManager(store)

	res, err := m.ListObjects(context.Background(), "p/", "/", "p/0", 3)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if res != page {
		t.Errorf("ListObjects = %+v, want the store's page", res)
	}
	if res.KeyCount() != 3 {
		t.Errorf("KeyCount = %d, want 3", res.KeyCount())
	}
}
