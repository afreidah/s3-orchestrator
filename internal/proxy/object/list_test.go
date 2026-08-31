// -------------------------------------------------------------------------------
// ListObjects Tests - Store Routing and Response Mapping
//
// Author: Alex Freidah
//
// Verifies Manager.ListObjects routes to the right store query (the
// delimiter-grouped query when a delimiter is set, the flat prefix listing
// otherwise) and maps the returned page into the S3 response shape. The
// grouping and pagination logic itself lives in and is tested at the store
// layer (sqlite unit tests + the Postgres integration parity test).
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

func (r listTestRuntime) Acct() *accounting.Recorder { return r.acct }

// TestListObjects_DelimiterRoutesToStore verifies a delimiter list calls the
// store's grouped query and maps CommonPrefixes, leaf objects, truncation,
// token, and KeyCount into the response.
func TestListObjects_DelimiterRoutesToStore(t *testing.T) {
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	// Times(1)/Times(0) is the routing assertion: a delimiter list must reach
	// the grouped query and never the flat one.
	store.EXPECT().ListObjectsDelimited(gomock.Any(), "p/", "/", "", 1000).
		Return(&core.ListDelimitedResult{
			CommonPrefixes:        []string{"p/a/", "p/b/"},
			Objects:               []core.ObjectLocation{{ObjectKey: "p/file.txt", BackendName: "b1"}},
			IsTruncated:           true,
			NextContinuationToken: "p/b0",
		}, nil).Times(1)
	store.EXPECT().ListObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	m := newListTestManager(store)

	res, err := m.ListObjects(context.Background(), "p/", "/", "", 1000)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(res.CommonPrefixes) != 2 || len(res.Objects) != 1 {
		t.Errorf("got %d prefixes / %d objects, want 2 / 1", len(res.CommonPrefixes), len(res.Objects))
	}
	if res.KeyCount != 3 {
		t.Errorf("KeyCount = %d, want 3", res.KeyCount)
	}
	if !res.IsTruncated || res.NextContinuationToken != "p/b0" {
		t.Errorf("truncation = %v token = %q, want true / p/b0", res.IsTruncated, res.NextContinuationToken)
	}
}

// TestListObjects_NoDelimiterRoutesToFlatStore verifies a non-delimiter list
// calls the flat store query and maps its objects, truncation, and token.
func TestListObjects_NoDelimiterRoutesToFlatStore(t *testing.T) {
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().ListObjects(gomock.Any(), "p/", "", 1000).
		Return(&core.ListObjectsResult{
			Objects:               []core.ObjectLocation{{ObjectKey: "p/1"}, {ObjectKey: "p/2"}},
			IsTruncated:           true,
			NextContinuationToken: "p/2",
		}, nil).Times(1)
	store.EXPECT().ListObjectsDelimited(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	m := newListTestManager(store)

	res, err := m.ListObjects(context.Background(), "p/", "", "", 1000)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(res.CommonPrefixes) != 0 {
		t.Errorf("CommonPrefixes = %v, want none", res.CommonPrefixes)
	}
	if res.KeyCount != 2 || res.NextContinuationToken != "p/2" || !res.IsTruncated {
		t.Errorf("got KeyCount=%d token=%q trunc=%v, want 2 / p/2 / true", res.KeyCount, res.NextContinuationToken, res.IsTruncated)
	}
}
