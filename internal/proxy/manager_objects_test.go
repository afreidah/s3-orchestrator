// -------------------------------------------------------------------------------
// Object Operations Tests
//
// Author: Alex Freidah
//
// Tests for BackendManager object CRUD: PutObject routing and quota enforcement,
// GetObject failover across replicas, HeadObject, DeleteObject broadcast, and
// CopyObject. Uses mock backends and stores to verify routing strategy behavior.
// -------------------------------------------------------------------------------

package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newPermissiveMock returns a gomock-driven MockMetadataStore wired to
// the supplied test's controller with storetest.Permissive defaults so
// callers that do not need any specific stub behaviour can drop it
// straight into newTestManager.
func newPermissiveMock(t *testing.T) *storetest.MockMetadataStore {
	t.Helper()
	m := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(m)
	return m
}

// newTestManager creates a BackendManager with mock backends and store for testing.
func newTestManager(store core.MetadataStore, backends map[string]*mockBackend) *BackendManager {
	obs := make(map[string]s3be.ObjectBackend, len(backends))
	var order []string
	for name, b := range backends {
		obs[name] = b
		order = append(order, name)
	}
	return wireWorkersForTest(NewBackendManager(&BackendManagerConfig{
		Backends:        obs,
		Stores:          testStoresFromMock(store),
		PendingEnabled:  true,
		Dashboard:       store,
		Metrics:         store,
		Order:           order,
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	}))
}

// objectsCalls holds the per-test capture state for assertions.
type objectsCalls struct {
	mu                       sync.Mutex
	recordObject             []objRecordCall
	insertPending            []core.PendingObject
	enqueueCleanup           []core.CleanupItem
	getBackendWithSpaceCalls atomic.Int64
	getLeastUtilizedCalls    atomic.Int64
}

type objRecordCall struct {
	Key, Backend string
	Size         int64
}

func stubObjGetBackend(c *objectsCalls, resp string, err error) func(context.Context, int64, []string) (string, error) {
	return func(_ context.Context, _ int64, _ []string) (string, error) {
		c.getBackendWithSpaceCalls.Add(1)
		return resp, err
	}
}

func stubObjGetBackendEligible(c *objectsCalls) func(context.Context, int64, []string) (string, error) {
	return func(_ context.Context, _ int64, eligible []string) (string, error) {
		c.getBackendWithSpaceCalls.Add(1)
		if len(eligible) > 0 {
			return eligible[0], nil
		}
		return "", core.ErrNoSpaceAvailable
	}
}

func stubObjGetLeastUtilized(c *objectsCalls, resp string, err error) func(context.Context, int64, []string) (string, error) {
	return func(_ context.Context, _ int64, _ []string) (string, error) {
		c.getLeastUtilizedCalls.Add(1)
		return resp, err
	}
}

func stubObjRecord(c *objectsCalls, err error) func(context.Context, string, string, int64, *core.EncryptionMeta) ([]core.DeletedCopy, error) {
	return func(_ context.Context, key, backend string, size int64, _ *core.EncryptionMeta) ([]core.DeletedCopy, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.recordObject = append(c.recordObject, objRecordCall{Key: key, Backend: backend, Size: size})
		return nil, err
	}
}

func stubObjRecordAndClear(c *objectsCalls, err error) func(context.Context, string, string, int64, *core.EncryptionMeta, string) ([]core.DeletedCopy, error) {
	return func(_ context.Context, key, backend string, size int64, _ *core.EncryptionMeta, _ string) ([]core.DeletedCopy, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.recordObject = append(c.recordObject, objRecordCall{Key: key, Backend: backend, Size: size})
		return nil, err
	}
}

func stubObjInsertPending(c *objectsCalls, err error) func(context.Context, *core.PendingObject) error {
	return func(_ context.Context, p *core.PendingObject) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.insertPending = append(c.insertPending, *p)
		return err
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
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjRecord(c, nil)).AnyTimes()
	store.EXPECT().RecordObjectAndClearPending(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjRecordAndClear(c, nil)).AnyTimes()
	store.EXPECT().InsertPending(gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjInsertPending(c, nil)).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjEnqueue(c)).AnyTimes()
	return c
}

// putObjectStore returns a store wired for a successful PutObject path.
func putObjectStore(t *testing.T, getBackendResp string) (*storetest.MockMetadataStore, *objectsCalls) {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	c := objectsStubs(store)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjGetBackend(c, getBackendResp, nil)).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjGetLeastUtilized(c, getBackendResp, nil)).AnyTimes()
	storetest.Permissive(store)
	return store, c
}

// putObjectErrStore returns a store where GetBackendWithSpace + GetLeastUtilizedBackend fail.
func putObjectErrStore(t *testing.T, err error) *storetest.MockMetadataStore {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	objectsStubs(store)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).Return("", err).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).Return("", err).AnyTimes()
	storetest.Permissive(store)
	return store
}

// eligibleStore returns a store where GetBackendWithSpace returns the
// first eligible candidate.
func eligibleStore(t *testing.T) (*storetest.MockMetadataStore, *objectsCalls) {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	c := objectsStubs(store)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjGetBackendEligible(c)).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjGetBackendEligible(c)).AnyTimes()
	storetest.Permissive(store)
	return store, c
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

// listObjectsPaged hands out paginated ListObjects results in order.
func listObjectsPaged(t *testing.T, pages []core.ListObjectsResult) *storetest.MockMetadataStore {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	objectsStubs(store)
	idx := 0
	store.EXPECT().ListObjects(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, _ int) (*core.ListObjectsResult, error) {
			if idx >= len(pages) {
				return &core.ListObjectsResult{}, nil
			}
			page := pages[idx]
			idx++
			return &page, nil
		}).AnyTimes()
	storetest.Permissive(store)
	return store
}

// deleteObjectStore wires a DeleteObject response.
func deleteObjectStore(t *testing.T, resp []core.DeletedCopy, err error) *storetest.MockMetadataStore {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	objectsStubs(store)
	store.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).Return(resp, err).AnyTimes()
	storetest.Permissive(store)
	return store
}

// TestPutObject_Success drives the happy path.
func TestPutObject_Success(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store, c := putObjectStore(t, "b1")
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	etag, err := mgr.ObjectManager.PutObject(context.Background(), "mykey", bytes.NewReader([]byte("hello")), 5, "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if !backend.hasObject("mykey") {
		t.Error("object not found on backend")
	}
	if len(c.recordObject) != 1 {
		t.Fatalf("expected 1 RecordObject call, got %d", len(c.recordObject))
	}
	call := c.recordObject[0]
	if call.Key != "mykey" || call.Backend != "b1" || call.Size != 5 {
		t.Errorf("RecordObject called with %+v", call)
	}
}

// TestPutObject_PackStrategy_UsesGetBackendWithSpace pins the pack
// strategy routing.
func TestPutObject_PackStrategy_UsesGetBackendWithSpace(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store, c := putObjectStore(t, "b1")
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"b1": backend},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	wireWorkersForTest(mgr)

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "pack-key", bytes.NewReader([]byte("data")), 4, "text/plain", nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if c.getBackendWithSpaceCalls.Load() != 1 {
		t.Errorf("expected 1 GetBackendWithSpace call, got %d", c.getBackendWithSpaceCalls.Load())
	}
	if c.getLeastUtilizedCalls.Load() != 0 {
		t.Errorf("expected 0 GetLeastUtilizedBackend calls, got %d", c.getLeastUtilizedCalls.Load())
	}
}

// TestPutObject_SpreadStrategy_UsesGetLeastUtilized pins the spread
// strategy routing.
func TestPutObject_SpreadStrategy_UsesGetLeastUtilized(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store, c := putObjectStore(t, "b1")
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"b1": backend},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingSpread,
	})
	wireWorkersForTest(mgr)

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "spread-key", bytes.NewReader([]byte("data")), 4, "text/plain", nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if c.getLeastUtilizedCalls.Load() != 1 {
		t.Errorf("expected 1 GetLeastUtilizedBackend call, got %d", c.getLeastUtilizedCalls.Load())
	}
	if c.getBackendWithSpaceCalls.Load() != 0 {
		t.Errorf("expected 0 GetBackendWithSpace calls, got %d", c.getBackendWithSpaceCalls.Load())
	}
}

// TestCanAcceptWrite_HasCapacity asserts the positive-capacity branch.
func TestCanAcceptWrite_HasCapacity(t *testing.T) {
	t.Parallel()
	store, _ := putObjectStore(t, "b1")
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	if !mgr.ObjectManager.CanAcceptWrite(100) {
		t.Error("CanAcceptWrite should return true when backend has capacity")
	}
}

// TestCanAcceptWrite_NoCapacity asserts the over-limit branch.
func TestCanAcceptWrite_NoCapacity(t *testing.T) {
	t.Parallel()
	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 1},
	}
	mgr := newTestManagerWithLimits(newPermissiveMock(t), map[string]*mockBackend{"b1": newMockBackend()}, limits)

	mgr.ObjectManager.usage.SetBaseline("b1", core.UsageStat{APIRequests: 1})

	if mgr.ObjectManager.CanAcceptWrite(100) {
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

	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	got := mgr.ObjectManager.BackendCapacityStats(context.Background())
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

	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	if got := mgr.ObjectManager.BackendCapacityStats(context.Background()); got != nil {
		t.Errorf("BackendCapacityStats on DB failure = %+v, want nil", got)
	}
}

// TestPutObject_QuotaExhausted surfaces the no-space branch.
func TestPutObject_QuotaExhausted(t *testing.T) {
	t.Parallel()
	store := putObjectErrStore(t, core.ErrNoSpaceAvailable)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("x")), 1, "", nil); !errors.Is(err, core.ErrInsufficientStorage) {
		t.Fatalf("expected st.ErrInsufficientStorage, got %v", err)
	}
}

// TestPutObject_DBUnavailable surfaces the DB-down branch.
func TestPutObject_DBUnavailable(t *testing.T) {
	t.Parallel()
	store := putObjectErrStore(t, core.ErrDBUnavailable)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("x")), 1, "", nil); !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// TestPutObject_BackendFailure_StillRecordsUsage pins API-call counting
// on backend failures.
func TestPutObject_BackendFailure_StillRecordsUsage(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.putErr = errors.New("backend timeout")
	store, _ := putObjectStore(t, "b1")
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil); err == nil {
		t.Fatal("expected error from backend failure")
	}
	if got := mgr.usage.Backend().Load("b1", counter.FieldAPIRequests); got != 1 {
		t.Errorf("apiRequests = %d, want 1 (failed call still counts)", got)
	}
	if got := mgr.usage.Backend().Load("b1", counter.FieldIngressBytes); got != 0 {
		t.Errorf("ingressBytes = %d, want 0 (upload failed)", got)
	}
}

// TestPutObject_RecordFailure_LeavesBackendBytesAndPendingIntent pins
// the pending-row pattern: backend bytes survive a metadata commit
// failure.
func TestPutObject_RecordFailure_LeavesBackendBytesAndPendingIntent(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	c := &objectsCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjGetBackend(c, "b1", nil)).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjGetLeastUtilized(c, "b1", nil)).AnyTimes()
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjRecord(c, errors.New("db write failed"))).AnyTimes()
	store.EXPECT().RecordObjectAndClearPending(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjRecordAndClear(c, errors.New("db write failed"))).AnyTimes()
	store.EXPECT().InsertPending(gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjInsertPending(c, nil)).AnyTimes()
	store.EXPECT().EnqueueCleanup(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjEnqueue(c)).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "cleanup-key", bytes.NewReader([]byte("data")), 4, "", nil); err == nil {
		t.Fatal("expected error from RecordObjectAndClearPending failure")
	}
	if !backend.hasObject("cleanup-key") {
		t.Error("backend bytes should be retained for the pending reaper to resolve")
	}
	if len(c.insertPending) != 1 {
		t.Fatalf("expected 1 InsertPending call, got %d", len(c.insertPending))
	}
	if c.insertPending[0].ObjectKey != "cleanup-key" || c.insertPending[0].BackendName != "b1" {
		t.Errorf("InsertPending called with %+v", c.insertPending[0])
	}
	if got := mgr.usage.Backend().Load("b1", counter.FieldAPIRequests); got != 1 {
		t.Errorf("apiRequests = %d, want 1 (PUT only)", got)
	}
}

// TestPutObject_RecordFailure_LegacyPath asserts that with the pending
// store nil, the legacy delete-on-failure path runs.
func TestPutObject_RecordFailure_LegacyPath(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	c := &objectsCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjGetBackend(c, "b1", nil)).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjGetLeastUtilized(c, "b1", nil)).AnyTimes()
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjRecord(c, errors.New("db write failed"))).AnyTimes()
	store.EXPECT().InsertPending(gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjInsertPending(c, nil)).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})
	mgr.pendingEnabled = false

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "legacy-key", bytes.NewReader([]byte("data")), 4, "", nil); err == nil {
		t.Fatal("expected error from RecordObject failure")
	}
	if backend.hasObject("legacy-key") {
		t.Error("legacy path should delete the orphan from the backend")
	}
	if len(c.insertPending) != 0 {
		t.Errorf("legacy path should not insert pending intents, got %d", len(c.insertPending))
	}
}

// errReader is an io.Reader that always returns the configured error.
type errReader struct{ err error }

// Read returns the configured error.
func (r *errReader) Read([]byte) (int, error) { return 0, r.err }

// newTestManagerWithOrder creates a BackendManager with explicit order.
func newTestManagerWithOrder(store core.MetadataStore, backends map[string]*mockBackend, order []string) *BackendManager {
	obs := make(map[string]s3be.ObjectBackend, len(backends))
	for name, b := range backends {
		obs[name] = b
	}
	return wireWorkersForTest(NewBackendManager(&BackendManagerConfig{
		Backends:        obs,
		Stores:          testStoresFromMock(store),
		PendingEnabled:  true,
		Dashboard:       store,
		Metrics:         store,
		Order:           order,
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	}))
}

// TestPutObject_WriteFailover_Success pins the failover happy path.
func TestPutObject_WriteFailover_Success(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.putErr = errors.New("connection refused")
	b2 := newMockBackend()

	store, c := eligibleStore(t)
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": b1, "b2": b2}, []string{"b1", "b2"})

	etag, err := mgr.ObjectManager.PutObject(context.Background(), "failover-key", bytes.NewReader([]byte("hello")), 5, "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject should succeed via failover: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if b1.hasObject("failover-key") {
		t.Error("object should NOT be on failed backend b1")
	}
	if !b2.hasObject("failover-key") {
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
	b1 := newMockBackend()
	b1.putErr = errors.New("b1 down")
	b2 := newMockBackend()
	b2.putErr = errors.New("b2 down")
	b3 := newMockBackend()
	b3.putErr = errors.New("b3 down")

	store, _ := eligibleStore(t)
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": b1, "b2": b2, "b3": b3}, []string{"b1", "b2", "b3"})

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil); err == nil {
		t.Fatal("expected error when all backends fail")
	}
	total := mgr.usage.Backend().Load("b1", counter.FieldAPIRequests) +
		mgr.usage.Backend().Load("b2", counter.FieldAPIRequests) +
		mgr.usage.Backend().Load("b3", counter.FieldAPIRequests)
	if total != 3 {
		t.Errorf("total API requests = %d, want 3 (one per failed backend)", total)
	}
}

// TestPutObject_WriteFailover_SkipsMultipleFailedBackends pins the
// retry-many-then-succeed branch.
func TestPutObject_WriteFailover_SkipsMultipleFailedBackends(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.putErr = errors.New("b1 down")
	b2 := newMockBackend()
	b2.putErr = errors.New("b2 down")
	b3 := newMockBackend()

	store, c := eligibleStore(t)
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": b1, "b2": b2, "b3": b3}, []string{"b1", "b2", "b3"})

	etag, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject should succeed on b3: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if !b3.hasObject("key") {
		t.Error("object should be on b3")
	}
	if c.getBackendWithSpaceCalls.Load() != 3 {
		t.Errorf("GetBackendWithSpace calls = %d, want 3", c.getBackendWithSpaceCalls.Load())
	}
}

// TestPutObject_WriteFailover_Metrics pins the failover-metric increment.
func TestPutObject_WriteFailover_Metrics(t *testing.T) {
	telemetry.WriteFailoverTotal.Reset()

	b1 := newMockBackend()
	b1.putErr = errors.New("b1 down")
	b2 := newMockBackend()

	store, _ := eligibleStore(t)
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": b1, "b2": b2}, []string{"b1", "b2"})

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil); err != nil {
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
	b1 := newMockBackend()
	b1.putErr = errors.New("b1 timeout")
	b2 := newMockBackend()

	store, _ := eligibleStore(t)
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": b1, "b2": b2}, []string{"b1", "b2"})

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	if got := mgr.usage.Backend().Load("b1", counter.FieldAPIRequests); got != 1 {
		t.Errorf("b1 apiRequests = %d, want 1", got)
	}
	if got := mgr.usage.Backend().Load("b1", counter.FieldIngressBytes); got != 0 {
		t.Errorf("b1 ingressBytes = %d, want 0", got)
	}
	if got := mgr.usage.Backend().Load("b2", counter.FieldAPIRequests); got != 1 {
		t.Errorf("b2 apiRequests = %d, want 1", got)
	}
	if got := mgr.usage.Backend().Load("b2", counter.FieldIngressBytes); got != 4 {
		t.Errorf("b2 ingressBytes = %d, want 4", got)
	}
}

// TestPutObject_WriteFailover_DataIntegrity asserts the failed-over
// payload survives intact.
func TestPutObject_WriteFailover_DataIntegrity(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.putErr = errors.New("b1 down")
	b2 := newMockBackend()

	store, _ := eligibleStore(t)
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": b1, "b2": b2}, []string{"b1", "b2"})

	payload := []byte("the quick brown fox jumps over the lazy dog")
	if _, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader(payload), int64(len(payload)), "text/plain", map[string]string{"x-custom": "value"}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	b2.mu.Lock()
	obj := b2.objects["key"]
	b2.mu.Unlock()

	if !bytes.Equal(obj.data, payload) {
		t.Errorf("data mismatch: got %d bytes, want %d bytes", len(obj.data), len(payload))
	}
	if obj.contentType != "text/plain" {
		t.Errorf("contentType = %s, want text/plain", obj.contentType)
	}
	if obj.metadata["x-custom"] != "value" {
		t.Errorf("metadata[x-custom] = %s, want value", obj.metadata["x-custom"])
	}
}

// TestPutObject_WriteFailover_BufferBodyError surfaces the body-buffer
// failure path.
func TestPutObject_WriteFailover_BufferBodyError(t *testing.T) {
	t.Parallel()
	store, _ := eligibleStore(t)
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": newMockBackend()}, []string{"b1"})

	_, err := mgr.ObjectManager.PutObject(context.Background(), "key", &errReader{err: errors.New("read failed")}, 4, "text/plain", nil)
	if err == nil {
		t.Fatal("expected error from body buffer failure")
	}
	if got := err.Error(); got != "buffer request body: read failed" {
		t.Errorf("error = %q, want %q", got, "buffer request body: read failed")
	}
}

// TestPutObject_WriteFailover_SelectBackendErrorDuringRetry exercises a
// second-call DB failure during failover retry.
func TestPutObject_WriteFailover_SelectBackendErrorDuringRetry(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.putErr = errors.New("b1 down")

	c := &objectsCalls{}
	callCount := 0
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ int64, eligible []string) (string, error) {
			callCount++
			if callCount == 1 {
				return eligible[0], nil
			}
			return "", core.ErrDBUnavailable
		}).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ int64, eligible []string) (string, error) {
			callCount++
			if callCount == 1 {
				return eligible[0], nil
			}
			return "", core.ErrDBUnavailable
		}).AnyTimes()
	objectsStubs(store)
	storetest.Permissive(store)
	_ = c

	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": b1, "b2": newMockBackend()}, []string{"b1", "b2"})

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil); !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// TestPutObject_WriteFailover_BackendNotInMap rejects an unknown
// backend.
func TestPutObject_WriteFailover_BackendNotInMap(t *testing.T) {
	t.Parallel()
	store, _ := putObjectStore(t, "ghost")
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": newMockBackend()}, []string{"b1"})

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil); err == nil {
		t.Fatal("expected error when backend not in map")
	}
}

// TestPutObject_WriteFailover_WithEncryption pins the encryption-aware
// failover path.
func TestPutObject_WriteFailover_WithEncryption(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.putErr = errors.New("b1 down")
	b2 := newMockBackend()

	provider, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 64*1024)
	if err != nil {
		t.Fatal(err)
	}

	store, c := eligibleStore(t)
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"b1": b1, "b2": b2},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1", "b2"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
		Encryptor:       enc,
	})
	wireWorkersForTest(mgr)

	payload := []byte("encrypt-failover-test-data")
	etag, err := mgr.ObjectManager.PutObject(context.Background(), "enc-key", bytes.NewReader(payload), int64(len(payload)), "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject with encryption failover: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if b1.hasObject("enc-key") {
		t.Error("object should NOT be on failed backend b1")
	}
	if !b2.hasObject("enc-key") {
		t.Error("object should be on failover backend b2")
	}
	if len(c.recordObject) != 1 {
		t.Fatalf("expected 1 RecordObject call, got %d", len(c.recordObject))
	}
	if c.recordObject[0].Backend != "b2" {
		t.Errorf("RecordObject backend = %s, want b2", c.recordObject[0].Backend)
	}

	b2.mu.Lock()
	ciphertextLen := len(b2.objects["enc-key"].data)
	b2.mu.Unlock()
	if ciphertextLen <= len(payload) {
		t.Errorf("ciphertext len %d should be > plaintext len %d", ciphertextLen, len(payload))
	}
}

// TestGetObject_WithEncryption_UsesLocationMap exercises the
// location-map build path.
func TestGetObject_WithEncryption_UsesLocationMap(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
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
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"b1": b1},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
		Encryptor:       enc,
	})
	wireWorkersForTest(mgr)
	defer mgr.Close()

	result, err := mgr.ObjectManager.GetObject(context.Background(), "enc-key", "")
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
	b1 := newMockBackend()

	provider, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 64*1024)
	if err != nil {
		t.Fatal(err)
	}

	c := &objectsCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{
			{ObjectKey: "enc-key", BackendName: "b1", SizeBytes: 100, Encrypted: true, PlaintextSize: 25},
		}, nil).AnyTimes()
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjGetBackend(c, "b1", nil)).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjGetLeastUtilized(c, "b1", nil)).AnyTimes()
	objectsStubs(store)
	storetest.Permissive(store)

	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"b1": b1},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
		Encryptor:       enc,
	})
	wireWorkersForTest(mgr)
	defer mgr.Close()

	payload := []byte("head-encryption-test-data")
	if _, err = mgr.ObjectManager.PutObject(context.Background(), "enc-key", bytes.NewReader(payload), int64(len(payload)), "text/plain", nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	head, err := mgr.ObjectManager.HeadObject(context.Background(), "enc-key")
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

	b1 := newMockBackend()
	b2 := newMockBackend()

	store, _ := eligibleStore(t)
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": b1, "b2": b2}, []string{"b1", "b2"})

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil); err != nil {
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
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "key", bytes.NewReader([]byte("hello")), 5, "text/plain", nil)

	store := locationsStore(t, []core.ObjectLocation{{ObjectKey: "key", BackendName: "b1"}}, nil)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	result, err := mgr.ObjectManager.GetObject(context.Background(), "key", "")
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
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, err := mgr.ObjectManager.GetObject(context.Background(), "missing", ""); !errors.Is(err, core.ErrObjectNotFound) {
		t.Fatalf("expected st.ErrObjectNotFound, got %v", err)
	}
}

// TestGetObject_FailoverToReplica pins the replica failover path.
func TestGetObject_FailoverToReplica(t *testing.T) {
	t.Parallel()
	primary := newMockBackend()
	primary.getErr = errors.New("backend down")
	replica := newMockBackend()
	_, _ = replica.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	store := locationsStore(t, []core.ObjectLocation{
		{ObjectKey: "key", BackendName: "primary"},
		{ObjectKey: "key", BackendName: "replica"},
	}, nil)
	mgr := newTestManager(store, map[string]*mockBackend{"primary": primary, "replica": replica})

	result, err := mgr.ObjectManager.GetObject(context.Background(), "key", "")
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
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "key", bytes.NewReader([]byte("broadcast")), 9, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	result, err := mgr.ObjectManager.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("GetObject broadcast should succeed: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
	got, _ := io.ReadAll(result.Body)
	if string(got) != "broadcast" {
		t.Errorf("body = %q, want %q", got, "broadcast")
	}
}

// TestGetObject_DBUnavailable_CacheHit asserts the cache hit branch
// after a successful broadcast.
func TestGetObject_DBUnavailable_CacheHit(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	_, _ = b2.PutObject(context.Background(), "cached-key", bytes.NewReader([]byte("cached")), 6, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": b1, "b2": b2})

	r1, err := mgr.ObjectManager.GetObject(context.Background(), "cached-key", "")
	if err != nil {
		t.Fatalf("first GetObject: %v", err)
	}
	_ = r1.Body.Close()

	r2, err := mgr.ObjectManager.GetObject(context.Background(), "cached-key", "")
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
	b1 := newMockBackend()
	b2 := newMockBackend()

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": b1, "b2": b2})

	_, err := mgr.ObjectManager.GetObject(context.Background(), "nowhere", "")
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
	b1 := newMockBackend()
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
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"b1": b1},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
		Encryptor:       enc,
	})
	wireWorkersForTest(mgr)
	defer mgr.Close()

	_, err = mgr.ObjectManager.GetObject(context.Background(), "enc-key", "")
	if err == nil {
		t.Fatal("expected error for encrypted read with DB unavailable")
	}
	var s3err *core.S3Error
	if !errors.As(err, &s3err) || s3err.StatusCode != 503 {
		t.Errorf("expected 503 S3Error, got: %v", err)
	}
}

// TestHeadObject_Success drives the HeadObject happy path.
func TestHeadObject_Success(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "key", bytes.NewReader([]byte("headme")), 6, "application/json", nil)

	store := locationsStore(t, []core.ObjectLocation{{ObjectKey: "key", BackendName: "b1"}}, nil)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	result, err := mgr.ObjectManager.HeadObject(context.Background(), "key")
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
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "key", bytes.NewReader([]byte("head")), 4, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	result, err := mgr.ObjectManager.HeadObject(context.Background(), "key")
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
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "del-key", bytes.NewReader([]byte("rm")), 2, "", nil)

	store := deleteObjectStore(t, []core.DeletedCopy{{BackendName: "b1", SizeBytes: 2}}, nil)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	if err := mgr.ObjectManager.DeleteObject(context.Background(), "del-key"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if backend.hasObject("del-key") {
		t.Error("object should be deleted from backend")
	}
}

// TestDeleteObject_NotFound_Idempotent asserts the not-found
// idempotent branch.
func TestDeleteObject_NotFound_Idempotent(t *testing.T) {
	t.Parallel()
	store := deleteObjectStore(t, nil, core.ErrObjectNotFound)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	if err := mgr.ObjectManager.DeleteObject(context.Background(), "nonexistent"); err != nil {
		t.Fatalf("DeleteObject of nonexistent key should succeed (idempotent): %v", err)
	}
}

// TestDeleteObject_DBUnavailable surfaces the DB-down branch.
func TestDeleteObject_DBUnavailable(t *testing.T) {
	t.Parallel()
	store := deleteObjectStore(t, nil, core.ErrDBUnavailable)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	if err := mgr.ObjectManager.DeleteObject(context.Background(), "key"); !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// stubBatchDelete returns a DoAndReturn that drives DeleteObjectsBatch.
func stubBatchDelete(fn func(keys []string) (map[string][]core.DeletedCopy, error)) func(context.Context, []string) (map[string][]core.DeletedCopy, error) {
	return func(_ context.Context, keys []string) (map[string][]core.DeletedCopy, error) {
		return fn(keys)
	}
}

// TestDeleteObjects_AllSuccess pins the per-key all-success path.
func TestDeleteObjects_AllSuccess(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	for _, k := range []string{"a", "b", "c"} {
		_, _ = backend.PutObject(context.Background(), k, bytes.NewReader([]byte("x")), 1, "", nil)
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

	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	results := mgr.ObjectManager.DeleteObjects(context.Background(), []string{"a", "b", "c"})
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("results[%d]: unexpected error: %v", i, r.Err)
		}
	}
	for _, k := range []string{"a", "b", "c"} {
		if backend.hasObject(k) {
			t.Errorf("object %q should be deleted from backend", k)
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
		Return(nil, errors.New("db error")).AnyTimes()
	storetest.Permissive(store)

	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	results := mgr.ObjectManager.DeleteObjects(context.Background(), []string{"k1", "k2", "k3"})
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
	store := newPermissiveMock(t)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	results := mgr.ObjectManager.DeleteObjects(context.Background(), []string{"gone1", "gone2"})

	for i, r := range results {
		if r.Err != nil {
			t.Errorf("results[%d]: not-found should be success, got %v", i, r.Err)
		}
	}
}

// TestDeleteObjects_BackendFailureEnqueuesCleanup pins that backend
// failures during batch delete enqueue cleanup rows.
func TestDeleteObjects_BackendFailureEnqueuesCleanup(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.delErr = errors.New("backend down")

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

	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	results := mgr.ObjectManager.DeleteObjects(context.Background(), []string{"k1", "k2"})

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
	store := newPermissiveMock(t)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	results := mgr.ObjectManager.DeleteObjects(context.Background(), []string{})
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

	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	results := mgr.ObjectManager.DeleteObjects(context.Background(), []string{"k1"})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Errorf("expected no error (missing backend is non-fatal), got %v", results[0].Err)
	}
}

// copyObjectStore wires a CopyObject success path: locations + a chosen
// destination backend.
func copyObjectStore(t *testing.T, locs []core.ObjectLocation, locsErr error, getBackend string, getBackendErr error) (*storetest.MockMetadataStore, *objectsCalls) {
	t.Helper()
	c := &objectsCalls{}
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return(locs, locsErr).AnyTimes()
	store.EXPECT().GetBackendWithSpace(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjGetBackend(c, getBackend, getBackendErr)).AnyTimes()
	store.EXPECT().GetLeastUtilizedBackend(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(stubObjGetLeastUtilized(c, getBackend, getBackendErr)).AnyTimes()
	objectsStubs(store)
	storetest.Permissive(store)
	return store, c
}

// TestCopyObject_Success drives the happy path.
func TestCopyObject_Success(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "src", bytes.NewReader([]byte("copy-me")), 7, "text/plain", nil)

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}}, nil,
		"b1", nil)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	etag, err := mgr.ObjectManager.CopyObject(context.Background(), "src", "dst")
	if err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if !backend.hasObject("dst") {
		t.Error("destination object not found")
	}
}

// TestCopyObject_SourceNotFound surfaces a not-found source.
func TestCopyObject_SourceNotFound(t *testing.T) {
	t.Parallel()
	store := locationsStore(t, nil, core.ErrObjectNotFound)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, err := mgr.ObjectManager.CopyObject(context.Background(), "missing", "dst"); !errors.Is(err, core.ErrObjectNotFound) {
		t.Fatalf("expected st.ErrObjectNotFound, got %v", err)
	}
}

// TestCopyObject_DBUnavailable_SourceLookup surfaces a DB failure on
// the source lookup.
func TestCopyObject_DBUnavailable_SourceLookup(t *testing.T) {
	t.Parallel()
	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	if _, err := mgr.ObjectManager.CopyObject(context.Background(), "src", "dst"); !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// TestCopyObject_DBUnavailable_DestLookup surfaces a DB failure on the
// destination lookup.
func TestCopyObject_DBUnavailable_DestLookup(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "src", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}}, nil,
		"", core.ErrDBUnavailable)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	if _, err := mgr.ObjectManager.CopyObject(context.Background(), "src", "dst"); !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
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
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	result, err := mgr.ObjectManager.ListObjects(context.Background(), "a/", "", "", 1000)
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

// TestListObjects_WithDelimiter pins the delimiter-based grouping.
func TestListObjects_WithDelimiter(t *testing.T) {
	t.Parallel()
	store := listObjectsStore(t, &core.ListObjectsResult{
		Objects: []core.ObjectLocation{
			{ObjectKey: "photos/2024/a.jpg", BackendName: "b1"},
			{ObjectKey: "photos/2024/b.jpg", BackendName: "b1"},
			{ObjectKey: "photos/2025/c.jpg", BackendName: "b1"},
			{ObjectKey: "photos/top.jpg", BackendName: "b1"},
		},
	}, nil)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	result, err := mgr.ObjectManager.ListObjects(context.Background(), "photos/", "/", "", 1000)
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

// TestListObjects_DelimiterPagination pins the multi-page delimiter
// behaviour.
func TestListObjects_DelimiterPagination(t *testing.T) {
	t.Parallel()
	store := listObjectsPaged(t, []core.ListObjectsResult{
		{
			Objects: []core.ObjectLocation{
				{ObjectKey: "dir/a/1", BackendName: "b1"},
				{ObjectKey: "dir/a/2", BackendName: "b1"},
				{ObjectKey: "dir/a/3", BackendName: "b1"},
			},
			IsTruncated: true,
		},
		{
			Objects: []core.ObjectLocation{
				{ObjectKey: "dir/b/1", BackendName: "b1"},
				{ObjectKey: "dir/b/2", BackendName: "b1"},
			},
			IsTruncated: true,
		},
		{
			Objects: []core.ObjectLocation{
				{ObjectKey: "dir/c/1", BackendName: "b1"},
				{ObjectKey: "dir/top.txt", BackendName: "b1"},
			},
			IsTruncated: false,
		},
	})
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	result, err := mgr.ObjectManager.ListObjects(context.Background(), "dir/", "/", "", 3)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if result.KeyCount != 3 {
		t.Errorf("KeyCount = %d, want 3 (full page)", result.KeyCount)
	}
	if len(result.CommonPrefixes) != 3 {
		t.Errorf("CommonPrefixes = %v, want 3 entries", result.CommonPrefixes)
	}
	if !result.IsTruncated {
		t.Error("expected IsTruncated=true since dir/top.txt remains")
	}
}

// TestListObjects_DelimiterDedup pins prefix dedup across pages.
func TestListObjects_DelimiterDedup(t *testing.T) {
	t.Parallel()
	store := listObjectsPaged(t, []core.ListObjectsResult{
		{
			Objects: []core.ObjectLocation{
				{ObjectKey: "p/a/1", BackendName: "b1"},
				{ObjectKey: "p/a/2", BackendName: "b1"},
			},
			IsTruncated: true,
		},
		{
			Objects: []core.ObjectLocation{
				{ObjectKey: "p/a/3", BackendName: "b1"},
				{ObjectKey: "p/b/1", BackendName: "b1"},
			},
			IsTruncated: false,
		},
	})
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	result, err := mgr.ObjectManager.ListObjects(context.Background(), "p/", "/", "", 1000)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(result.CommonPrefixes) != 2 {
		t.Errorf("CommonPrefixes = %v, want [p/a/ p/b/]", result.CommonPrefixes)
	}
	if result.KeyCount != 2 {
		t.Errorf("KeyCount = %d, want 2", result.KeyCount)
	}
}

// TestListObjects_DelimiterTruncationSkipsSeen pins the cross-call
// dedup advance.
func TestListObjects_DelimiterTruncationSkipsSeen(t *testing.T) {
	t.Parallel()
	store := listObjectsStore(t, &core.ListObjectsResult{
		Objects: []core.ObjectLocation{
			{ObjectKey: "a/1", BackendName: "b1"},
			{ObjectKey: "a/2", BackendName: "b1"},
			{ObjectKey: "a/3", BackendName: "b1"},
			{ObjectKey: "b/1", BackendName: "b1"},
		},
		IsTruncated: false,
	}, nil)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	result, err := mgr.ObjectManager.ListObjects(context.Background(), "", "/", "", 1)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if result.KeyCount != 1 {
		t.Errorf("KeyCount = %d, want 1", result.KeyCount)
	}
	if len(result.CommonPrefixes) != 1 || result.CommonPrefixes[0] != "a/" {
		t.Errorf("CommonPrefixes = %v, want [a/]", result.CommonPrefixes)
	}
	if !result.IsTruncated {
		t.Fatal("expected IsTruncated=true")
	}
	if result.NextContinuationToken != "a/3" {
		t.Errorf("NextContinuationToken = %q, want %q", result.NextContinuationToken, "a/3")
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
	store := listObjectsStore(t, &core.ListObjectsResult{Objects: objs, IsTruncated: true}, nil)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	result, err := mgr.ObjectManager.ListObjects(context.Background(), "pfx/", "", "", 3)
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

// TestAdvancePastEmittedCommonPrefix_TableDriven covers the helper.
func TestAdvancePastEmittedCommonPrefix_TableDriven(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		prefix    string
		delimiter string
		cursor    string
		seen      map[string]bool
		want      string
	}{
		{name: "empty delimiter returns cursor unchanged", cursor: "tenant/a/k1", delimiter: "", want: "tenant/a/k1"},
		{name: "empty cursor returns cursor unchanged", delimiter: "/", cursor: "", want: ""},
		{name: "cursor not under prefix returns unchanged", prefix: "users/", delimiter: "/", cursor: "other/x", seen: map[string]bool{"users/0010/": true}, want: "other/x"},
		{name: "no delimiter in cursor's tail returns unchanged", prefix: "users/", delimiter: "/", cursor: "users/standalone-key", seen: map[string]bool{}, want: "users/standalone-key"},
		{name: "cursor inside un-emitted CP returns unchanged", prefix: "users/", delimiter: "/", cursor: "users/0010/k1", seen: map[string]bool{}, want: "users/0010/k1"},
		{name: "cursor inside emitted CP advances past group", prefix: "users/", delimiter: "/", cursor: "users/0010/k99", seen: map[string]bool{"users/0010/": true}, want: "users/00100"},
		{name: "multi-byte delimiter advances correctly", prefix: "u-", delimiter: "--", cursor: "u-0010--k1", seen: map[string]bool{"u-0010--": true}, want: "u-0010-."},
		{name: "0xff last byte cannot advance, returns cursor unchanged", prefix: "p", delimiter: "\xff", cursor: "p\xffk", seen: map[string]bool{"p\xff": true}, want: "p\xffk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := advancePastEmittedCommonPrefix(tc.prefix, tc.delimiter, tc.cursor, tc.seen)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestListObjects_PageBoundaryMidCommonPrefix is the regression test
// for the mid-group cursor rewrite.
func TestListObjects_PageBoundaryMidCommonPrefix(t *testing.T) {
	t.Parallel()
	store := listObjectsPaged(t, []core.ListObjectsResult{
		{
			Objects: []core.ObjectLocation{
				{ObjectKey: "a/1", BackendName: "b1"},
				{ObjectKey: "a/2", BackendName: "b1"},
			},
			IsTruncated: true,
		},
		{
			Objects: []core.ObjectLocation{
				{ObjectKey: "b/1", BackendName: "b1"},
				{ObjectKey: "b/2", BackendName: "b1"},
				{ObjectKey: "b/3", BackendName: "b1"},
			},
			IsTruncated: true,
		},
	})
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	result, err := mgr.ObjectManager.ListObjects(context.Background(), "", "/", "", 2)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(result.CommonPrefixes) != 2 || result.CommonPrefixes[0] != "a/" || result.CommonPrefixes[1] != "b/" {
		t.Errorf("CommonPrefixes = %v, want [a/ b/]", result.CommonPrefixes)
	}
	if !result.IsTruncated {
		t.Error("IsTruncated = false, want true (b/ group still has data)")
	}
	if result.NextContinuationToken != "b0" {
		t.Errorf("NextContinuationToken = %q, want %q (advanced past b/ group)", result.NextContinuationToken, "b0")
	}
}

// TestListObjects_MaxPagesCapMidCommonPrefix exercises the maxPages
// cap branch.
func TestListObjects_MaxPagesCapMidCommonPrefix(t *testing.T) {
	originalCap := listObjectsMaxPages
	listObjectsMaxPages = 2
	defer func() { listObjectsMaxPages = originalCap }()

	store := listObjectsPaged(t, []core.ListObjectsResult{
		{
			Objects: []core.ObjectLocation{
				{ObjectKey: "users/0001/k01", BackendName: "b1"},
				{ObjectKey: "users/0001/k02", BackendName: "b1"},
			},
			IsTruncated: true,
		},
		{
			Objects: []core.ObjectLocation{
				{ObjectKey: "users/0001/k03", BackendName: "b1"},
				{ObjectKey: "users/0001/k04", BackendName: "b1"},
			},
			IsTruncated: true,
		},
	})
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	result, err := mgr.ObjectManager.ListObjects(context.Background(), "users/", "/", "", 1000)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(result.CommonPrefixes) != 1 || result.CommonPrefixes[0] != "users/0001/" {
		t.Errorf("CommonPrefixes = %v, want [users/0001/]", result.CommonPrefixes)
	}
	if !result.IsTruncated {
		t.Error("IsTruncated = false, want true (maxPages cap with more data)")
	}
	if result.NextContinuationToken != "users/00010" {
		t.Errorf("NextContinuationToken = %q, want %q (advanced past users/0001/ group)", result.NextContinuationToken, "users/00010")
	}
}

// TestListObjects_CrossCallWalkDoesNotDuplicateCommonPrefix simulates
// paginating across calls.
func TestListObjects_CrossCallWalkDoesNotDuplicateCommonPrefix(t *testing.T) {
	t.Parallel()
	store := listObjectsPaged(t, []core.ListObjectsResult{
		{
			Objects: []core.ObjectLocation{
				{ObjectKey: "a/1", BackendName: "b1"},
				{ObjectKey: "a/2", BackendName: "b1"},
			},
			IsTruncated: true,
		},
		{
			Objects: []core.ObjectLocation{
				{ObjectKey: "b/1", BackendName: "b1"},
				{ObjectKey: "b/2", BackendName: "b1"},
				{ObjectKey: "b/3", BackendName: "b1"},
			},
			IsTruncated: true,
		},
		{
			Objects: []core.ObjectLocation{
				{ObjectKey: "c/1", BackendName: "b1"},
				{ObjectKey: "d/1", BackendName: "b1"},
			},
			IsTruncated: false,
		},
	})
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	first, err := mgr.ObjectManager.ListObjects(context.Background(), "", "/", "", 2)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if first.NextContinuationToken == "" {
		t.Fatal("first call returned empty token; cannot walk")
	}

	second, err := mgr.ObjectManager.ListObjects(context.Background(), "", "/", first.NextContinuationToken, 1000)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	combined := append([]string{}, first.CommonPrefixes...)
	combined = append(combined, second.CommonPrefixes...)
	seen := map[string]bool{}
	for _, cp := range combined {
		if seen[cp] {
			t.Errorf("CommonPrefix %q emitted twice across paginated calls", cp)
		}
		seen[cp] = true
	}
	for _, cp := range second.CommonPrefixes {
		if cp == "b/" {
			t.Error("second call re-emitted b/ - cross-call dedup broken")
		}
	}
}

// TestListObjects_DBUnavailable surfaces the 503 mapping.
func TestListObjects_DBUnavailable(t *testing.T) {
	t.Parallel()
	store := listObjectsStore(t, nil, core.ErrDBUnavailable)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.ObjectManager.ListObjects(context.Background(), "", "", "", 1000)
	if err == nil {
		t.Fatal("expected error")
	}
	var s3err *core.S3Error
	if !errors.As(err, &s3err) {
		t.Fatalf("expected st.S3Error, got %T: %v", err, err)
	}
	if s3err.StatusCode != 503 {
		t.Errorf("StatusCode = %d, want 503", s3err.StatusCode)
	}
}

// TestPutObject_BackendTimeout pins the deadline-bound put.
func TestPutObject_BackendTimeout(t *testing.T) {
	t.Parallel()
	backend := &mockBackend{objects: make(map[string]mockObject), putErr: nil}
	slowBackend := &slowMockBackend{mockBackend: backend, delay: 200 * time.Millisecond}

	store, _ := putObjectStore(t, "b1")
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"b1": slowBackend},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"b1"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  50 * time.Millisecond,
		RoutingStrategy: config.RoutingPack,
	})
	wireWorkersForTest(mgr)

	_, err := mgr.ObjectManager.PutObject(context.Background(), "timeout-key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

// slowMockBackend wraps a mockBackend with a delayed PutObject.
type slowMockBackend struct {
	*mockBackend
	delay time.Duration
}

// PutObject sleeps then forwards.
func (s *slowMockBackend) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string, metadata map[string]string) (string, error) {
	select {
	case <-time.After(s.delay):
		return s.mockBackend.PutObject(ctx, key, body, size, contentType, metadata)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// TestLocationCache_SetAndGet pins basic cache set/get.
func TestLocationCache_SetAndGet(t *testing.T) {
	t.Parallel()
	mgr := NewBackendManager(&BackendManagerConfig{CacheTTL: 5 * time.Second, RoutingStrategy: config.RoutingPack})
	wireWorkersForTest(mgr)
	wireWorkersForTest(mgr)
	defer mgr.Close()
	mgr.ObjectManager.cache.Set("key1", "backend-a")

	got, ok := mgr.ObjectManager.cache.Get("key1")
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
	mgr := NewBackendManager(&BackendManagerConfig{CacheTTL: 10 * time.Millisecond, RoutingStrategy: config.RoutingPack})
	wireWorkersForTest(mgr)
	wireWorkersForTest(mgr)
	defer mgr.Close()
	mgr.ObjectManager.cache.Set("key1", "backend-a")

	time.Sleep(15 * time.Millisecond)

	if _, ok := mgr.ObjectManager.cache.Get("key1"); ok {
		t.Fatal("expected cache miss after TTL")
	}
}

// TestLocationCache_Overwrite pins cache overwrites.
func TestLocationCache_Overwrite(t *testing.T) {
	t.Parallel()
	mgr := NewBackendManager(&BackendManagerConfig{CacheTTL: 5 * time.Second, RoutingStrategy: config.RoutingPack})
	wireWorkersForTest(mgr)
	wireWorkersForTest(mgr)
	defer mgr.Close()
	mgr.ObjectManager.cache.Set("key1", "old-backend")
	mgr.ObjectManager.cache.Set("key1", "new-backend")

	got, ok := mgr.ObjectManager.cache.Get("key1")
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
	backend := newMockBackend()
	store, _ := putObjectStore(t, "b1")
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})
	defer mgr.Close()

	mgr.ObjectManager.cache.Set("mykey", "old-backend")

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "mykey", bytes.NewReader([]byte("hello")), 5, "text/plain", nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	if _, ok := mgr.ObjectManager.cache.Get("mykey"); ok {
		t.Error("cache should be invalidated after PutObject")
	}
}

// TestDeleteObject_InvalidatesCache pins post-delete cache invalidation.
func TestDeleteObject_InvalidatesCache(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "del-key", bytes.NewReader([]byte("rm")), 2, "", nil)
	store := deleteObjectStore(t, []core.DeletedCopy{{BackendName: "b1", SizeBytes: 2}}, nil)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})
	defer mgr.Close()

	mgr.ObjectManager.cache.Set("del-key", "b1")

	if err := mgr.ObjectManager.DeleteObject(context.Background(), "del-key"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	if _, ok := mgr.ObjectManager.cache.Get("del-key"); ok {
		t.Error("cache should be invalidated after DeleteObject")
	}
}

// newTestManagerWithLimits constructs a new test manager with limits.
func newTestManagerWithLimits(store core.MetadataStore, backends map[string]*mockBackend, limits map[string]core.UsageLimits) *BackendManager {
	obs := make(map[string]s3be.ObjectBackend, len(backends))
	var order []string
	for name, b := range backends {
		obs[name] = b
		order = append(order, name)
	}
	return wireWorkersForTest(NewBackendManager(&BackendManagerConfig{
		Backends:        obs,
		Stores:          testStoresFromMock(store),
		PendingEnabled:  true,
		Dashboard:       store,
		Metrics:         store,
		Order:           order,
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		UsageLimits:     limits,
		RoutingStrategy: config.RoutingPack,
	}))
}

// TestPutObject_UsageLimitOverflow asserts the eligible-fallback branch.
func TestPutObject_UsageLimitOverflow(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()

	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 10},
		"b2": {APIRequestLimit: 100},
	}
	store, _ := putObjectStore(t, "b2")
	mgr := newTestManagerWithLimits(store, map[string]*mockBackend{"b1": b1, "b2": b2}, limits)

	mgr.usage.SetBaseline("b1", core.UsageStat{APIRequests: 10})

	etag, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject should overflow to b2: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	if b1.hasObject("key") {
		t.Error("object should NOT be on b1 (over limit)")
	}
	if !b2.hasObject("key") {
		t.Error("object should be on b2 (overflow)")
	}
}

// TestGetObject_UsageLimitSkipsBackend asserts limit-driven failover on
// reads.
func TestGetObject_UsageLimitSkipsBackend(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key", bytes.NewReader([]byte("from-b1")), 7, "text/plain", nil)
	_, _ = b2.PutObject(context.Background(), "key", bytes.NewReader([]byte("from-b2")), 7, "text/plain", nil)

	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 10},
		"b2": {APIRequestLimit: 100},
	}
	store := locationsStore(t, []core.ObjectLocation{
		{ObjectKey: "key", BackendName: "b1"},
		{ObjectKey: "key", BackendName: "b2"},
	}, nil)
	mgr := newTestManagerWithLimits(store, map[string]*mockBackend{"b1": b1, "b2": b2}, limits)

	mgr.usage.SetBaseline("b1", core.UsageStat{APIRequests: 10})

	result, err := mgr.ObjectManager.GetObject(context.Background(), "key", "")
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
	b1 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 10},
	}
	store := locationsStore(t, []core.ObjectLocation{
		{ObjectKey: "key", BackendName: "b1"},
	}, nil)
	mgr := newTestManagerWithLimits(store, map[string]*mockBackend{"b1": b1}, limits)

	mgr.usage.SetBaseline("b1", core.UsageStat{APIRequests: 10})

	if _, err := mgr.ObjectManager.GetObject(context.Background(), "key", ""); !errors.Is(err, core.ErrUsageLimitExceeded) {
		t.Fatalf("expected st.ErrUsageLimitExceeded, got %v", err)
	}
}

// TestDeleteObject_AlwaysAllowed asserts deletes ignore usage limits.
func TestDeleteObject_AlwaysAllowed(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "del-key", bytes.NewReader([]byte("rm")), 2, "", nil)

	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 1, EgressByteLimit: 1, IngressByteLimit: 1},
	}
	store := deleteObjectStore(t, []core.DeletedCopy{{BackendName: "b1", SizeBytes: 2}}, nil)
	mgr := newTestManagerWithLimits(store, map[string]*mockBackend{"b1": backend}, limits)

	mgr.usage.SetBaseline("b1", core.UsageStat{APIRequests: 100, EgressBytes: 100, IngressBytes: 100})

	if err := mgr.ObjectManager.DeleteObject(context.Background(), "del-key"); err != nil {
		t.Fatalf("DeleteObject should always succeed regardless of limits: %v", err)
	}
	if backend.hasObject("del-key") {
		t.Error("object should be deleted from backend")
	}
}

// TestPutObject_UsageLimitRejectionsMetric pins the rejection metric on
// writes.
func TestPutObject_UsageLimitRejectionsMetric(t *testing.T) {
	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 10},
	}
	store, _ := putObjectStore(t, "b1")
	mgr := newTestManagerWithLimits(store, map[string]*mockBackend{"b1": newMockBackend()}, limits)
	defer mgr.Close()

	mgr.usage.SetBaseline("b1", core.UsageStat{APIRequests: 10})

	before := testutil.ToFloat64(telemetry.UsageLimitRejectionsTotal.WithLabelValues("PutObject", "write"))

	if _, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("x")), 1, "text/plain", nil); err == nil {
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
	b1 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 10},
	}
	store := locationsStore(t, []core.ObjectLocation{
		{ObjectKey: "key", BackendName: "b1"},
	}, nil)
	mgr := newTestManagerWithLimits(store, map[string]*mockBackend{"b1": b1}, limits)
	defer mgr.Close()

	mgr.usage.SetBaseline("b1", core.UsageStat{APIRequests: 10})

	before := testutil.ToFloat64(telemetry.UsageLimitRejectionsTotal.WithLabelValues("GetObject", "read"))

	if _, err := mgr.ObjectManager.GetObject(context.Background(), "key", ""); !errors.Is(err, core.ErrUsageLimitExceeded) {
		t.Fatalf("expected st.ErrUsageLimitExceeded, got %v", err)
	}

	after := testutil.ToFloat64(telemetry.UsageLimitRejectionsTotal.WithLabelValues("GetObject", "read"))
	if after <= before {
		t.Errorf("UsageLimitRejectionsTotal[GetObject,read] did not increment: before=%v, after=%v", before, after)
	}
}

// newTestManagerParallel creates a BackendManager with parallel
// broadcast enabled and explicit ordering.
func newTestManagerParallel(store core.MetadataStore, orderedBackends []struct {
	name    string
	backend s3be.ObjectBackend
}) *BackendManager {
	obs := make(map[string]s3be.ObjectBackend, len(orderedBackends))
	order := make([]string, 0, len(orderedBackends))
	for _, b := range orderedBackends {
		obs[b.name] = b.backend
		order = append(order, b.name)
	}
	return wireWorkersForTest(NewBackendManager(&BackendManagerConfig{
		Backends:          obs,
		Stores:            testStoresFromMock(store),
		Dashboard:         store,
		Metrics:           store,
		Order:             order,
		CacheTTL:          5 * time.Second,
		BackendTimeout:    30 * time.Second,
		RoutingStrategy:   "pack",
		ParallelBroadcast: true,
	}))
}

// slowGetBackend wraps a mockBackend with delayed Get/Head.
type slowGetBackend struct {
	*mockBackend
	delay time.Duration
}

// GetObject sleeps then forwards.
func (s *slowGetBackend) GetObject(ctx context.Context, key string, rangeHeader string) (*s3be.GetObjectResult, error) {
	select {
	case <-time.After(s.delay):
		return s.mockBackend.GetObject(ctx, key, rangeHeader)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// HeadObject sleeps then forwards.
func (s *slowGetBackend) HeadObject(ctx context.Context, key string) (*s3be.HeadObjectResult, error) {
	select {
	case <-time.After(s.delay):
		return s.mockBackend.HeadObject(ctx, key)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestGetObject_ParallelBroadcast_FirstSuccessWins pins the parallel
// race-to-success behaviour.
func TestGetObject_ParallelBroadcast_FirstSuccessWins(t *testing.T) {
	t.Parallel()
	slow := newMockBackend()
	fast := newMockBackend()
	_, _ = slow.PutObject(context.Background(), "key", bytes.NewReader([]byte("slow-data")), 9, "text/plain", nil)
	_, _ = fast.PutObject(context.Background(), "key", bytes.NewReader([]byte("fast-data")), 9, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newTestManagerParallel(store, []struct {
		name    string
		backend s3be.ObjectBackend
	}{
		{"slow", &slowGetBackend{mockBackend: slow, delay: 200 * time.Millisecond}},
		{"fast", fast},
	})
	defer mgr.Close()

	start := time.Now()
	result, err := mgr.ObjectManager.GetObject(context.Background(), "key", "")
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
	mgr := newTestManagerParallel(store, []struct {
		name    string
		backend s3be.ObjectBackend
	}{
		{"b1", newMockBackend()},
		{"b2", newMockBackend()},
	})
	defer mgr.Close()

	_, err := mgr.ObjectManager.GetObject(context.Background(), "nowhere", "")
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
	b1 := newMockBackend()
	b2 := newMockBackend()
	_, _ = b2.PutObject(context.Background(), "cached-key", bytes.NewReader([]byte("cached")), 6, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newTestManagerParallel(store, []struct {
		name    string
		backend s3be.ObjectBackend
	}{
		{"b1", b1},
		{"b2", b2},
	})
	defer mgr.Close()

	r1, err := mgr.ObjectManager.GetObject(context.Background(), "cached-key", "")
	if err != nil {
		t.Fatalf("first GetObject: %v", err)
	}
	_ = r1.Body.Close()

	r2, err := mgr.ObjectManager.GetObject(context.Background(), "cached-key", "")
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
	slow := newMockBackend()
	fast := newMockBackend()
	_, _ = slow.PutObject(context.Background(), "key", bytes.NewReader([]byte("slow-data")), 9, "text/plain", nil)
	_, _ = fast.PutObject(context.Background(), "key", bytes.NewReader([]byte("fast-data")), 9, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	obs := map[string]s3be.ObjectBackend{
		"slow": &slowGetBackend{mockBackend: slow, delay: 100 * time.Millisecond},
		"fast": fast,
	}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:          obs,
		Stores:            testStoresFromMock(store),
		Dashboard:         store,
		Metrics:           store,
		Order:             []string{"slow", "fast"},
		CacheTTL:          5 * time.Second,
		BackendTimeout:    30 * time.Second,
		RoutingStrategy:   "pack",
		ParallelBroadcast: false,
	})
	wireWorkersForTest(mgr)
	defer mgr.Close()

	start := time.Now()
	result, err := mgr.ObjectManager.GetObject(context.Background(), "key", "")
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

// TestGetObject_BackendNotFound_FailsOverToNext pins the missing-backend
// failover.
func TestGetObject_BackendNotFound_FailsOverToNext(t *testing.T) {
	t.Parallel()
	b2 := newMockBackend()
	_, _ = b2.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	store := locationsStore(t, []core.ObjectLocation{
		{ObjectKey: "key", BackendName: "gone-backend"},
		{ObjectKey: "key", BackendName: "b2"},
	}, nil)
	mgr := newTestManager(store, map[string]*mockBackend{"b2": b2})

	result, err := mgr.ObjectManager.GetObject(context.Background(), "key", "")
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
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.ObjectManager.GetObject(context.Background(), "key", "")
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
	b1 := newMockBackend()
	b2 := newMockBackend()
	_, _ = b2.PutObject(context.Background(), "key", bytes.NewReader([]byte("from-b2")), 7, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": b1, "b2": b2})

	mgr.ObjectManager.cache.Set("key", "b1")

	result, err := mgr.ObjectManager.GetObject(context.Background(), "key", "")
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
	b1 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "", nil)

	store := deleteObjectStore(t, []core.DeletedCopy{
		{BackendName: "gone-backend", SizeBytes: 4},
		{BackendName: "b1", SizeBytes: 4},
	}, nil)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": b1})

	if err := mgr.ObjectManager.DeleteObject(context.Background(), "key"); err != nil {
		t.Fatalf("DeleteObject should succeed even with missing backend: %v", err)
	}
	if b1.hasObject("key") {
		t.Error("expected b1 copy to be deleted")
	}
}

// TestCopyObject_AllSourceHeadsFail surfaces an all-heads-fail error.
func TestCopyObject_AllSourceHeadsFail(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.headErr = errors.New("head failed")

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}}, nil,
		"b1", nil)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": b1})

	if _, err := mgr.ObjectManager.CopyObject(context.Background(), "src", "dst"); err == nil {
		t.Fatal("expected error when all source HeadObjects fail")
	}
}

// TestCopyObject_DestWriteFails surfaces a dst write failure.
func TestCopyObject_DestWriteFails(t *testing.T) {
	t.Parallel()
	src := newMockBackend()
	_, _ = src.PutObject(context.Background(), "src", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dst := newMockBackend()
	dst.putErr = errors.New("write failed")

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "src-be"}}, nil,
		"dst-be", nil)
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"src-be": src, "dst-be": dst},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"src-be", "dst-be"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	wireWorkersForTest(mgr)

	if _, err := mgr.ObjectManager.CopyObject(context.Background(), "src", "dst"); err == nil {
		t.Fatal("expected error when dest PutObject fails")
	}
}

// TestCopyObject_ExcludesDrainingBackend asserts draining backends are
// excluded from copy targets.
func TestCopyObject_ExcludesDrainingBackend(t *testing.T) {
	t.Parallel()
	src := newMockBackend()
	_, _ = src.PutObject(context.Background(), "src", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dst := newMockBackend()

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "src-be"}}, nil,
		"dst-be", nil)
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"src-be": src, "dst-be": dst},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"src-be", "dst-be"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	wireWorkersForTest(mgr)

	mgr.DrainManager.SeedActiveForTest("src-be")
	mgr.DrainManager.SeedActiveForTest("dst-be")

	if _, err := mgr.ObjectManager.CopyObject(context.Background(), "src", "dst"); !errors.Is(err, core.ErrInsufficientStorage) {
		t.Fatalf("expected st.ErrInsufficientStorage when all backends are draining, got %v", err)
	}
	if dst.hasObject("dst") {
		t.Error("object should not have been copied to draining backend")
	}
}

// TestCopyObject_SourceReadFails surfaces a source body-read failure.
func TestCopyObject_SourceReadFails(t *testing.T) {
	t.Parallel()
	src := newMockBackend()
	_, _ = src.PutObject(context.Background(), "src", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	src.getReadErr = errors.New("disk I/O error")

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "src-be"}}, nil,
		"dst-be", nil)
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"src-be": src, "dst-be": newMockBackend()},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"src-be", "dst-be"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	wireWorkersForTest(mgr)

	if _, err := mgr.ObjectManager.CopyObject(context.Background(), "src", "dst"); err == nil {
		t.Fatal("expected error when source body read fails")
	}
}

// TestCopyObject_AllSourceGetObjectsFail surfaces an all-Get-fail error.
func TestCopyObject_AllSourceGetObjectsFail(t *testing.T) {
	t.Parallel()
	src := newMockBackend()
	_, _ = src.PutObject(context.Background(), "src", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	src.getErr = errors.New("get unavailable")

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "src-be"}}, nil,
		"dst-be", nil)
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        map[string]s3be.ObjectBackend{"src-be": src, "dst-be": newMockBackend()},
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           []string{"src-be", "dst-be"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	wireWorkersForTest(mgr)

	if _, err := mgr.ObjectManager.CopyObject(context.Background(), "src", "dst"); err == nil {
		t.Fatal("expected error when all source GetObjects fail")
	}
}

// TestListObjects_GenericError surfaces a non-typed list error.
func TestListObjects_GenericError(t *testing.T) {
	t.Parallel()
	store := listObjectsStore(t, nil, errors.New("unexpected query error"))
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.ObjectManager.ListObjects(context.Background(), "", "", "", 1000)
	if err == nil {
		t.Fatal("expected error from generic store failure")
	}
	var s3err *core.S3Error
	if errors.As(err, &s3err) {
		t.Errorf("generic error should not be st.S3Error, got %+v", s3err)
	}
}

// TestHeadObject_ParallelBroadcast pins parallel HeadObject behaviour.
func TestHeadObject_ParallelBroadcast(t *testing.T) {
	t.Parallel()
	slow := newMockBackend()
	fast := newMockBackend()
	_, _ = slow.PutObject(context.Background(), "key", bytes.NewReader([]byte("head")), 4, "text/plain", nil)
	_, _ = fast.PutObject(context.Background(), "key", bytes.NewReader([]byte("head")), 4, "text/plain", nil)

	store := locationsStore(t, nil, core.ErrDBUnavailable)
	mgr := newTestManagerParallel(store, []struct {
		name    string
		backend s3be.ObjectBackend
	}{
		{"slow", &slowGetBackend{mockBackend: slow, delay: 200 * time.Millisecond}},
		{"fast", fast},
	})
	defer mgr.Close()

	result, err := mgr.ObjectManager.HeadObject(context.Background(), "key")
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
	start, end, ok := parsePlaintextRange("bytes=-1000", 100)
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
	start, end, ok := parsePlaintextRange("bytes=0-200", 100)
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
	start, end, ok := parsePlaintextRange("bytes=0-99", 100)
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
	_, _, ok := parsePlaintextRange("bytes=99-0", 100)
	if ok {
		t.Error("expected ok=false for inverted range")
	}
}

// TestParsePlaintextRange_StartBeyondFile rejects start-past-file.
func TestParsePlaintextRange_StartBeyondFile(t *testing.T) {
	t.Parallel()
	_, _, ok := parsePlaintextRange("bytes=100-200", 100)
	if ok {
		t.Error("expected ok=false when start >= plaintextSize")
	}
}

// TestParsePlaintextRange_OpenEndedBeyondFile rejects open-ended past
// end of file.
func TestParsePlaintextRange_OpenEndedBeyondFile(t *testing.T) {
	t.Parallel()
	_, _, ok := parsePlaintextRange("bytes=100-", 100)
	if ok {
		t.Error("expected ok=false for open-ended range beyond file")
	}
}

// TestCopyObject_SourceGetPanics surfaces a panic in the source-reader
// goroutine as an error.
func TestCopyObject_SourceGetPanics(t *testing.T) {
	t.Parallel()
	srcBackend := newMockBackend()
	srcBackend.getPanic = true

	store, _ := copyObjectStore(t,
		[]core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}}, nil,
		"b1", nil)
	mgr := newTestManager(store, map[string]*mockBackend{"b1": srcBackend})

	if _, err := mgr.ObjectManager.CopyObject(context.Background(), "src", "dst"); err == nil {
		t.Fatal("expected error from panicking source reader, got nil")
	}
}

// TestRedisCounterConfigured_LocalBackendReturnsFalse pins the
// local-backend false branch.
func TestRedisCounterConfigured_LocalBackendReturnsFalse(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(newPermissiveMock(t), map[string]*mockBackend{"b1": newMockBackend()})

	if mgr.RedisCounterConfigured() {
		t.Errorf("RedisCounterConfigured = true, want false for local counter backend")
	}
}
