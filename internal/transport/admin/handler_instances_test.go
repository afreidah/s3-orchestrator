// -------------------------------------------------------------------------------
// Admin API - Views Gathered From Every Instance - Tests
//
// Author: Alex Freidah
//
// Covers the instance list and the fleet-wide worker view over an in-memory
// store standing in for Redis, the instance header every response carries,
// and the cache change announced to the other instances.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/sharedstate"
	"github.com/afreidah/s3-orchestrator/internal/sharedstate/sharedstatetest"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// sharedHandler builds a test handler for instance "a" over mem, with instance
// "b" already reporting the given workers.
func sharedHandler(t *testing.T, mem *sharedstatetest.Memory, bWorkers []adminapi.WorkerHealth) *Handler {
	t.Helper()
	h := newTestHandler(t)
	h.d.Instance = adminapi.InstanceStatus{ID: "a", Mode: "all"}
	h.instances = h.instanceRoster(sharedstate.New(mem, "a"))
	other, err := json.Marshal(adminapi.InstanceReport{
		Instance: adminapi.InstanceStatus{ID: "b", Mode: "worker"},
		LogLevel: "debug",
		Workers:  bWorkers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.PutShared(context.Background(), instanceRosterName+":b", other, time.Minute); err != nil {
		t.Fatal(err)
	}
	return h
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestHandleInstances_ListsEveryInstance answers with this instance's report
// first, built now, then the other instance's, and names itself in the header.
func TestHandleInstances_ListsEveryInstance(t *testing.T) {
	t.Parallel()
	h := sharedHandler(t, sharedstatetest.NewMemory(), nil)
	w := serveOn(t, h, http.MethodGet, adminapi.PathInstances)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(adminapi.HeaderInstance); got != "a" {
		t.Errorf("%s = %q, want a", adminapi.HeaderInstance, got)
	}
	var resp adminapi.InstancesResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Instances) != 2 {
		t.Fatalf("instances = %+v, want a and b", resp.Instances)
	}
	self, other := resp.Instances[0], resp.Instances[1]
	if self.Instance.ID != "a" || self.LogLevel != "info" || self.Reload.Status != reloadNotYetStatus || self.ReportedAt.IsZero() {
		t.Errorf("own report = %+v", self)
	}
	if other.Instance.ID != "b" || other.LogLevel != "debug" {
		t.Errorf("other report = %+v", other)
	}
}

// TestHandleWorkers_TakesTheLatestTickAcrossInstances serves, for each worker,
// the tick of the instance that ran it last, and lists a worker only another
// instance runs after this instance's own.
func TestHandleWorkers_TakesTheLatestTickAcrossInstances(t *testing.T) {
	t.Parallel()
	older, newer := time.Unix(1715000000, 0).UTC(), time.Unix(1715000600, 0).UTC()
	h := sharedHandler(t, sharedstatetest.NewMemory(), []adminapi.WorkerHealth{
		{Name: "rebalance", LastFailure: newer, LastError: "boom", ConsecutiveFailures: 1},
		{Name: "scrub", LastSuccess: older},
		{Name: "reconcile", LastSuccess: newer},
	})
	h.d.WorkerHealth = func() []adminapi.WorkerHealth {
		return []adminapi.WorkerHealth{
			{Name: "rebalance", LastSuccess: older},
			{Name: "scrub", LastSuccess: newer},
			{Name: "lifecycle"},
		}
	}
	w := serveOn(t, h, http.MethodGet, adminapi.PathWorkers)

	var resp adminapi.WorkersResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []adminapi.WorkerHealth{
		{Name: "rebalance", Instance: "b", LastFailure: newer, LastError: "boom", ConsecutiveFailures: 1},
		{Name: "scrub", Instance: "a", LastSuccess: newer},
		{Name: "lifecycle"},
		{Name: "reconcile", Instance: "b", LastSuccess: newer},
	}
	if len(resp.Workers) != len(want) {
		t.Fatalf("workers = %+v, want %+v", resp.Workers, want)
	}
	for i := range want {
		got := resp.Workers[i]
		if got.Name != want[i].Name || got.Instance != want[i].Instance || !got.LastSuccess.Equal(want[i].LastSuccess) ||
			!got.LastFailure.Equal(want[i].LastFailure) || got.LastError != want[i].LastError {
			t.Errorf("worker %d = %+v, want %+v", i, got, want[i])
		}
	}
}

// TestHandleWorkers_ServesOwnWhenStoreFails still answers with this
// instance's workers when the other instances' reports cannot be read.
func TestHandleWorkers_ServesOwnWhenStoreFails(t *testing.T) {
	t.Parallel()
	mem := sharedstatetest.NewMemory()
	h := sharedHandler(t, mem, nil)
	h.d.WorkerHealth = func() []adminapi.WorkerHealth { return []adminapi.WorkerHealth{{Name: "scrub"}} }
	mem.Fail(context.DeadlineExceeded)
	w := serveOn(t, h, http.MethodGet, adminapi.PathWorkers)

	var resp adminapi.WorkersResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if w.Code != http.StatusOK || len(resp.Workers) != 1 || resp.Workers[0].Name != "scrub" {
		t.Errorf("status %d, workers %+v; want 200 and scrub alone", w.Code, resp.Workers)
	}
}

// TestHandleCacheFlush_AnnouncedToOtherInstances empties this instance's cache
// and publishes the flush for the others to apply.
func TestHandleCacheFlush_AnnouncedToOtherInstances(t *testing.T) {
	t.Parallel()
	mem := sharedstatetest.NewMemory()
	h := newTestHandlerWithCache(t)
	h.cacheTopic = cache.InvalidationTopic(sharedstate.New(mem, "a"))
	h.d.ObjectCache.PutBytes("k", []byte("x"), cache.EntryMeta{})

	got := make(chan cache.Invalidation, 1)
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cache.InvalidationTopic(sharedstate.New(mem, "b")).Watch(ctx,
			func(_ context.Context, inv cache.Invalidation) { got <- inv },
			func(context.Context) { close(ready) })
	}()
	defer func() {
		cancel()
		<-done
	}()
	<-ready

	w := serveOn(t, h, http.MethodPost, adminapi.PathCacheFlush)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if h.d.ObjectCache.Stats().Entries != 0 {
		t.Error("this instance's cache not emptied")
	}
	select {
	case inv := <-got:
		if !inv.All {
			t.Errorf("announced %+v, want a full flush", inv)
		}
	default:
		t.Error("flush not announced to the other instances")
	}
}
