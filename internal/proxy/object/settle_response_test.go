// -------------------------------------------------------------------------------
// Settling After the Response - Tests
//
// Author: Alex Freidah
//
// A client's delete or overwrite is answered once its transaction commits, and
// the backend deletes run behind the response. Each test runs in a synctest
// bubble, so a slow backend costs fake time and a response that waited for it
// shows as exactly that much elapsed.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// backendDelete is how long the slow backend takes over each delete.
const backendDelete = time.Second

// settledRows counts how the store saw each cleanup row end.
type settledRows struct {
	retired atomic.Int64
	retried atomic.Int64
}

// settledRequests are the client operations that queue a cleanup row and answer
// before its bytes are deleted.
var settledRequests = map[string]func(ctx context.Context, f *fleet){
	"delete": func(ctx context.Context, f *fleet) {
		_ = f.DeleteObject(ctx, "key")
	},
	"batch delete": func(ctx context.Context, f *fleet) {
		f.DeleteObjects(ctx, []string{"key"})
	},
	"overwrite": func(ctx context.Context, f *fleet) {
		req := &core.RecordObjectRequest{Key: "key", Copies: []core.ObjectCopy{{Backend: "b1", IntentID: "intent"}}}
		_ = f.Coord.RecordObjectAndPromoteIntent(ctx, noop.Span{}, s3op.PutObject, req)
	},
}

// newSettleFleet builds a fleet over one slow backend, on a store that hands
// every delete and overwrite one cleanup row and counts how each row ends.
func newSettleFleet(t *testing.T) (*fleet, *settledRows) {
	t.Helper()
	rows := &settledRows{}
	queued := []core.CleanupItem{{ID: 1, BackendName: "b1", ObjectKey: "key", StorageKey: "key"}}
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).Return(queued, nil).AnyTimes()
	store.EXPECT().DeleteObjectsBatch(gomock.Any(), gomock.Any()).Return(queued, nil).AnyTimes()
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any()).Return(queued, nil).AnyTimes()
	store.EXPECT().CompleteCleanupItems(gomock.Any(), gomock.Any()).DoAndReturn(
		func(context.Context, []core.CleanupItem) ([]int64, error) {
			rows.retired.Add(1)
			return []int64{1}, nil
		}).AnyTimes()
	store.EXPECT().RetryCleanupItem(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(context.Context, *core.CleanupItem, time.Duration, string) (bool, error) {
			rows.retried.Add(1)
			return true, nil
		}).AnyTimes()
	storetest.Permissive(store)

	be := backendtest.NewSlow(backendtest.NewInMemory(), backendDelete)
	be.DelayDeletes = true
	return newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, &fleetOpts{MaxDetached: 1}), rows
}

// waitForSettles blocks until the deletes left behind the responses finish.
func waitForSettles(t *testing.T, f *fleet) {
	t.Helper()
	if n := f.Coord.Settling().Wait(t.Context()); n != 0 {
		t.Fatalf("%d settles still running", n)
	}
}

// clientContext is the context a request carries through the S3 handler.
func clientContext() context.Context {
	return audit.WithWorkload(context.Background(), audit.WorkloadClient)
}

// eachSettledRequest runs check in its own bubble for every client operation.
func eachSettledRequest(t *testing.T, check func(t *testing.T, answer func(context.Context, *fleet))) {
	t.Helper()
	for name, answer := range settledRequests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				check(t, answer)
			})
		})
	}
}

// TestSettle_ResponseDoesNotWaitForTheBackend verifies a client is answered
// without waiting for the delete, and the row still retires behind the response.
func TestSettle_ResponseDoesNotWaitForTheBackend(t *testing.T) {
	eachSettledRequest(t, func(t *testing.T, answer func(context.Context, *fleet)) {
		f, rows := newSettleFleet(t)
		start := time.Now()
		answer(clientContext(), f)
		if took := time.Since(start); took != 0 {
			t.Errorf("answered after %s, want no wait on the backend", took)
		}
		waitForSettles(t, f)
		if rows.retired.Load() != 1 {
			t.Errorf("retired %d rows, want 1", rows.retired.Load())
		}
	})
}

// TestSettle_HangUpDoesNotCancelTheDelete verifies a client gone mid-delete
// leaves the delete running, so the row retires instead of using up an attempt.
func TestSettle_HangUpDoesNotCancelTheDelete(t *testing.T) {
	eachSettledRequest(t, func(t *testing.T, answer func(context.Context, *fleet)) {
		f, rows := newSettleFleet(t)
		ctx, hangUp := context.WithTimeout(clientContext(), backendDelete/2)
		defer hangUp()
		answer(ctx, f)
		waitForSettles(t, f)
		if rows.retried.Load() != 0 || rows.retired.Load() != 1 {
			t.Errorf("retried %d and retired %d rows, want 0 and 1", rows.retried.Load(), rows.retired.Load())
		}
	})
}

// TestSettle_FullCeilingSettlesBeforeAnswering verifies a request past the
// ceiling waits for its deletes, so queued rows cannot pile up.
func TestSettle_FullCeilingSettlesBeforeAnswering(t *testing.T) {
	eachSettledRequest(t, func(t *testing.T, answer func(context.Context, *fleet)) {
		f, rows := newSettleFleet(t)
		for range writepath.SettleCeiling {
			release, admitted := f.Coord.Settling().Begin()
			if !admitted {
				t.Fatal("the ceiling refused a slot below its limit")
			}
			t.Cleanup(release)
		}

		start := time.Now()
		answer(clientContext(), f)
		if took := time.Since(start); took != backendDelete {
			t.Errorf("answered after %s, want %s", took, backendDelete)
		}
		if rows.retired.Load() != 1 {
			t.Errorf("retired %d rows, want 1", rows.retired.Load())
		}
	})
}

// TestSettle_StalledWritesDoNotRefuseDeletes verifies a full fan-out ceiling
// leaves client deletes running behind their responses.
func TestSettle_StalledWritesDoNotRefuseDeletes(t *testing.T) {
	eachSettledRequest(t, func(t *testing.T, answer func(context.Context, *fleet)) {
		f, rows := newSettleFleet(t)
		release, admitted := f.Detached.Begin()
		if !admitted {
			t.Fatal("the ceiling refused the slot that fills it")
		}
		defer release()

		start := time.Now()
		answer(clientContext(), f)
		if took := time.Since(start); took != 0 {
			t.Errorf("answered after %s, want no wait on the backend", took)
		}
		waitForSettles(t, f)
		if rows.retired.Load() != 1 {
			t.Errorf("retired %d rows, want 1", rows.retired.Load())
		}
	})
}

// TestSettle_BackgroundWorkWaitsForTheBackend verifies work no client is waiting
// on, such as a lifecycle expiry page, settles before it returns, so a pass
// cannot run ahead of its deletes.
func TestSettle_BackgroundWorkWaitsForTheBackend(t *testing.T) {
	eachSettledRequest(t, func(t *testing.T, answer func(context.Context, *fleet)) {
		f, rows := newSettleFleet(t)
		start := time.Now()
		answer(audit.WithWorkload(context.Background(), audit.WorkloadDrain), f)
		if took := time.Since(start); took != backendDelete {
			t.Errorf("returned after %s, want %s", took, backendDelete)
		}
		if rows.retired.Load() != 1 {
			t.Errorf("retired %d rows, want 1", rows.retired.Load())
		}
	})
}
