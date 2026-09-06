// -------------------------------------------------------------------------------
// Object Operations Tests
//
// Author: Alex Freidah
//
// Tests for object CRUD: PutObject routing and quota enforcement,
// GetObject failover across replicas, HeadObject, DeleteObject broadcast, and
// CopyObject. Uses mock backends and stores to verify routing strategy behavior.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// objectsCalls holds the per-test capture state for assertions.
// objectsCalls records what the write path asked the store to do. full names
// the backends whose claim is declined, standing in for the headroom test the
// real insert makes: a backend is out of room when the database says so, not
// when an in-memory figure does.
type objectsCalls struct {
	mu             sync.Mutex
	recordObject   []objRecordCall
	insertPending  []core.PendingObject
	enqueueCleanup []core.CleanupItem
	full           map[string]bool
}

type objRecordCall struct {
	Key, Backend string
	Size         int64
	Form         *core.StoredForm
	Tags         []core.Tag // the set the write carried, replacing whatever the key held
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

func stubObjRecord(c *objectsCalls, err error) func(context.Context, *core.RecordObjectRequest) ([]core.DeletedCopy, core.QuotaDeltas, error) {
	return func(_ context.Context, req *core.RecordObjectRequest) ([]core.DeletedCopy, core.QuotaDeltas, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		backend := req.Copies[0].Backend
		c.recordObject = append(c.recordObject, objRecordCall{
			Key: req.Key, Backend: backend, Size: req.Size, Form: req.Form, Tags: req.Tags,
		})
		return nil, core.QuotaDeltas{backend: req.Size}, err
	}
}

// stubObjInsertPending records the claim and admits it unless the test declared
// that backend full, which is how the real insert reports one without room.
func stubObjInsertPending(c *objectsCalls, err error) func(context.Context, *core.PendingObject) (bool, error) {
	return func(_ context.Context, p *core.PendingObject) (bool, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			return false, err
		}
		if c.full[p.BackendName] {
			return false, nil
		}
		c.insertPending = append(c.insertPending, *p)
		return true, nil
	}
}

func stubObjEnqueue(c *objectsCalls) func(context.Context, string, string, string, int64) error {
	return func(_ context.Context, backend, key, reason string, size int64) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.enqueueCleanup = append(c.enqueueCleanup, core.CleanupItem{
			BackendName: backend, ObjectKey: key, Reason: reason, SizeBytes: size,
		})
		return nil
	}
}

// objectsStubs wires the default object-path stubs onto store and
// returns the calls accumulator.
func objectsStubs(store *storetest.MockMetadataStore) *objectsCalls {
	c := &objectsCalls{}
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjRecord(c, nil)).AnyTimes()
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjInsertPending(c, nil)).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjEnqueue(c)).AnyTimes()
	return c
}

// putObjectStore returns a store wired for a successful PutObject path.
// Placement is decided from the in-memory byte counter rather than the store,
// so the backend a write lands on is whichever the fleet's order and baselines
// admit rather than anything stubbed here.
func putObjectStore(t *testing.T) (*storetest.MockMetadataStore, *objectsCalls) {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	c := objectsStubs(store)
	storetest.Permissive(store)
	return store, c
}

// eligibleStore is putObjectStore under its older name, kept for the tests that
// read as "any eligible backend will do".
func eligibleStore(t *testing.T) (*storetest.MockMetadataStore, *objectsCalls) {
	t.Helper()
	return putObjectStore(t)
}

// locationsStore returns a store with GetAllObjectLocations stubbed.
func locationsStore(t *testing.T, locs []core.ObjectLocation, err error) *storetest.MockMetadataStore {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	objectsStubs(store)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return(locs, err).AnyTimes()
	storetest.Permissive(store)
	return store
}

// listObjectsStore wires a single ListObjects response.
func listObjectsStore(t *testing.T, resp *core.ListObjectsResult, err error) *storetest.MockMetadataStore {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	objectsStubs(store)
	store.EXPECT().ListObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(resp, err).AnyTimes()
	storetest.Permissive(store)
	return store
}

// listObjectsDelimitedStore wires a single ListObjectsDelimited response for the
// delimiter path, which the store now serves as one grouped page.
func listObjectsDelimitedStore(t *testing.T, resp *core.ListDelimitedResult, err error) *storetest.MockMetadataStore {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	objectsStubs(store)
	store.EXPECT().ListObjectsDelimited(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(resp, err).AnyTimes()
	storetest.Permissive(store)
	return store
}

// deleteObjectStore wires a DeleteObject response.
func deleteObjectStore(t *testing.T, resp []core.DeletedCopy, err error) *storetest.MockMetadataStore {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	objectsStubs(store)
	store.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).Return(resp, nil, err).AnyTimes()
	storetest.Permissive(store)
	return store
}

// TestPutObject_Success drives the happy path.
func TestPutObject_Success(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	store, c := putObjectStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	etag, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "mykey", Body: bytes.NewReader([]byte("hello")), Size: 5, ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if !be.Has("mykey") {
		t.Error("object not found on be")
	}
	if len(c.recordObject) != 1 {
		t.Fatalf("expected 1 RecordObject call, got %d", len(c.recordObject))
	}
	call := c.recordObject[0]
	if call.Key != "mykey" || call.Backend != "b1" || call.Size != 5 {
		t.Errorf("RecordObject called with %+v", call)
	}
}

// flippingDrainChecker reports a backend as not draining on the first
// IsDraining call and as draining on every subsequent call. Simulates
// the exact race the attemptPutOnBackend re-check closes: the upstream
// EligibleForWrite filter sees the backend healthy (call 1 -> false), a
// drain starts mid-PUT, and the post-PutObject re-check fires
// (call 2 -> true) so the orchestrator aborts the commit on the now-
// draining backend.
type flippingDrainChecker struct {
	mu      sync.Mutex
	backend string
	calls   int
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

func (f *flippingDrainChecker) IsDraining(name string) bool {
	if name != f.backend {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.calls > 1
}

// TestPutObject_DrainRace_AbortsAndFailsOver actually triggers the
// drain-race close. The flipping checker reports b1 as healthy on the
// first IsDraining call (the upstream EligibleForWrite filter) and as
// draining on every subsequent call (the post-PutObject re-check in
// attemptPutOnBackend). That exercises the exact race window the fix
// closes: b1 passes eligibility, the backend PUT completes, and the
// re-check then catches the drain that started mid-write so the
// commit aborts and the bytes are cleaned up. The orchestrator fails
// the attempt over to b2.
func TestPutObject_DrainRace_AbortsAndFailsOver(t *testing.T) {
	// Not parallel: asserts an exact +1 delta on the global
	// telemetry.DrainRaceAbortedTotal counter, which is also bumped by
	// TestPutObject_DrainRace_AllBackendsDraining.
	drained := backendtest.NewInMemory()
	healthy := backendtest.NewInMemory()
	store, _ := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": drained, "b2": healthy}, &fleetOpts{Order: []string{"b1", "b2"}})
	mgr.Runtime.SetDrainChecker(&flippingDrainChecker{backend: "b1"})

	before := testutil.ToFloat64(telemetry.DrainRaceAbortedTotal)
	etag, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "mykey", Body: bytes.NewReader([]byte("hello")), Size: 5, ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag from the failover backend")
	}
	if drained.Has("mykey") {
		t.Error("draining backend still holds the orphaned bytes; RecoverFromRecordFailure did not delete")
	}
	if !healthy.Has("mykey") {
		t.Error("healthy backend did not receive the failed-over write")
	}
	if got := testutil.ToFloat64(telemetry.DrainRaceAbortedTotal); got != before+1 {
		t.Errorf("DrainRaceAbortedTotal incremented by %v, want 1 (proves the re-check fired)", got-before)
	}
}

// TestPutObject_DrainRace_AllBackendsDraining surfaces the failure
// path: when every eligible backend flips to draining mid-write, the
// retry loop exhausts without committing anywhere.
func TestPutObject_DrainRace_AllBackendsDraining(t *testing.T) {
	// Not parallel: bumps the shared telemetry.DrainRaceAbortedTotal
	// counter that TestPutObject_DrainRace_AbortsAndFailsOver asserts an
	// exact delta against.
	drainedA := backendtest.NewInMemory()
	drainedB := backendtest.NewInMemory()
	store, _ := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": drainedA, "b2": drainedB}, &fleetOpts{Order: []string{"b1", "b2"}})
	// Drain checker that flips both backends to draining after their
	// EligibleForWrite check; the per-attempt re-check fires for each.
	mgr.Runtime.SetDrainChecker(&allFlippingDrainChecker{})

	_, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "mykey", Body: bytes.NewReader([]byte("hello")), Size: 5, ContentType: "text/plain"})
	if err == nil {
		t.Fatal("expected error when every backend flipped to draining mid-write")
	}
	if drainedA.Has("mykey") || drainedB.Has("mykey") {
		t.Error("orphaned bytes left on a draining backend; RecoverFromRecordFailure did not delete")
	}
}

// allFlippingDrainChecker reports every backend as not draining on its
// first IsDraining call and as draining on every subsequent call.
// Drives the all-backends-flip-mid-write scenario.
type allFlippingDrainChecker struct {
	mu    sync.Mutex
	calls map[string]int
}

func (a *allFlippingDrainChecker) IsDraining(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.calls == nil {
		a.calls = make(map[string]int)
	}
	a.calls[name]++
	return a.calls[name] > 1
}

// TestPutObject_PackStrategy_FillsTheFirstBackend pins pack routing: the
// configured order decides, so a write lands on the first backend with room
// even when a later one is emptier.
func TestPutObject_PackStrategy_FillsTheFirstBackend(t *testing.T) {
	t.Parallel()
	first, second := backendtest.NewInMemory(), backendtest.NewInMemory()
	store, _ := putObjectStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": first, "b2": second}, &fleetOpts{
		Order:          []string{"b1", "b2"},
		BackendTimeout: 30 * time.Second,
		QuotaBaselines: map[string]core.BackendQuotaUsage{
			"b1": {BackendName: "b1", BytesLimit: 1000, BytesUsed: 900},
			"b2": {BackendName: "b2", BytesLimit: 1000},
		},
	})

	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "pack-key", Body: bytes.NewReader([]byte("data")), Size: 4, ContentType: "text/plain"}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if !first.Has("pack-key") {
		t.Error("pack routing should fill the first backend in order")
	}
	if second.Has("pack-key") {
		t.Error("pack routing moved on while the first backend still had room")
	}
}

// TestPutObject_SpreadStrategy_PicksTheEmptiest pins spread routing against the
// same fleet: the emptiest backend wins regardless of where it sits in the
// order, and the utilization it is judged on comes from the in-memory counter.
func TestPutObject_SpreadStrategy_PicksTheEmptiest(t *testing.T) {
	t.Parallel()
	first, second := backendtest.NewInMemory(), backendtest.NewInMemory()
	store, _ := putObjectStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": first, "b2": second}, &fleetOpts{
		Order:          []string{"b1", "b2"},
		Routing:        config.RoutingSpread,
		BackendTimeout: 30 * time.Second,
		QuotaBaselines: map[string]core.BackendQuotaUsage{
			"b1": {BackendName: "b1", BytesLimit: 1000, BytesUsed: 900},
			"b2": {BackendName: "b2", BytesLimit: 1000},
		},
	})

	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "spread-key", Body: bytes.NewReader([]byte("data")), Size: 4, ContentType: "text/plain"}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if !second.Has("spread-key") {
		t.Error("spread routing should land on the least utilized backend")
	}
}

// TestCanAcceptWrite_HasCapacity asserts the positive-capacity branch.
func TestCanAcceptWrite_HasCapacity(t *testing.T) {
	t.Parallel()
	store, _ := putObjectStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	if !mgr.CanAcceptWrite(100) {
		t.Error("CanAcceptWrite should return true when backend has capacity")
	}
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// requestCapped builds a backend's limits from a bare monthly request cap plus
// byte caps, the shape api_request_limit desugars into.
func requestCapped(t *testing.T, api, egress, ingress int64) core.UsageLimits {
	t.Helper()
	lim, err := core.NewUsageLimits(egress, ingress, core.SingleRequestPool(api), nil)
	if err != nil {
		t.Fatalf("build limits: %v", err)
	}
	return lim
}

// alreadySpent seeds a period partly consumed. Both halves of the baseline are
// set: the request total, and the wildcard pool's share of it that admission
// actually judges against.
func alreadySpent(mgr *fleet, name string, api, egress, ingress int64) {
	stat := core.UsageStat{APIRequests: api, EgressBytes: egress, IngressBytes: ingress}
	var pools core.PoolUsage
	if api > 0 {
		pools = core.PoolUsage{core.PoolAll: api}
	}
	mgr.Runtime.Usage().SetBaseline(name, stat, pools)
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestCanAcceptWrite_NoCapacity asserts the over-limit branch.
func TestCanAcceptWrite_NoCapacity(t *testing.T) {
	t.Parallel()
	limits := map[string]core.UsageLimits{
		"b1": requestCapped(t, 1, 0, 0),
	}
	mgr := newFleet(t, newPermissiveStore(t), map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{UsageLimits: limits})

	alreadySpent(mgr, "b1", 1, 0, 0)

	if mgr.CanAcceptWrite(100) {
		t.Error("CanAcceptWrite should return false when no backend has capacity")
	}
}

// TestBackendCapacityStats_PassesThroughStoreSnapshot pins the snapshot
// pass-through.
func TestBackendCapacityStats_PassesThroughStoreSnapshot(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(map[string]core.QuotaStat{
			"b1": {BackendName: "b1", BytesUsed: 100, BytesLimit: 1000},
		}, nil).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	got := mgr.BackendCapacityStats(context.Background())
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got["b1"].BytesUsed != 100 || got["b1"].BytesLimit != 1000 {
		t.Errorf("snapshot mismatch: %+v", got["b1"])
	}
}

// TestBackendCapacityStats_DBFailureReturnsNil asserts a DB failure
// degrades to nil.
func TestBackendCapacityStats_DBFailureReturnsNil(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetQuotaStats(gomock.Any()).
		Return(nil, core.ErrDBUnavailable).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	if got := mgr.BackendCapacityStats(context.Background()); got != nil {
		t.Errorf("BackendCapacityStats on DB failure = %+v, want nil", got)
	}
}

// TestPutObject_PlacementErrors pins what a write reports when placement
// cannot answer: the store's failure is translated into the sentinel the
// transport turns into a status, and the two are not the same failure.
func TestPutObject_PlacementErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		wantSent  error
		wantWhyOf string
	}{
		{
			name:      "the only backend declines the claim",
			wantSent:  core.ErrInsufficientStorage,
			wantWhyOf: "insufficient storage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, calls := putObjectStore(t)
			// The backend is out of room, which it reports by declining the
			// claim rather than by any figure held in memory.
			calls.full = map[string]bool{"b1": true}
			mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

			_, err := mgr.PutObject(context.Background(), &PutObjectRequest{
				Key: "key", Body: bytes.NewReader([]byte("x")), Size: 1,
			})
			if !errors.Is(err, tt.wantSent) {
				t.Fatalf("err = %v, want %s", err, tt.wantWhyOf)
			}
		})
	}
}

// TestPutObject_BackendFailure_StillRecordsUsage pins API-call counting
// on be failures.
func TestPutObject_BackendFailure_StillRecordsUsage(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.PutErr = errors.New("be timeout")
	store, _ := putObjectStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "key", Body: bytes.NewReader([]byte("data")), Size: 4, ContentType: "text/plain"}); err == nil {
		t.Fatal("expected error from be failure")
	}
	if got := mgr.Runtime.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != 1 {
		t.Errorf("apiRequests = %d, want 1 (failed call still counts)", got)
	}
	if got := mgr.Runtime.Usage().Backend().Load("b1", counter.FieldIngressBytes); got != 0 {
		t.Errorf("ingressBytes = %d, want 0 (upload failed)", got)
	}
}

// TestPutObject_RecordFailure_LeavesBackendBytesAndPendingIntent pins
// the pending-row pattern: be bytes survive a metadata commit
// failure.
func TestPutObject_RecordFailure_LeavesBackendBytesAndPendingIntent(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	c := &objectsCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjRecord(c, errors.New("db write failed"))).AnyTimes()
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjInsertPending(c, nil)).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjEnqueue(c)).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "cleanup-key", Body: bytes.NewReader([]byte("data")), Size: 4}); err == nil {
		t.Fatal("expected error from RecordObjectAndClearPending failure")
	}
	if !be.Has("cleanup-key") {
		t.Error("be bytes should be retained for the pending reaper to resolve")
	}
	if len(c.insertPending) != 1 {
		t.Fatalf("expected 1 InsertPending call, got %d", len(c.insertPending))
	}
	if c.insertPending[0].ObjectKey != "cleanup-key" || c.insertPending[0].BackendName != "b1" {
		t.Errorf("InsertPending called with %+v", c.insertPending[0])
	}
	if got := mgr.Runtime.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != 1 {
		t.Errorf("apiRequests = %d, want 1 (PUT only)", got)
	}
}

// TestPutObject_RecordFailure_LegacyPath asserts that with the pending
// store nil, the legacy delete-on-failure path runs.
// errReader is an io.Reader that always returns the configured error.
type errReader struct{ err error }

// Read returns the configured error.
func (r *errReader) Read([]byte) (int, error) { return 0, r.err }

// TestPutObject_WriteFailover_Success pins the failover happy path.
func TestPutObject_WriteFailover_Success(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b1.PutErr = errors.New("connection refused")
	b2 := backendtest.NewInMemory()

	store, c := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{Order: []string{"b1", "b2"}})

	etag, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "failover-key", Body: bytes.NewReader([]byte("hello")), Size: 5, ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("PutObject should succeed via failover: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if b1.Has("failover-key") {
		t.Error("object should NOT be on failed backend b1")
	}
	if !b2.Has("failover-key") {
		t.Error("object should be on failover backend b2")
	}
	if len(c.recordObject) != 1 {
		t.Fatalf("expected 1 RecordObject call, got %d", len(c.recordObject))
	}
	if c.recordObject[0].Backend != "b2" {
		t.Errorf("RecordObject backend = %s, want b2", c.recordObject[0].Backend)
	}
}

// TestPutObject_WriteFailover_AllBackendsFail asserts that every backend
// is tried before giving up.
func TestPutObject_WriteFailover_AllBackendsFail(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b1.PutErr = errors.New("b1 down")
	b2 := backendtest.NewInMemory()
	b2.PutErr = errors.New("b2 down")
	b3 := backendtest.NewInMemory()
	b3.PutErr = errors.New("b3 down")

	store, _ := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2, "b3": b3}, &fleetOpts{Order: []string{"b1", "b2", "b3"}})

	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "key", Body: bytes.NewReader([]byte("data")), Size: 4, ContentType: "text/plain"}); err == nil {
		t.Fatal("expected error when all backends fail")
	}
	total := mgr.Runtime.Usage().Backend().Load("b1", counter.FieldAPIRequests) +
		mgr.Runtime.Usage().Backend().Load("b2", counter.FieldAPIRequests) +
		mgr.Runtime.Usage().Backend().Load("b3", counter.FieldAPIRequests)
	if total != 3 {
		t.Errorf("total API requests = %d, want 3 (one per failed backend)", total)
	}
}

// TestPutObject_WriteFailover_SkipsMultipleFailedBackends pins the
// retry-many-then-succeed branch.
func TestPutObject_WriteFailover_SkipsMultipleFailedBackends(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b1.PutErr = errors.New("b1 down")
	b2 := backendtest.NewInMemory()
	b2.PutErr = errors.New("b2 down")
	b3 := backendtest.NewInMemory()

	store, calls := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2, "b3": b3}, &fleetOpts{Order: []string{"b1", "b2", "b3"}})

	etag, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "key", Body: bytes.NewReader([]byte("data")), Size: 4, ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("PutObject should succeed on b3: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if !b3.Has("key") {
		t.Error("object should be on b3")
	}
	// Every attempt claims its own candidate, so failover leaves one intent per
	// backend it tried. The two failed ones are deliberately not withdrawn: a
	// backend PUT error does not prove the bytes are absent, so the reaper HEADs
	// each one and drops the intent only once it knows. Until then those bytes
	// stay claimed, which is the conservative direction.
	var claimed []string
	for _, p := range calls.insertPending {
		claimed = append(claimed, p.BackendName)
	}
	if want := []string{"b1", "b2", "b3"}; !slices.Equal(claimed, want) {
		t.Errorf("claimed %v, want %v - one intent per backend the failover tried", claimed, want)
	}
}

// TestPutObject_WriteFailover_Metrics pins the failover-metric increment.
func TestPutObject_WriteFailover_Metrics(t *testing.T) {
	telemetry.WriteFailoverTotal.Reset()

	b1 := backendtest.NewInMemory()
	b1.PutErr = errors.New("b1 down")
	b2 := backendtest.NewInMemory()

	store, _ := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{Order: []string{"b1", "b2"}})

	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "key", Body: bytes.NewReader([]byte("data")), Size: 4, ContentType: "text/plain"}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	got := testutil.ToFloat64(telemetry.WriteFailoverTotal.WithLabelValues("PutObject", "b1", "b2"))
	if got != 1 {
		t.Errorf("WriteFailoverTotal{PutObject,b1,b2} = %v, want 1", got)
	}
}

// TestPutObject_WriteFailover_UsageTracking pins per-backend usage
// counting during failover.
func TestPutObject_WriteFailover_UsageTracking(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b1.PutErr = errors.New("b1 timeout")
	b2 := backendtest.NewInMemory()

	store, _ := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{Order: []string{"b1", "b2"}})

	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "key", Body: bytes.NewReader([]byte("data")), Size: 4, ContentType: "text/plain"}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	if got := mgr.Runtime.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != 1 {
		t.Errorf("b1 apiRequests = %d, want 1", got)
	}
	if got := mgr.Runtime.Usage().Backend().Load("b1", counter.FieldIngressBytes); got != 0 {
		t.Errorf("b1 ingressBytes = %d, want 0", got)
	}
	if got := mgr.Runtime.Usage().Backend().Load("b2", counter.FieldAPIRequests); got != 1 {
		t.Errorf("b2 apiRequests = %d, want 1", got)
	}
	if got := mgr.Runtime.Usage().Backend().Load("b2", counter.FieldIngressBytes); got != 4 {
		t.Errorf("b2 ingressBytes = %d, want 4", got)
	}
}

// TestPutObject_WriteFailover_DataIntegrity asserts the failed-over
// payload survives intact.
func TestPutObject_WriteFailover_DataIntegrity(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b1.PutErr = errors.New("b1 down")
	b2 := backendtest.NewInMemory()

	store, _ := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{Order: []string{"b1", "b2"}})

	payload := []byte("the quick brown fox jumps over the lazy dog")
	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "key", Body: bytes.NewReader(payload), Size: int64(len(payload)), ContentType: "text/plain", Metadata: map[string]string{"x-custom": "value"}}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	obj, _ := b2.Get("key")

	if !bytes.Equal(obj.Data, payload) {
		t.Errorf("data mismatch: got %d bytes, want %d bytes", len(obj.Data), len(payload))
	}
	if obj.ContentType != "text/plain" {
		t.Errorf("contentType = %s, want text/plain", obj.ContentType)
	}
	if obj.Metadata["x-custom"] != "value" {
		t.Errorf("metadata[x-custom] = %s, want value", obj.Metadata["x-custom"])
	}
}

// TestPutObject_WriteFailover_BufferBodyError surfaces the body-buffer
// failure path.
func TestPutObject_WriteFailover_BufferBodyError(t *testing.T) {
	t.Parallel()
	store, _ := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{Order: []string{"b1"}})

	_, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "key", Body: &errReader{err: errors.New("read failed")}, Size: 4, ContentType: "text/plain"})
	if err == nil {
		t.Fatal("expected error from body buffer failure")
	}
	if got := err.Error(); got != "buffer request body: read failed" {
		t.Errorf("error = %q, want %q", got, "buffer request body: read failed")
	}
}

// TestPutObject_WriteFailover_SelectBackendErrorDuringRetry exercises a
// second attempt has nowhere left to go.
func TestPutObject_WriteFailover_NoRoomOnTheRetry(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b1.PutErr = errors.New("b1 down")

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	calls := objectsStubs(store)
	storetest.Permissive(store)

	// b1 takes the write and fails it; b2 declines the claim, so the retry has
	// no candidate left and the write surfaces the storage sentinel rather than
	// looping.
	calls.full = map[string]bool{"b2": true}
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": backendtest.NewInMemory()}, &fleetOpts{
		Order: []string{"b1", "b2"},
	})

	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "key", Body: bytes.NewReader([]byte("data")), Size: 4, ContentType: "text/plain"}); err == nil {
		t.Fatal("expected the write to fail once no backend could take it")
	}
}

// TestPutObject_WriteFailover_WithEncryption pins the encryption-aware
// failover path.
func TestPutObject_WriteFailover_WithEncryption(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b1.PutErr = errors.New("b1 down")
	b2 := backendtest.NewInMemory()

	provider, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 64*1024)
	if err != nil {
		t.Fatal(err)
	}

	store, c := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{Order: []string{"b1", "b2"}, BackendTimeout: 30 * time.Second, Encryptor: enc})

	payload := []byte("encrypt-failover-test-data")
	etag, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "enc-key", Body: bytes.NewReader(payload), Size: int64(len(payload)), ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("PutObject with encryption failover: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if b1.Has("enc-key") {
		t.Error("object should NOT be on failed backend b1")
	}
	if !b2.Has("enc-key") {
		t.Error("object should be on failover backend b2")
	}
	if len(c.recordObject) != 1 {
		t.Fatalf("expected 1 RecordObject call, got %d", len(c.recordObject))
	}
	if c.recordObject[0].Backend != "b2" {
		t.Errorf("RecordObject backend = %s, want b2", c.recordObject[0].Backend)
	}

	ciphertextLenObj, _ := b2.Get("enc-key")
	ciphertextLen := len(ciphertextLenObj.Data)
	if ciphertextLen <= len(payload) {
		t.Errorf("ciphertext len %d should be > plaintext len %d", ciphertextLen, len(payload))
	}
}

// TestGetObject_WithEncryption_UsesLocationMap exercises the
// location-map build path.
func TestGetObject_WithEncryption_UsesLocationMap(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "enc-key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	provider, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 64*1024)
	if err != nil {
		t.Fatal(err)
	}

	store := locationsStore(t,
		[]core.ObjectLocation{{ObjectKey: "enc-key", BackendName: "b1", SizeBytes: 4, Encrypted: false}},
		nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1}, &fleetOpts{Order: []string{"b1"}, BackendTimeout: 30 * time.Second, Encryptor: enc})

	result, err := mgr.GetObject(context.Background(), "enc-key", "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer func() { _ = result.Body.Close() }()

	got, _ := io.ReadAll(result.Body)
	if string(got) != "data" {
		t.Errorf("body = %q, want %q", got, "data")
	}
}

// TestHeadObject_WithEncryption asserts HeadObject returns the
// plaintext size.
func TestHeadObject_WithEncryption(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()

	provider, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 64*1024)
	if err != nil {
		t.Fatal(err)
	}

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "enc-key", BackendName: "b1", SizeBytes: 100, Encrypted: true, PlaintextSize: 25, EncryptionKey: []byte("wrapped-dek")},
		}, nil).AnyTimes()
	objectsStubs(store)
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1}, &fleetOpts{Order: []string{"b1"}, BackendTimeout: 30 * time.Second, Encryptor: enc})

	payload := []byte("head-encryption-test-data")
	if _, err = mgr.PutObject(context.Background(), &PutObjectRequest{Key: "enc-key", Body: bytes.NewReader(payload), Size: int64(len(payload)), ContentType: "text/plain"}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	head, err := mgr.HeadObject(context.Background(), "enc-key")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if head.Size != 25 {
		t.Errorf("HeadObject size = %d, want 25 (plaintext size from location)", head.Size)
	}
}

// TestPutObject_WriteFailover_NoFailoverMetricOnFirstSuccess asserts
// the first-success branch doesn't increment the metric.
func TestPutObject_WriteFailover_NoFailoverMetricOnFirstSuccess(t *testing.T) {
	telemetry.WriteFailoverTotal.Reset()

	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()

	store, _ := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{Order: []string{"b1", "b2"}})

	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "key", Body: bytes.NewReader([]byte("data")), Size: 4, ContentType: "text/plain"}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	got := testutil.ToFloat64(telemetry.WriteFailoverTotal.WithLabelValues("PutObject", "b1", "b2"))
	if got != 0 {
		t.Errorf("WriteFailoverTotal should be 0 when no failover occurs, got %v", got)
	}
}

// TestGetObject_Success drives the GetObject happy path.
func TestGetObject_Success(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "key", bytes.NewReader([]byte("hello")), 5, "text/plain", nil)

	store := locationsStore(t, []core.ObjectLocation{{ObjectKey: "key", BackendName: "b1"}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	result, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
	if result.Size != 5 {
		t.Errorf("size = %d, want 5", result.Size)
	}
	if result.ContentType != "text/plain" {
		t.Errorf("content-type = %q, want %q", result.ContentType, "text/plain")
	}
	got, _ := io.ReadAll(result.Body)
	if string(got) != "hello" {
		t.Errorf("body = %q, want %q", got, "hello")
	}
}

// TestGetObject_NotFound surfaces the not-found error.
func TestGetObject_NotFound(t *testing.T) {
	t.Parallel()
	store := locationsStore(t, nil, core.ErrObjectNotFound)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	if _, err := mgr.GetObject(context.Background(), "missing", ""); !errors.Is(err, core.ErrObjectNotFound) {
		t.Fatalf("expected st.ErrObjectNotFound, got %v", err)
	}
}

// TestGetObject_FailoverToReplica pins the replica failover path.
func TestGetObject_FailoverToReplica(t *testing.T) {
	t.Parallel()
	primary := backendtest.NewInMemory()
	primary.GetErr = errors.New("backend down")
	replica := backendtest.NewInMemory()
	_, _ = replica.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	store := locationsStore(t, []core.ObjectLocation{
		{ObjectKey: "key", BackendName: "primary"},
		{ObjectKey: "key", BackendName: "replica"},
	}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"primary": primary, "replica": replica}, nil)

	result, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("GetObject should failover: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
	got, _ := io.ReadAll(result.Body)
	if string(got) != "data" {
		t.Errorf("body = %q, want %q", got, "data")
	}
}

// TestGetObject_DBUnavailable_BroadcastHit asserts the broadcast hit
// branch when DB is down.
func TestGetObject_DBUnavailable_BroadcastHit(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "key", bytes.NewReader([]byte("broadcast")), 9, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	result, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("GetObject broadcast should succeed: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
	got, _ := io.ReadAll(result.Body)
	if string(got) != "broadcast" {
		t.Errorf("body = %q, want %q", got, "broadcast")
	}
}

// rangeRecordingBackend wraps the shared in-memory be to capture the Range
// header the proxy forwards on GetObject. Used to assert the degraded
// broadcast path does not strip Range before dispatching to backends.
type rangeRecordingBackend struct {
	*backendtest.InMemory
	receivedRange string
}

func (b *rangeRecordingBackend) GetObject(ctx context.Context, key, rangeHeader string) (*backend.GetObjectResult, error) {
	b.receivedRange = rangeHeader
	return b.InMemory.GetObject(ctx, key, rangeHeader)
}

// TestGetObject_DBUnavailable_RangeRequest pins that the degraded
// broadcast forwards the client's Range header to the backend instead
// of silently dropping it.
func TestGetObject_DBUnavailable_RangeRequest(t *testing.T) {
	t.Parallel()
	inner := backendtest.NewInMemory()
	_, _ = inner.PutObject(context.Background(), "k", bytes.NewReader([]byte("0123456789")), 10, "text/plain", nil)
	recorder := &rangeRecordingBackend{InMemory: inner}

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": recorder}, &fleetOpts{Order: []string{"b1"}, BackendTimeout: 30 * time.Second})

	result, err := mgr.GetObject(context.Background(), "k", "bytes=2-5")
	if err != nil {
		t.Fatalf("GetObject with Range during degraded mode: %v", err)
	}
	_ = result.Body.Close()
	if recorder.receivedRange != "bytes=2-5" {
		t.Errorf("backend received Range = %q, want %q", recorder.receivedRange, "bytes=2-5")
	}
}

// TestGetObject_DBUnavailable_DegradedReadsDisabled asserts the
// operator opt-out: a DB outage with DisableDegradedReads=true returns
// ErrServiceUnavailable instead of fanning out to every backend.
func TestGetObject_DBUnavailable_DegradedReadsDisabled(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key", bytes.NewReader([]byte("would-be-broadcast")), 18, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1}, &fleetOpts{Order: []string{"b1"}, BackendTimeout: 30 * time.Second, DisableDegradedReads: true})

	_, err := mgr.GetObject(context.Background(), "key", "")
	if !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("GetObject err = %v, want core.ErrServiceUnavailable", err)
	}
}

// TestGetObject_DBUnavailable_CacheHit asserts the cache hit branch
// after a successful broadcast.
func TestGetObject_DBUnavailable_CacheHit(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	_, _ = b2.PutObject(context.Background(), "cached-key", bytes.NewReader([]byte("cached")), 6, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, nil)

	r1, err := mgr.GetObject(context.Background(), "cached-key", "")
	if err != nil {
		t.Fatalf("first GetObject: %v", err)
	}
	_ = r1.Body.Close()

	r2, err := mgr.GetObject(context.Background(), "cached-key", "")
	if err != nil {
		t.Fatalf("second GetObject (cache hit): %v", err)
	}
	defer func() { _ = r2.Body.Close() }()
	got, _ := io.ReadAll(r2.Body)
	if string(got) != "cached" {
		t.Errorf("body = %q, want %q", got, "cached")
	}
}

// TestGetObject_DBUnavailable_AllFail asserts that backend errors
// surface raw rather than masking as not-found.
func TestGetObject_DBUnavailable_AllFail(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, nil)

	_, err := mgr.GetObject(context.Background(), "nowhere", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, core.ErrObjectNotFound) {
		t.Fatal("should not mask backend errors as st.ErrObjectNotFound")
	}
}

// TestGetObject_DBUnavailable_EncryptedRejects503 pins the
// encryption-aware DB-down rejection.
func TestGetObject_DBUnavailable_EncryptedRejects503(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "enc-key", bytes.NewReader([]byte("ciphertext")), 10, "text/plain", nil)

	provider, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 64*1024)
	if err != nil {
		t.Fatal(err)
	}

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1}, &fleetOpts{Order: []string{"b1"}, BackendTimeout: 30 * time.Second, Encryptor: enc})

	_, err = mgr.GetObject(context.Background(), "enc-key", "")
	if err == nil {
		t.Fatal("expected error for encrypted read with DB unavailable")
	}
	s3err, ok := errors.AsType[*core.S3Error](err)
	if !ok || s3err.StatusCode != 503 {
		t.Errorf("expected 503 S3Error, got: %v", err)
	}
}

// TestHeadObject_Success drives the HeadObject happy path.
func TestHeadObject_Success(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "key", bytes.NewReader([]byte("headme")), 6, "application/json", nil)

	store := locationsStore(t, []core.ObjectLocation{{ObjectKey: "key", BackendName: "b1"}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	result, err := mgr.HeadObject(context.Background(), "key")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if result.Size != 6 {
		t.Errorf("size = %d, want 6", result.Size)
	}
	if result.ContentType != "application/json" {
		t.Errorf("content-type = %q", result.ContentType)
	}
	if result.ETag == "" {
		t.Error("expected non-empty etag")
	}
}

// TestHeadObject_DBUnavailable_Broadcast asserts the broadcast head path.
func TestHeadObject_DBUnavailable_Broadcast(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "key", bytes.NewReader([]byte("head")), 4, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	result, err := mgr.HeadObject(context.Background(), "key")
	if err != nil {
		t.Fatalf("HeadObject broadcast should succeed: %v", err)
	}
	if result.Size != 4 {
		t.Errorf("size = %d, want 4", result.Size)
	}
}

// TestDeleteObject_Success drives the DeleteObject happy path.
func TestDeleteObject_Success(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "del-key", bytes.NewReader([]byte("rm")), 2, "", nil)

	store := deleteObjectStore(t, []core.DeletedCopy{{BackendName: "b1", SizeBytes: 2}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if err := mgr.DeleteObject(context.Background(), "del-key"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if be.Has("del-key") {
		t.Error("object should be deleted from be")
	}
}

// TestDeleteObject_NotFound_Idempotent asserts the not-found
// idempotent branch.
func TestDeleteObject_NotFound_Idempotent(t *testing.T) {
	t.Parallel()
	store := deleteObjectStore(t, nil, core.ErrObjectNotFound)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	if err := mgr.DeleteObject(context.Background(), "nonexistent"); err != nil {
		t.Fatalf("DeleteObject of nonexistent key should succeed (idempotent): %v", err)
	}
}

// TestDeleteObject_DBUnavailable surfaces the DB-down branch.
func TestDeleteObject_DBUnavailable(t *testing.T) {
	t.Parallel()
	store := deleteObjectStore(t, nil, core.ErrDBUnavailable)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	if err := mgr.DeleteObject(context.Background(), "key"); !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// stubBatchDelete returns a DoAndReturn that drives DeleteObjectsBatch.
func stubBatchDelete(fn func(keys []string) (map[string][]core.DeletedCopy, error)) func(context.Context, []string) (map[string][]core.DeletedCopy, core.QuotaDeltas, error) {
	return func(_ context.Context, keys []string) (map[string][]core.DeletedCopy, core.QuotaDeltas, error) {
		copies, err := fn(keys)
		deltas := make(core.QuotaDeltas)
		for _, perKey := range copies {
			for _, dc := range perKey {
				deltas.Add(dc.BackendName, -dc.SizeBytes)
			}
		}
		return copies, deltas, err
	}
}

// TestDeleteObjects_AllSuccess pins the per-key all-success path.
func TestDeleteObjects_AllSuccess(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	for _, k := range []string{"a", "b", "c"} {
		_, _ = be.PutObject(context.Background(), k, bytes.NewReader([]byte("x")), 1, "", nil)
	}

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	objectsStubs(store)
	store.EXPECT().DeleteObjectsBatch(gomock.Any(), gomock.Any()).
		DoAndReturn(stubBatchDelete(func(keys []string) (map[string][]core.DeletedCopy, error) {
			out := make(map[string][]core.DeletedCopy, len(keys))
			for _, k := range keys {
				out[k] = []core.DeletedCopy{{BackendName: "b1", SizeBytes: 1}}
			}
			return out, nil
		})).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	results := mgr.DeleteObjects(context.Background(), []string{"a", "b", "c"})
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("results[%d]: unexpected error: %v", i, r.Err)
		}
	}
	for _, k := range []string{"a", "b", "c"} {
		if be.Has(k) {
			t.Errorf("object %q should be deleted from be", k)
		}
	}
}

// TestDeleteObjects_DBFailureFailsAll pins the all-fail tx semantics.
func TestDeleteObjects_DBFailureFailsAll(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	objectsStubs(store)
	store.EXPECT().DeleteObjectsBatch(gomock.Any(), gomock.Any()).
		Return(nil, nil, errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	results := mgr.DeleteObjects(context.Background(), []string{"k1", "k2", "k3"})
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	for i, r := range results {
		if r.Err == nil {
			t.Errorf("results[%d]: expected DB error to surface", i)
		}
	}
}

// TestDeleteObjects_NotFoundIsSuccess pins the missing-keys-are-success
// behaviour.
func TestDeleteObjects_NotFoundIsSuccess(t *testing.T) {
	t.Parallel()
	store := newPermissiveStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	results := mgr.DeleteObjects(context.Background(), []string{"gone1", "gone2"})

	for i, r := range results {
		if r.Err != nil {
			t.Errorf("results[%d]: not-found should be success, got %v", i, r.Err)
		}
	}
}

// TestDeleteObjects_BackendFailureEnqueuesCleanup pins that be
// failures during batch delete enqueue cleanup rows.
func TestDeleteObjects_BackendFailureEnqueuesCleanup(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.DeleteErr = errors.New("be down")

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	c := objectsStubs(store)
	store.EXPECT().DeleteObjectsBatch(gomock.Any(), gomock.Any()).
		DoAndReturn(stubBatchDelete(func(keys []string) (map[string][]core.DeletedCopy, error) {
			out := make(map[string][]core.DeletedCopy, len(keys))
			for _, k := range keys {
				out[k] = []core.DeletedCopy{{BackendName: "b1", SizeBytes: 1}}
			}
			return out, nil
		})).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	results := mgr.DeleteObjects(context.Background(), []string{"k1", "k2"})

	for i, r := range results {
		if r.Err != nil {
			t.Errorf("results[%d]: unexpected error: %v", i, r.Err)
		}
	}

	if len(c.enqueueCleanup) != 2 {
		t.Fatalf("expected 2 enqueue calls, got %d", len(c.enqueueCleanup))
	}
	for _, e := range c.enqueueCleanup {
		if e.Reason != "batch_delete_failed" {
			t.Errorf("expected reason=batch_delete_failed, got %q", e.Reason)
		}
	}
}

// TestDeleteObjects_EmptyKeys returns empty results.
func TestDeleteObjects_EmptyKeys(t *testing.T) {
	t.Parallel()
	store := newPermissiveStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	results := mgr.DeleteObjects(context.Background(), []string{})
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

// TestDeleteObjects_BackendNotInMap tolerates an unknown backend.
func TestDeleteObjects_BackendNotInMap(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	objectsStubs(store)
	store.EXPECT().DeleteObjectsBatch(gomock.Any(), gomock.Any()).
		DoAndReturn(stubBatchDelete(func(keys []string) (map[string][]core.DeletedCopy, error) {
			out := make(map[string][]core.DeletedCopy, len(keys))
			for _, k := range keys {
				out[k] = []core.DeletedCopy{{BackendName: "ghost", SizeBytes: 1}}
			}
			return out, nil
		})).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	results := mgr.DeleteObjects(context.Background(), []string{"k1"})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Errorf("expected no error (missing backend is non-fatal), got %v", results[0].Err)
	}
}

// TestDeleteObject_RecordsOneAPICallPerCopy pins the single-DELETE-per-
// physical-DELETE rule for ObjectManager.DeleteObject. DeleteOrEnqueue
// owns the API-call tick, so an N-copy delete must record exactly N
// APICalls across the involved backends (not 2*N as it did before the
// duplicate-accounting fix). See issue #881.
func TestDeleteObject_RecordsOneAPICallPerCopy(t *testing.T) {
	t.Parallel()
	be1 := backendtest.NewInMemory()
	be2 := backendtest.NewInMemory()
	for _, b := range []*backendtest.InMemory{be1, be2} {
		_, _ = b.PutObject(context.Background(), "k", bytes.NewReader([]byte("rm")), 2, "", nil)
	}

	store := deleteObjectStore(t, []core.DeletedCopy{
		{BackendName: "b1", SizeBytes: 2},
		{BackendName: "b2", SizeBytes: 2},
	}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be1, "b2": be2}, nil)

	if err := mgr.DeleteObject(context.Background(), "k"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	if got := mgr.Runtime.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != 1 {
		t.Errorf("b1 apiRequests = %d, want 1", got)
	}
	if got := mgr.Runtime.Usage().Backend().Load("b2", counter.FieldAPIRequests); got != 1 {
		t.Errorf("b2 apiRequests = %d, want 1", got)
	}
}

// TestDeleteObjects_RecordsOneAPICallPerCopy pins the same rule for the
// batch path: N keys with one copy each must record N APICalls total
// (not 2*N). See issue #881.
func TestDeleteObjects_RecordsOneAPICallPerCopy(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	for _, k := range []string{"a", "b", "c"} {
		_, _ = be.PutObject(context.Background(), k, bytes.NewReader([]byte("x")), 1, "", nil)
	}

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	objectsStubs(store)
	store.EXPECT().DeleteObjectsBatch(gomock.Any(), gomock.Any()).
		DoAndReturn(stubBatchDelete(func(keys []string) (map[string][]core.DeletedCopy, error) {
			out := make(map[string][]core.DeletedCopy, len(keys))
			for _, k := range keys {
				out[k] = []core.DeletedCopy{{BackendName: "b1", SizeBytes: 1}}
			}
			return out, nil
		})).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	mgr.DeleteObjects(context.Background(), []string{"a", "b", "c"})

	if got := mgr.Runtime.Usage().Backend().Load("b1", counter.FieldAPIRequests); got != 3 {
		t.Errorf("b1 apiRequests = %d, want 3 (one per key, not 2*N)", got)
	}
}

// copyObjectStore wires a CopyObject success path: locations + a chosen
// destination backend. Placement and record calls land in one accumulator so a
// caller can assert on the row the copy wrote, not just on where it went.
func copyObjectStore(t *testing.T, locs []core.ObjectLocation, locsErr error) (*storetest.MockMetadataStore, *objectsCalls) {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	c := objectsStubs(store)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return(locs, locsErr).AnyTimes()
	storetest.Permissive(store)
	return store, c
}

// TestPutObject_IntegrityEnabled_PersistsContentHash drives the
// integrity-enabled branches: bufferPutBody allocates a SHA-256 hasher and
// describeStoredBytes builds a form to carry the resulting ContentHash on the
// unencrypted path, which is the only thing there is to say about bytes stored
// verbatim.
func TestPutObject_IntegrityEnabled_PersistsContentHash(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	store, c := putObjectStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	mgr.SetIntegrityConfig(&config.IntegrityConfig{Enabled: true})

	_, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "k", Body: bytes.NewReader([]byte("hello")), Size: 5, ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if len(c.recordObject) != 1 {
		t.Fatalf("expected 1 RecordObject call, got %d", len(c.recordObject))
	}
	form := c.recordObject[0].Form
	if form == nil {
		t.Fatal("integrity is on, so the row needs a form to carry the hash")
	}
	if form.ContentHash == "" {
		t.Error("form.ContentHash is empty")
	}
	if form.Encrypted {
		t.Error("form.Encrypted = true with no encryptor configured")
	}
}

// TestCopyObject_HeadSourceForCopy_SkipsUnknownBackend exercises the
// "be not in map" skip in headSourceForCopy: the first listed
// location points at a phantom be the proxy does not have, so
// the helper continues to the second (real) location. Without the
// skip the lookup would return ok=false and CopyObject would surface
// "failed to head source from any copy" even though a healthy replica
// exists.
func TestCopyObject_HeadSourceForCopy_SkipsUnknownBackend(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "src", bytes.NewReader([]byte("copy-me")), 7, "text/plain", nil)

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{
			{ObjectKey: "src", BackendName: "ghost"}, // unknown -> skip branch
			{ObjectKey: "src", BackendName: "b1"},    // real -> succeeds
		}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"}); err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	if !be.Has("dst") {
		t.Error("destination object not found after unknown-be skip")
	}
}

// TestCopyObject_DestBackendNotInMap surfaces the GetBackend error
// branch in CopyObject: SelectWriteTarget returns a backend name the
// orchestrator does not know about (config drift or test misuse), so
// GetBackend errors and the copy fails fast instead of nil-derefing
// TestCopyObject_RecordFailureSurfaces exercises the
// RecordObjectOrCleanup error branch: the destination PUT succeeds but
// the metadata commit fails. RecordObjectOrCleanup recovers the
// orphaned bytes; CopyObject must surface the error rather than
// reporting success.
func TestCopyObject_RecordFailureSurfaces(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "src", bytes.NewReader([]byte("copy-me")), 7, "text/plain", nil)

	c := &objectsCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}}, nil).AnyTimes()
	// RecordObject returns an error -> RecordObjectOrCleanup wraps + recovers + returns.
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjRecord(c, errors.New("commit failed"))).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjEnqueue(c)).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"}); err == nil {
		t.Fatal("expected error when destination record fails")
	}
}

// TestCopyObject_Success drives the happy path.
func TestCopyObject_Success(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "src", bytes.NewReader([]byte("copy-me")), 7, "text/plain", nil)

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	etag, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"})
	if err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if !be.Has("dst") {
		t.Error("destination object not found")
	}
	// Regression pin for #815: the body handed to the destination
	// PutObject must satisfy io.Seeker so the AWS SDK stays on the
	// non-streaming UNSIGNED-PAYLOAD path and preserves Content-Length.
	// A pipe-based body broke OCI with HTTP 411 MissingContentLength.
	if !be.LastPutBodySeekable {
		t.Error("PutObject body was not seekable; would break OCI with HTTP 411")
	}
}

// TestCopyObject_SameBackendFastPath_UsesNativeCopy verifies the
// same-be fast path: when the destination ends up on the same
// be that holds a source replica and the be implements
// Copier, the orchestrator calls native CopyObject once and
// skips the materialize-then-PUT round trip.
func TestCopyObject_SameBackendFastPath_UsesNativeCopy(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.CopyEnabled = true
	_, _ = be.PutObject(context.Background(), "src", bytes.NewReader([]byte("copy-me")), 7, "text/plain", nil)
	be.LastPutBodySeekable = false // reset so we can detect a no-PUT fast path

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	etag, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"})
	if err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if !be.Has("dst") {
		t.Error("destination object not found")
	}
	calls := be.CopyCallCount()
	puttedBody := be.PutBodyWasSeekable()
	if calls != 1 {
		t.Errorf("native copyCalls = %d, want 1", calls)
	}
	if puttedBody {
		t.Error("PutObject ran; fast path should have skipped materialize+PUT")
	}
}

// TestCopyObject_FastPathFallsBackOnNativeError verifies the fast path
// gracefully falls back to materialized copy when native CopyObject
// returns an error. The destination must still end up populated via
// the slow path.
func TestCopyObject_FastPathFallsBackOnNativeError(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.CopyEnabled = true
	be.CopyErr = errors.New("simulated native copy failure")
	_, _ = be.PutObject(context.Background(), "src", bytes.NewReader([]byte("copy-me")), 7, "text/plain", nil)
	be.LastPutBodySeekable = false

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"}); err != nil {
		t.Fatalf("CopyObject (fallback path): %v", err)
	}
	if !be.Has("dst") {
		t.Error("destination object not found after fallback")
	}
	puttedBody := be.PutBodyWasSeekable()
	if !puttedBody {
		t.Error("expected materialized PUT after native-copy fallback")
	}
}

// TestCopyObject_AmbiguousNativeFailure_HeadConfirmsTreatsAsSuccess
// pins the #884 contract: when native CopyObject returns a non-
// capability error but a HEAD probe shows the destination already
// exists with the expected size, the orchestrator treats the copy as
// successful without falling back to materialized copy. This guards
// the "be copied server-side, response was lost" race against
// duplicate work.
func TestCopyObject_AmbiguousNativeFailure_HeadConfirmsTreatsAsSuccess(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.CopyEnabled = true
	be.CopyErr = errors.New("simulated response timeout")
	_, _ = be.PutObject(context.Background(), "src", bytes.NewReader([]byte("copy-me")), 7, "text/plain", nil)
	// Simulate the ambiguous case: the be already populated the
	// destination server-side before the response was lost.
	_, _ = be.PutObject(context.Background(), "dst", bytes.NewReader([]byte("copy-me")), 7, "text/plain", nil)
	// Reset the seekable flag so a materialized PUT would flip it true.
	be.LastPutBodySeekable = false

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	etag, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"})
	if err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag from HEAD-probe recovery")
	}
	puttedBody := be.PutBodyWasSeekable()
	if puttedBody {
		t.Error("materialized PUT ran; HEAD probe should have suppressed the fallback")
	}
}

// TestCopyObject_AmbiguousNativeFailure_HeadMissingFallsBack pins the
// other side of the #884 contract: when native CopyObject errors and
// the HEAD probe shows the destination is absent, the orchestrator
// falls back to materialized copy. The destination must still end up
// populated.
func TestCopyObject_AmbiguousNativeFailure_HeadMissingFallsBack(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.CopyEnabled = true
	be.CopyErr = errors.New("simulated network error")
	_, _ = be.PutObject(context.Background(), "src", bytes.NewReader([]byte("copy-me")), 7, "text/plain", nil)
	// No dst pre-populated: the probe sees 404 and falls back.
	be.LastPutBodySeekable = false

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"}); err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	if !be.Has("dst") {
		t.Error("destination object not found after materialized fallback")
	}
	puttedBody := be.PutBodyWasSeekable()
	if !puttedBody {
		t.Error("expected materialized PUT after probe returned 404")
	}
}

// TestCopyObject_AmbiguousNativeFailure_SizeMismatchFallsBack pins the
// safety guard: when the HEAD probe shows the destination exists but
// at a different size than the source, the orchestrator falls back to
// materialized copy (which overwrites with the correct content).
// Without the size check, an unrelated object on the destination key
// could be misclassified as a successful copy.
func TestCopyObject_AmbiguousNativeFailure_SizeMismatchFallsBack(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.CopyEnabled = true
	be.CopyErr = errors.New("simulated ambiguous failure")
	_, _ = be.PutObject(context.Background(), "src", bytes.NewReader([]byte("copy-me")), 7, "text/plain", nil)
	// Pre-populate dst with a different size to simulate "something
	// else is already at this key."
	_, _ = be.PutObject(context.Background(), "dst", bytes.NewReader([]byte("different-content")), 17, "text/plain", nil)
	be.LastPutBodySeekable = false

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"}); err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	puttedBody := be.PutBodyWasSeekable()
	if !puttedBody {
		t.Error("expected materialized PUT after size-mismatch probe")
	}
}

// TestCopyObject_FastPathSkippedCrossBackend verifies the fast path is
// not engaged when the source's only replica lives on a different
// backend than the chosen destination. The orchestrator must
// materialize the object via GET-then-PUT.
func TestCopyObject_FastPathSkippedCrossBackend(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	src.CopyEnabled = true
	_, _ = src.PutObject(context.Background(), "src", bytes.NewReader([]byte("cross")), 5, "text/plain", nil)
	dst := backendtest.NewInMemory()
	dst.CopyEnabled = true // destination supports native copy, but source is elsewhere

	store, storeCalls := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}}, nil)
	// b1 holds the source and has no room, so its claim is declined, the copy
	// lands on b2, and the cross-backend path is the one under test.
	storeCalls.full = map[string]bool{"b1": true}
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": src, "b2": dst}, &fleetOpts{
		Order: []string{"b1", "b2"},
	})

	if _, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"}); err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	calls := dst.CopyCallCount()
	if calls != 0 {
		t.Errorf("native copyCalls = %d, want 0 (cross-backend must materialize)", calls)
	}
	if !dst.Has("dst") {
		t.Error("destination object not found after cross-backend copy")
	}
}

// TestCopyObject_SourceNotFound surfaces a not-found source.
func TestCopyObject_SourceNotFound(t *testing.T) {
	t.Parallel()
	store := locationsStore(t, nil, core.ErrObjectNotFound)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	if _, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "missing", DestKey: "dst"}); !errors.Is(err, core.ErrObjectNotFound) {
		t.Fatalf("expected st.ErrObjectNotFound, got %v", err)
	}
}

// TestCopyObject_DBUnavailable_SourceLookup surfaces a DB failure on
// the source lookup.
func TestCopyObject_DBUnavailable_SourceLookup(t *testing.T) {
	t.Parallel()
	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	if _, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"}); !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// TestCopyObject_NoRoomForTheDestination surfaces the refusal when the only
// backend cannot take the copy.
func TestCopyObject_NoRoomForTheDestination(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "src", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	store, copyCalls := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "b1", SizeBytes: 4}}, nil)
	copyCalls.full = map[string]bool{"b1": true}
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"}); !errors.Is(err, core.ErrInsufficientStorage) {
		t.Fatalf("expected st.ErrInsufficientStorage, got %v", err)
	}
}

// TestListObjects_Success drives the simple list happy path.
func TestListObjects_Success(t *testing.T) {
	t.Parallel()
	store := listObjectsStore(t, &core.ListObjectsResult{
		Objects: []core.ObjectLocation{
			{ObjectKey: "a/1", BackendName: "b1", SizeBytes: 10},
			{ObjectKey: "a/2", BackendName: "b1", SizeBytes: 20},
		},
	}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	result, err := mgr.ListObjects(context.Background(), "a/", "", "", 1000)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(result.Objects) != 2 {
		t.Errorf("got %d objects, want 2", len(result.Objects))
	}
	if result.KeyCount != 2 {
		t.Errorf("KeyCount = %d, want 2", result.KeyCount)
	}
}

// TestListObjects_WithDelimiter pins that the delimiter path maps the store's
// grouped page (CommonPrefixes + leaf objects) into the response. The grouping
// itself is exercised at the store layer (see the sqlite/integration tests).
func TestListObjects_WithDelimiter(t *testing.T) {
	t.Parallel()
	store := listObjectsDelimitedStore(t, &core.ListDelimitedResult{
		CommonPrefixes: []string{"photos/2024/", "photos/2025/"},
		Objects:        []core.ObjectLocation{{ObjectKey: "photos/top.jpg", BackendName: "b1"}},
	}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	result, err := mgr.ListObjects(context.Background(), "photos/", "/", "", 1000)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(result.Objects) != 1 {
		t.Errorf("got %d objects, want 1", len(result.Objects))
	}
	if len(result.CommonPrefixes) != 2 {
		t.Errorf("got %d common prefixes, want 2", len(result.CommonPrefixes))
	}
	if result.KeyCount != 3 {
		t.Errorf("KeyCount = %d, want 3", result.KeyCount)
	}
}

// TestListObjects_ExactPageTruncation pins the exact-page truncation.
func TestListObjects_ExactPageTruncation(t *testing.T) {
	t.Parallel()
	objs := make([]core.ObjectLocation, 3)
	for i := range objs {
		objs[i] = core.ObjectLocation{
			ObjectKey:   fmt.Sprintf("pfx/%03d", i),
			BackendName: "b1",
			SizeBytes:   100,
		}
	}
	store := listObjectsStore(t, &core.ListObjectsResult{Objects: objs, IsTruncated: true, NextContinuationToken: "pfx/002"}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	result, err := mgr.ListObjects(context.Background(), "pfx/", "", "", 3)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if result.KeyCount != 3 {
		t.Errorf("KeyCount = %d, want 3", result.KeyCount)
	}
	if !result.IsTruncated {
		t.Error("expected IsTruncated=true when store has more data")
	}
	if result.NextContinuationToken != "pfx/002" {
		t.Errorf("NextContinuationToken = %q, want %q", result.NextContinuationToken, "pfx/002")
	}
}

// TestListObjects_DBUnavailable surfaces the 503 mapping.
func TestListObjects_DBUnavailable(t *testing.T) {
	t.Parallel()
	store := listObjectsStore(t, nil, core.ErrDBUnavailable)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	_, err := mgr.ListObjects(context.Background(), "", "", "", 1000)
	if err == nil {
		t.Fatal("expected error")
	}
	s3err, ok := errors.AsType[*core.S3Error](err)
	if !ok {
		t.Fatalf("expected st.S3Error, got %T: %v", err, err)
	}
	if s3err.StatusCode != 503 {
		t.Errorf("StatusCode = %d, want 503", s3err.StatusCode)
	}
}

// TestCopyObject_BackendTimeout_SourceGetSlow pins #882: the
// materialized-copy slow path runs the source GET under the configured
// backend timeout. Before the fix the GET used the raw request context
// and a stalled source could exceed backend_timeout.
func TestCopyObject_BackendTimeout_SourceGetSlow(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "src", bytes.NewReader([]byte("copy-me")), 7, "text/plain", nil)
	slow := &slowMockBackend{InMemory: be, delay: 200 * time.Millisecond, delayGets: true}

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": slow}, &fleetOpts{Order: []string{"b1"}, BackendTimeout: 50 * time.Millisecond})

	_, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

// TestCopyObject_BackendTimeout_DestPutSlow pins #882: the materialized
// copy's destination PUT runs under backend_timeout. Cross-backend
// setup so the source GET completes fast and only the destination
// write hits the timeout.
func TestCopyObject_BackendTimeout_DestPutSlow(t *testing.T) {
	t.Parallel()
	srcBE := backendtest.NewInMemory()
	_, _ = srcBE.PutObject(context.Background(), "src", bytes.NewReader([]byte("copy-me")), 7, "text/plain", nil)
	dstBE := backendtest.NewInMemory()
	slowDst := &slowMockBackend{InMemory: dstBE, delay: 200 * time.Millisecond}

	store, copyCalls := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}}, nil)
	// destination under test.
	copyCalls.full = map[string]bool{"b1": true}
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": srcBE, "b2": slowDst}, &fleetOpts{
		Order:          []string{"b1", "b2"},
		BackendTimeout: 50 * time.Millisecond,
	})

	_, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

// TestPutObject_BackendTimeout pins the deadline-bound put.
func TestPutObject_BackendTimeout(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	slowBackend := &slowMockBackend{InMemory: be, delay: 200 * time.Millisecond}

	store, _ := putObjectStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": slowBackend}, &fleetOpts{Order: []string{"b1"}, BackendTimeout: 50 * time.Millisecond})

	_, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "timeout-key", Body: bytes.NewReader([]byte("data")), Size: 4, ContentType: "text/plain"})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

// slowMockBackend wraps the shared in-memory be with a delayed PutObject. When
// delayGets is true GetObject also waits `delay` before forwarding -
// used by the CopyObject source-GET timeout regression for #882.
type slowMockBackend struct {
	*backendtest.InMemory
	delay     time.Duration
	delayGets bool
}

// PutObject sleeps then forwards.
func (s *slowMockBackend) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string, metadata map[string]string) (string, error) {
	select {
	case <-time.After(s.delay):
		return s.InMemory.PutObject(ctx, key, body, size, contentType, metadata)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// GetObject optionally sleeps before forwarding so tests can exercise
// timeout enforcement on the source-read leg of CopyObject (#882).
func (s *slowMockBackend) GetObject(ctx context.Context, key, rng string) (*backend.GetObjectResult, error) {
	if !s.delayGets {
		return s.InMemory.GetObject(ctx, key, rng)
	}
	select {
	case <-time.After(s.delay):
		return s.InMemory.GetObject(ctx, key, rng)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestLocationCache_SetAndGet pins basic cache set/get.
func TestLocationCache_SetAndGet(t *testing.T) {
	t.Parallel()
	mgr := newFleet(t, newPermissiveStore(t), nil, &fleetOpts{})
	mgr.LocationCache().Set("key1", "backend-a")

	got, ok := mgr.LocationCache().Get("key1")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got != "backend-a" {
		t.Errorf("cached backend = %q, want %q", got, "backend-a")
	}
}

// TestLocationCache_Expiry pins TTL-based cache expiration.
func TestLocationCache_Expiry(t *testing.T) {
	t.Parallel()
	mgr := newFleet(t, newPermissiveStore(t), nil,
		&fleetOpts{CacheTTL: 10 * time.Millisecond})
	mgr.LocationCache().Set("key1", "backend-a")

	time.Sleep(15 * time.Millisecond)

	if _, ok := mgr.LocationCache().Get("key1"); ok {
		t.Fatal("expected cache miss after TTL")
	}
}

// TestLocationCache_Overwrite pins cache overwrites.
func TestLocationCache_Overwrite(t *testing.T) {
	t.Parallel()
	mgr := newFleet(t, newPermissiveStore(t), nil, &fleetOpts{})
	mgr.LocationCache().Set("key1", "old-backend")
	mgr.LocationCache().Set("key1", "new-backend")

	got, ok := mgr.LocationCache().Get("key1")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got != "new-backend" {
		t.Errorf("cached backend = %q, want %q", got, "new-backend")
	}
}

// TestPutObject_InvalidatesCache pins post-put cache invalidation.
func TestPutObject_InvalidatesCache(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	store, _ := putObjectStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	mgr.LocationCache().Set("mykey", "old-be")

	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "mykey", Body: bytes.NewReader([]byte("hello")), Size: 5, ContentType: "text/plain"}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	if _, ok := mgr.LocationCache().Get("mykey"); ok {
		t.Error("cache should be invalidated after PutObject")
	}
}

// TestDeleteObject_InvalidatesCache pins post-delete cache invalidation.
func TestDeleteObject_InvalidatesCache(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "del-key", bytes.NewReader([]byte("rm")), 2, "", nil)
	store := deleteObjectStore(t, []core.DeletedCopy{{BackendName: "b1", SizeBytes: 2}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	mgr.LocationCache().Set("del-key", "b1")

	if err := mgr.DeleteObject(context.Background(), "del-key"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	if _, ok := mgr.LocationCache().Get("del-key"); ok {
		t.Error("cache should be invalidated after DeleteObject")
	}
}

// TestPutObject_UsageLimitOverflow asserts the eligible-fallback branch.
func TestPutObject_UsageLimitOverflow(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()

	limits := map[string]core.UsageLimits{
		"b1": requestCapped(t, 10, 0, 0),
		"b2": requestCapped(t, 100, 0, 0),
	}
	store, _ := putObjectStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{UsageLimits: limits})

	alreadySpent(mgr, "b1", 10, 0, 0)

	etag, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "key", Body: bytes.NewReader([]byte("data")), Size: 4, ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("PutObject should overflow to b2: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if b1.Has("key") {
		t.Error("object should NOT be on b1 (over limit)")
	}
	if !b2.Has("key") {
		t.Error("object should be on b2 (overflow)")
	}
}

// TestGetObject_UsageLimitSkipsBackend asserts limit-driven failover on
// reads.
func TestGetObject_UsageLimitSkipsBackend(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key", bytes.NewReader([]byte("from-b1")), 7, "text/plain", nil)
	_, _ = b2.PutObject(context.Background(), "key", bytes.NewReader([]byte("from-b2")), 7, "text/plain", nil)

	limits := map[string]core.UsageLimits{
		"b1": requestCapped(t, 10, 0, 0),
		"b2": requestCapped(t, 100, 0, 0),
	}
	store := locationsStore(t, []core.ObjectLocation{
		{ObjectKey: "key", BackendName: "b1"},
		{ObjectKey: "key", BackendName: "b2"},
	}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{UsageLimits: limits})

	alreadySpent(mgr, "b1", 10, 0, 0)

	result, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("GetObject should skip b1 and use b2: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
	got, _ := io.ReadAll(result.Body)
	if string(got) != "from-b2" {
		t.Errorf("body = %q, want %q (from b2)", got, "from-b2")
	}
}

// TestGetObject_AllCopiesOverLimit surfaces the all-over-limit error.
func TestGetObject_AllCopiesOverLimit(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	limits := map[string]core.UsageLimits{
		"b1": requestCapped(t, 10, 0, 0),
	}
	store := locationsStore(t, []core.ObjectLocation{
		{ObjectKey: "key", BackendName: "b1"},
	}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1}, &fleetOpts{UsageLimits: limits})

	alreadySpent(mgr, "b1", 10, 0, 0)

	if _, err := mgr.GetObject(context.Background(), "key", ""); !errors.Is(err, core.ErrUsageLimitExceeded) {
		t.Fatalf("expected st.ErrUsageLimitExceeded, got %v", err)
	}
}

// TestDeleteObject_AlwaysAllowed asserts deletes ignore usage limits.
func TestDeleteObject_AlwaysAllowed(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	_, _ = be.PutObject(context.Background(), "del-key", bytes.NewReader([]byte("rm")), 2, "", nil)

	limits := map[string]core.UsageLimits{
		"b1": requestCapped(t, 1, 1, 1),
	}
	store := deleteObjectStore(t, []core.DeletedCopy{{BackendName: "b1", SizeBytes: 2}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, &fleetOpts{UsageLimits: limits})

	alreadySpent(mgr, "b1", 100, 100, 100)

	if err := mgr.DeleteObject(context.Background(), "del-key"); err != nil {
		t.Fatalf("DeleteObject should always succeed regardless of limits: %v", err)
	}
	if be.Has("del-key") {
		t.Error("object should be deleted from be")
	}
}

// TestPutObject_UsageLimitRejectionsMetric pins the rejection metric on
// writes.
func TestPutObject_UsageLimitRejectionsMetric(t *testing.T) {
	limits := map[string]core.UsageLimits{
		"b1": requestCapped(t, 10, 0, 0),
	}
	store, _ := putObjectStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, &fleetOpts{UsageLimits: limits})

	alreadySpent(mgr, "b1", 10, 0, 0)

	before := testutil.ToFloat64(telemetry.UsageLimitRejectionsTotal.WithLabelValues("PutObject", "write"))

	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{Key: "key", Body: bytes.NewReader([]byte("x")), Size: 1, ContentType: "text/plain"}); err == nil {
		t.Fatal("expected error from PutObject with all backends over limit")
	}

	after := testutil.ToFloat64(telemetry.UsageLimitRejectionsTotal.WithLabelValues("PutObject", "write"))
	if after <= before {
		t.Errorf("UsageLimitRejectionsTotal[PutObject,write] did not increment: before=%v, after=%v", before, after)
	}
}

// TestGetObject_UsageLimitRejectionsMetric pins the rejection metric
// on reads.
func TestGetObject_UsageLimitRejectionsMetric(t *testing.T) {
	b1 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	limits := map[string]core.UsageLimits{
		"b1": requestCapped(t, 10, 0, 0),
	}
	store := locationsStore(t, []core.ObjectLocation{
		{ObjectKey: "key", BackendName: "b1"},
	}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1}, &fleetOpts{UsageLimits: limits})

	alreadySpent(mgr, "b1", 10, 0, 0)

	before := testutil.ToFloat64(telemetry.UsageLimitRejectionsTotal.WithLabelValues("GetObject", "read"))

	if _, err := mgr.GetObject(context.Background(), "key", ""); !errors.Is(err, core.ErrUsageLimitExceeded) {
		t.Fatalf("expected st.ErrUsageLimitExceeded, got %v", err)
	}

	after := testutil.ToFloat64(telemetry.UsageLimitRejectionsTotal.WithLabelValues("GetObject", "read"))
	if after <= before {
		t.Errorf("UsageLimitRejectionsTotal[GetObject,read] did not increment: before=%v, after=%v", before, after)
	}
}

// newTestManagerParallel builds a fleet with parallel
// broadcast enabled and explicit ordering.
func newTestManagerParallel(t *testing.T, store storetest.MetadataStore, orderedBackends []struct {
	name    string
	backend backend.ObjectBackend
}) *fleet {
	t.Helper()
	obs := make(map[string]backend.ObjectBackend, len(orderedBackends))
	order := make([]string, 0, len(orderedBackends))
	for _, b := range orderedBackends {
		obs[b.name] = b.backend
		order = append(order, b.name)
	}
	mgr := newFleet(t, store, obs, &fleetOpts{Order: order, BackendTimeout: 30 * time.Second, ParallelBroadcast: true})
	return mgr
}

// slowGetBackend wraps the shared in-memory backend with delayed Get/Head.
type slowGetBackend struct {
	*backendtest.InMemory
	delay time.Duration
}

// GetObject sleeps then forwards.
func (s *slowGetBackend) GetObject(ctx context.Context, key string, rangeHeader string) (*backend.GetObjectResult, error) {
	select {
	case <-time.After(s.delay):
		return s.InMemory.GetObject(ctx, key, rangeHeader)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// HeadObject sleeps then forwards.
func (s *slowGetBackend) HeadObject(ctx context.Context, key string) (*backend.HeadObjectResult, error) {
	select {
	case <-time.After(s.delay):
		return s.InMemory.HeadObject(ctx, key)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestGetObject_ParallelBroadcast_FirstSuccessWins pins the parallel
// race-to-success behaviour.
func TestGetObject_ParallelBroadcast_FirstSuccessWins(t *testing.T) {
	t.Parallel()
	slow := backendtest.NewInMemory()
	fast := backendtest.NewInMemory()
	_, _ = slow.PutObject(context.Background(), "key", bytes.NewReader([]byte("slow-data")), 9, "text/plain", nil)
	_, _ = fast.PutObject(context.Background(), "key", bytes.NewReader([]byte("fast-data")), 9, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newTestManagerParallel(t, store, []struct {
		name    string
		backend backend.ObjectBackend
	}{
		{"slow", &slowGetBackend{InMemory: slow, delay: 200 * time.Millisecond}},
		{"fast", fast},
	})

	start := time.Now()
	result, err := mgr.GetObject(context.Background(), "key", "")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("parallel broadcast should succeed: %v", err)
	}
	defer func() { _ = result.Body.Close() }()

	got, _ := io.ReadAll(result.Body)
	if string(got) != "fast-data" {
		t.Errorf("body = %q, want %q (fast backend should win)", got, "fast-data")
	}
	if elapsed > 150*time.Millisecond {
		t.Errorf("parallel broadcast took %v, expected < 150ms", elapsed)
	}
}

// TestGetObject_ParallelBroadcast_AllFail surfaces the all-fail branch
// in parallel mode.
func TestGetObject_ParallelBroadcast_AllFail(t *testing.T) {
	t.Parallel()
	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newTestManagerParallel(t, store, []struct {
		name    string
		backend backend.ObjectBackend
	}{
		{"b1", backendtest.NewInMemory()},
		{"b2", backendtest.NewInMemory()},
	})

	_, err := mgr.GetObject(context.Background(), "nowhere", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, core.ErrObjectNotFound) {
		t.Fatal("should not mask backend errors as st.ErrObjectNotFound")
	}
}

// TestGetObject_ParallelBroadcast_CacheHitSkipsParallel pins the
// cache-hit-after-broadcast branch.
func TestGetObject_ParallelBroadcast_CacheHitSkipsParallel(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	_, _ = b2.PutObject(context.Background(), "cached-key", bytes.NewReader([]byte("cached")), 6, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newTestManagerParallel(t, store, []struct {
		name    string
		backend backend.ObjectBackend
	}{
		{"b1", b1},
		{"b2", b2},
	})

	r1, err := mgr.GetObject(context.Background(), "cached-key", "")
	if err != nil {
		t.Fatalf("first GetObject: %v", err)
	}
	_ = r1.Body.Close()

	r2, err := mgr.GetObject(context.Background(), "cached-key", "")
	if err != nil {
		t.Fatalf("second GetObject (cache hit): %v", err)
	}
	defer func() { _ = r2.Body.Close() }()
	got, _ := io.ReadAll(r2.Body)
	if string(got) != "cached" {
		t.Errorf("body = %q, want %q", got, "cached")
	}
}

// TestGetObject_SequentialBroadcast_WhenDisabled pins the
// disabled-parallel branch.
func TestGetObject_SequentialBroadcast_WhenDisabled(t *testing.T) {
	t.Parallel()
	slow := backendtest.NewInMemory()
	fast := backendtest.NewInMemory()
	_, _ = slow.PutObject(context.Background(), "key", bytes.NewReader([]byte("slow-data")), 9, "text/plain", nil)
	_, _ = fast.PutObject(context.Background(), "key", bytes.NewReader([]byte("fast-data")), 9, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	obs := map[string]backend.ObjectBackend{
		"slow": &slowGetBackend{InMemory: slow, delay: 100 * time.Millisecond},
		"fast": fast,
	}
	mgr := newFleet(t, store, obs, &fleetOpts{Order: []string{"slow", "fast"}, BackendTimeout: 30 * time.Second})

	start := time.Now()
	result, err := mgr.GetObject(context.Background(), "key", "")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("sequential broadcast should succeed: %v", err)
	}
	defer func() { _ = result.Body.Close() }()

	got, _ := io.ReadAll(result.Body)
	if string(got) != "slow-data" {
		t.Errorf("body = %q, want %q (slow backend tried first sequentially)", got, "slow-data")
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("sequential broadcast took %v, expected >= 100ms", elapsed)
	}
}

// concurrencyTrackingBackend wraps the shared in-memory backend and tracks the high
// watermark of concurrent GetObject calls so a test can assert that the
// degraded broadcast respects its parallelism cap. Used by
// TestGetObject_DegradedBroadcastCap_RespectsLimit.
// The counters are shared across every wrapper backed by the same tracker, so
// the watermark reflects total cross-backend concurrency rather than
// per-backend reentrancy.
type concurrencyTrackingBackend struct {
	*backendtest.InMemory
	delay       time.Duration
	inFlight    *atomic.Int32
	maxInFlight *atomic.Int32
}

// GetObject increments the shared in-flight counter, naps for delay (so
// the test has a window to observe overlap), then forwards to the
// underlying mock.
func (c *concurrencyTrackingBackend) GetObject(ctx context.Context, key string, rangeHeader string) (*backend.GetObjectResult, error) {
	now := c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	for {
		peak := c.maxInFlight.Load()
		if now <= peak || c.maxInFlight.CompareAndSwap(peak, now) {
			break
		}
	}
	select {
	case <-time.After(c.delay):
		return c.InMemory.GetObject(ctx, key, rangeHeader)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestGetObject_DegradedBroadcastCap_RespectsLimit pins issue #858: when
// a positive DegradedBroadcastParallelism cap is set, the parallel
// degraded broadcast probes at most that many backends concurrently
// even if more eligible backends are configured. The slow-probe backend
// pool guarantees that without the cap every backend would be probed at
// once, so a max-in-flight watermark of 2 is only possible if the
// rolling-window launcher is honouring the limit.
func TestGetObject_DegradedBroadcastCap_RespectsLimit(t *testing.T) {
	t.Parallel()

	const probeDelay = 80 * time.Millisecond
	var inFlight, maxInFlight atomic.Int32
	names := []string{"b1", "b2", "b3", "b4", "b5"}
	obs := make(map[string]backend.ObjectBackend, len(names))
	for _, n := range names {
		mb := backendtest.NewInMemory()
		_, _ = mb.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
		obs[n] = &concurrencyTrackingBackend{
			InMemory:    mb,
			delay:       probeDelay,
			inFlight:    &inFlight,
			maxInFlight: &maxInFlight,
		}
	}

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newFleet(t, store, obs, &fleetOpts{Order: names, BackendTimeout: 30 * time.Second, ParallelBroadcast: true, DegradedBroadcastParallelism: 2})

	result, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
	_, _ = io.ReadAll(result.Body)

	if peak := maxInFlight.Load(); peak > 2 {
		t.Errorf("maxInFlight = %d, want <= 2 (cap should bound concurrent probes)", peak)
	}
}

// TestGetObject_DegradedBroadcastCap_ReplenishesAfterFailure exercises
// the rolling-window backfill: with cap=1 and the first two backends
// returning errors, the third backend must still be probed and win.
// Pins that launchNext fires inside the failure branch of the receive
// loop.
func TestGetObject_DegradedBroadcastCap_ReplenishesAfterFailure(t *testing.T) {
	t.Parallel()

	b1 := backendtest.NewInMemory()
	b1.GetErr = errors.New("b1 down")
	b2 := backendtest.NewInMemory()
	b2.GetErr = errors.New("b2 down")
	b3 := backendtest.NewInMemory()
	_, _ = b3.PutObject(context.Background(), "key", bytes.NewReader([]byte("ok")), 2, "text/plain", nil)

	obs := map[string]backend.ObjectBackend{"b1": b1, "b2": b2, "b3": b3}
	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newFleet(t, store, obs, &fleetOpts{Order: []string{"b1", "b2", "b3"}, BackendTimeout: 30 * time.Second, ParallelBroadcast: true, DegradedBroadcastParallelism: 1})

	result, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
	got, _ := io.ReadAll(result.Body)
	if string(got) != "ok" {
		t.Errorf("body = %q, want %q (b3 should win after b1+b2 fail)", got, "ok")
	}
}

// TestGetObject_BackendNotFound_FailsOverToNext pins the missing-backend
// failover.
func TestGetObject_BackendNotFound_FailsOverToNext(t *testing.T) {
	t.Parallel()
	b2 := backendtest.NewInMemory()
	_, _ = b2.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	store := locationsStore(t, []core.ObjectLocation{
		{ObjectKey: "key", BackendName: "gone-backend"},
		{ObjectKey: "key", BackendName: "b2"},
	}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b2": b2}, nil)

	result, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("GetObject should failover past missing backend: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
	got, _ := io.ReadAll(result.Body)
	if string(got) != "data" {
		t.Errorf("body = %q, want %q", got, "data")
	}
}

// TestGetObject_GenericStoreError surfaces a non-typed DB error.
func TestGetObject_GenericStoreError(t *testing.T) {
	t.Parallel()
	store := locationsStore(t, nil, errors.New("unexpected db error"))
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	_, err := mgr.GetObject(context.Background(), "key", "")
	if err == nil {
		t.Fatal("expected error from generic store failure")
	}
	if errors.Is(err, core.ErrObjectNotFound) {
		t.Error("should not be st.ErrObjectNotFound")
	}
}

// TestGetObject_DBUnavailable_CacheHitFails_FallsThrough pins
// fall-through after a stale cache hit.
func TestGetObject_DBUnavailable_CacheHitFails_FallsThrough(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b2 := backendtest.NewInMemory()
	_, _ = b2.PutObject(context.Background(), "key", bytes.NewReader([]byte("from-b2")), 7, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, nil)

	mgr.LocationCache().Set("key", "b1")

	result, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("should fall through to broadcast after cache hit failure: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
	got, _ := io.ReadAll(result.Body)
	if string(got) != "from-b2" {
		t.Errorf("body = %q, want %q", got, "from-b2")
	}
}

// TestDeleteObject_BackendNotFound_ContinuesOtherCopies pins partial
// success when one copy lives on a missing backend.
func TestDeleteObject_BackendNotFound_ContinuesOtherCopies(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	_, _ = b1.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "", nil)

	store := deleteObjectStore(t, []core.DeletedCopy{
		{BackendName: "gone-backend", SizeBytes: 4},
		{BackendName: "b1", SizeBytes: 4},
	}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1}, nil)

	if err := mgr.DeleteObject(context.Background(), "key"); err != nil {
		t.Fatalf("DeleteObject should succeed even with missing backend: %v", err)
	}
	if b1.Has("key") {
		t.Error("expected b1 copy to be deleted")
	}
}

// TestCopyObject_AllSourceHeadsFail surfaces an all-heads-fail error.
func TestCopyObject_AllSourceHeadsFail(t *testing.T) {
	t.Parallel()
	b1 := backendtest.NewInMemory()
	b1.HeadErr = errors.New("head failed")

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1}, nil)

	if _, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"}); err == nil {
		t.Fatal("expected error when all source HeadObjects fail")
	}
}

// TestCopyObject_DestWriteFails surfaces a dst write failure.
func TestCopyObject_DestWriteFails(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "src", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dst := backendtest.NewInMemory()
	dst.PutErr = errors.New("write failed")

	store, copyCalls := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "src-be"}}, nil)
	// destination whose write fails.
	copyCalls.full = map[string]bool{"src-be": true}
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"src-be": src, "dst-be": dst}, &fleetOpts{
		Order:          []string{"src-be", "dst-be"},
		BackendTimeout: 30 * time.Second,
	})

	if _, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"}); err == nil {
		t.Fatal("expected error when dest PutObject fails")
	}
}

// TestCopyObject_ExcludesDrainingBackend asserts draining backends are
// excluded from copy targets.
func TestCopyObject_ExcludesDrainingBackend(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "src", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dst := backendtest.NewInMemory()

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "src-be"}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"src-be": src, "dst-be": dst},
		&fleetOpts{
			Order:    []string{"src-be", "dst-be"},
			Draining: []string{"src-be", "dst-be"},
		})

	if _, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"}); !errors.Is(err, core.ErrInsufficientStorage) {
		t.Fatalf("expected st.ErrInsufficientStorage when all backends are draining, got %v", err)
	}
	if dst.Has("dst") {
		t.Error("object should not have been copied to draining backend")
	}
}

// TestCopyObject_SourceReadFails surfaces a source body-read failure.
func TestCopyObject_SourceReadFails(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "src", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	src.GetReadErr = errors.New("disk I/O error")

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "src-be"}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"src-be": src, "dst-be": backendtest.NewInMemory()}, &fleetOpts{Order: []string{"src-be", "dst-be"}, BackendTimeout: 30 * time.Second})

	if _, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"}); err == nil {
		t.Fatal("expected error when source body read fails")
	}
}

// TestCopyObject_AllSourceGetObjectsFail surfaces an all-Get-fail error.
func TestCopyObject_AllSourceGetObjectsFail(t *testing.T) {
	t.Parallel()
	src := backendtest.NewInMemory()
	_, _ = src.PutObject(context.Background(), "src", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	src.GetErr = errors.New("get unavailable")

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "src-be"}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"src-be": src, "dst-be": backendtest.NewInMemory()}, &fleetOpts{Order: []string{"src-be", "dst-be"}, BackendTimeout: 30 * time.Second})

	if _, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"}); err == nil {
		t.Fatal("expected error when all source GetObjects fail")
	}
}

// TestListObjects_GenericError surfaces a non-typed list error.
func TestListObjects_GenericError(t *testing.T) {
	t.Parallel()
	store := listObjectsStore(t, nil, errors.New("unexpected query error"))
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	_, err := mgr.ListObjects(context.Background(), "", "", "", 1000)
	if err == nil {
		t.Fatal("expected error from generic store failure")
	}
	if s3err, ok := errors.AsType[*core.S3Error](err); ok {
		t.Errorf("generic error should not be st.S3Error, got %+v", s3err)
	}
}

// TestHeadObject_ParallelBroadcast pins parallel HeadObject behaviour.
func TestHeadObject_ParallelBroadcast(t *testing.T) {
	t.Parallel()
	slow := backendtest.NewInMemory()
	fast := backendtest.NewInMemory()
	_, _ = slow.PutObject(context.Background(), "key", bytes.NewReader([]byte("head")), 4, "text/plain", nil)
	_, _ = fast.PutObject(context.Background(), "key", bytes.NewReader([]byte("head")), 4, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newTestManagerParallel(t, store, []struct {
		name    string
		backend backend.ObjectBackend
	}{
		{"slow", &slowGetBackend{InMemory: slow, delay: 200 * time.Millisecond}},
		{"fast", fast},
	})

	result, err := mgr.HeadObject(context.Background(), "key")
	if err != nil {
		t.Fatalf("HeadObject parallel broadcast should succeed: %v", err)
	}
	if result.Size != 4 {
		t.Errorf("size = %d, want 4", result.Size)
	}
}

// TestParsePlaintextRange_SuffixLargerThanFile pins the suffix clamp.
func TestParsePlaintextRange_SuffixLargerThanFile(t *testing.T) {
	t.Parallel()
	start, end, ok := ParsePlaintextRange("bytes=-1000", 100)
	if !ok {
		t.Fatal("expected ok=true for valid suffix range")
	}
	if start != 0 {
		t.Errorf("start = %d, want 0 (clamped)", start)
	}
	if end != 99 {
		t.Errorf("end = %d, want 99", end)
	}
}

// TestParsePlaintextRange_ClampsEndToSize pins the end clamp.
func TestParsePlaintextRange_ClampsEndToSize(t *testing.T) {
	t.Parallel()
	start, end, ok := ParsePlaintextRange("bytes=0-200", 100)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if start != 0 {
		t.Errorf("start = %d, want 0", start)
	}
	if end != 99 {
		t.Errorf("end = %d, want 99 (clamped to plaintextSize-1)", end)
	}
}

// TestParsePlaintextRange_ExactEndNotClamped pins exact-fit ranges.
func TestParsePlaintextRange_ExactEndNotClamped(t *testing.T) {
	t.Parallel()
	start, end, ok := ParsePlaintextRange("bytes=0-99", 100)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if start != 0 || end != 99 {
		t.Errorf("start=%d end=%d, want 0,99", start, end)
	}
}

// TestParsePlaintextRange_InvertedRange rejects invalid ranges.
func TestParsePlaintextRange_InvertedRange(t *testing.T) {
	t.Parallel()
	_, _, ok := ParsePlaintextRange("bytes=99-0", 100)
	if ok {
		t.Error("expected ok=false for inverted range")
	}
}

// TestParsePlaintextRange_StartBeyondFile rejects start-past-file.
func TestParsePlaintextRange_StartBeyondFile(t *testing.T) {
	t.Parallel()
	_, _, ok := ParsePlaintextRange("bytes=100-200", 100)
	if ok {
		t.Error("expected ok=false when start >= plaintextSize")
	}
}

// TestParsePlaintextRange_OpenEndedBeyondFile rejects open-ended past
// end of file.
func TestParsePlaintextRange_OpenEndedBeyondFile(t *testing.T) {
	t.Parallel()
	_, _, ok := ParsePlaintextRange("bytes=100-", 100)
	if ok {
		t.Error("expected ok=false for open-ended range beyond file")
	}
}

// TestCopyObject_SourceGetPanics surfaces a panic in the source-reader
// goroutine as an error.
func TestCopyObject_SourceGetPanics(t *testing.T) {
	t.Parallel()
	srcBackend := backendtest.NewInMemory()
	srcBackend.GetPanic = true

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": srcBackend}, nil)

	if _, err := mgr.CopyObject(context.Background(), &CopyObjectRequest{SourceKey: "src", DestKey: "dst"}); err == nil {
		t.Fatal("expected error from panicking source reader, got nil")
	}
}

// TestGetObject_EmptyEncryptedObject proves a zero-byte object survives the
// read path when encryption is on. An empty plaintext encrypts to a bare
// 32-byte header with no chunks, so its row carries plaintext_size 0 over a
// non-zero stored size - the one legitimate shape that looks, at a glance,
// like a row that lost its plaintext size.
func TestGetObject_EmptyEncryptedObject(t *testing.T) {
	t.Parallel()
	provider, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 64*1024)
	if err != nil {
		t.Fatal(err)
	}

	res, err := enc.Encrypt(context.Background(), bytes.NewReader(nil), 0)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ciphertext, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}

	b1 := backendtest.NewInMemory()
	b1.Objects["empty"] = backendtest.Object{Data: ciphertext}

	store := locationsStore(t, []core.ObjectLocation{{
		ObjectKey:     "empty",
		BackendName:   "b1",
		SizeBytes:     int64(len(ciphertext)),
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(res.BaseNonce, res.WrappedDEK),
		KeyID:         res.KeyID,
		PlaintextSize: 0,
	}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1},
		&fleetOpts{Order: []string{"b1"}, BackendTimeout: 30 * time.Second, Encryptor: enc})

	result, err := mgr.GetObject(context.Background(), "empty", "")
	if err != nil {
		t.Fatalf("GetObject on an empty encrypted object: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
	got, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("body = %q, want empty", got)
	}

	head, err := mgr.HeadObject(context.Background(), "empty")
	if err != nil {
		t.Fatalf("HeadObject on an empty encrypted object: %v", err)
	}
	if head.Size != 0 {
		t.Errorf("HeadObject size = %d, want 0", head.Size)
	}
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// encryptedObjectFleet builds a fleet holding one encrypted object with the
// given plaintext, returning the manager and the recorded location row.
func encryptedObjectFleet(t *testing.T, key, plaintext string) *fleet {
	t.Helper()
	provider, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	res, err := enc.Encrypt(context.Background(), strings.NewReader(plaintext), int64(len(plaintext)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ciphertext, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}

	b1 := backendtest.NewInMemory()
	b1.Objects[key] = backendtest.Object{Data: ciphertext}
	store := locationsStore(t, []core.ObjectLocation{{
		ObjectKey:     key,
		BackendName:   "b1",
		SizeBytes:     int64(len(ciphertext)),
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(res.BaseNonce, res.WrappedDEK),
		KeyID:         res.KeyID,
		PlaintextSize: int64(len(plaintext)),
	}}, nil)
	return newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1},
		&fleetOpts{Order: []string{"b1"}, BackendTimeout: 30 * time.Second, Encryptor: enc})
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestGetObject_SuffixRangeOnEmptyEncryptedObject verifies an unsatisfiable
// range surfaces as InvalidRange instead of being silently swapped for an
// untranslated plaintext range against ciphertext.
func TestGetObject_SuffixRangeOnEmptyEncryptedObject(t *testing.T) {
	t.Parallel()
	mgr := encryptedObjectFleet(t, "empty", "")

	_, err := mgr.GetObject(context.Background(), "empty", "bytes=-5")
	if err == nil {
		t.Fatal("a suffix range against a zero-length object must not succeed")
	}
	s3err, ok := errors.AsType[*core.S3Error](err)
	if !ok {
		t.Fatalf("error must carry an S3 status, got %v", err)
	}
	if s3err.StatusCode != 416 || s3err.Code != "InvalidRange" {
		t.Errorf("got %d %s, want 416 InvalidRange", s3err.StatusCode, s3err.Code)
	}
}

// TestGetObject_UnparseableRangeOnEncryptedObject verifies a Range the server
// cannot act on falls back to the whole object rather than shipping plaintext
// offsets to a backend holding ciphertext.
func TestGetObject_UnparseableRangeOnEncryptedObject(t *testing.T) {
	t.Parallel()
	mgr := encryptedObjectFleet(t, "obj", "hello world")

	result, err := mgr.GetObject(context.Background(), "obj", "items=0-4")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
	got, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("body = %q, want the whole object", got)
	}
}

// TestGetObject_RangeOnEncryptedObject verifies the ordinary translated range
// still returns exactly the requested plaintext window.
func TestGetObject_RangeOnEncryptedObject(t *testing.T) {
	t.Parallel()
	mgr := encryptedObjectFleet(t, "obj", "hello world")

	result, err := mgr.GetObject(context.Background(), "obj", "bytes=6-10")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
	got, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != "world" {
		t.Errorf("body = %q, want %q", got, "world")
	}
}
