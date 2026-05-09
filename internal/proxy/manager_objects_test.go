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
	"testing"
	"time"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newTestManager creates a BackendManager with mock backends and store for testing.
func newTestManager(store *mockStore, backends map[string]*mockBackend) *BackendManager {
	obs := make(map[string]s3be.ObjectBackend, len(backends))
	var order []string
	for name, b := range backends {
		obs[name] = b
		order = append(order, name)
	}
	return wireWorkersForTest(NewBackendManager(&BackendManagerConfig{
		Backends:        obs,
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           order,
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	}))
}

// -------------------------------------------------------------------------
// PutObject
// -------------------------------------------------------------------------

// TestPutObject_Success verifies the put object success contract.
// Asserts that PutObject:.
func TestPutObject_Success(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{getBackendResp: "b1"}
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
	if len(store.recordObjectCalls) != 1 {
		t.Fatalf("expected 1 RecordObject call, got %d", len(store.recordObjectCalls))
	}
	call := store.recordObjectCalls[0]
	if call.Key != "mykey" || call.Backend != "b1" || call.Size != 5 {
		t.Errorf("RecordObject called with %+v", call)
	}
}

// TestPutObject_PackStrategy_UsesGetBackendWithSpace verifies the put object pack strategy uses get backend with space contract.
// Asserts that PutObject:.
func TestPutObject_PackStrategy_UsesGetBackendWithSpace(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{getBackendResp: "b1"}
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

	_, err := mgr.ObjectManager.PutObject(context.Background(), "pack-key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if store.getBackendWithSpaceCalls != 1 {
		t.Errorf("expected 1 GetBackendWithSpace call, got %d", store.getBackendWithSpaceCalls)
	}
	if store.getLeastUtilizedCalls != 0 {
		t.Errorf("expected 0 GetLeastUtilizedBackend calls, got %d", store.getLeastUtilizedCalls)
	}
}

// TestPutObject_SpreadStrategy_UsesGetLeastUtilized verifies the put object spread strategy uses get least utilized contract.
// Asserts that PutObject:.
func TestPutObject_SpreadStrategy_UsesGetLeastUtilized(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{getBackendResp: "b1"}
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

	_, err := mgr.ObjectManager.PutObject(context.Background(), "spread-key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if store.getLeastUtilizedCalls != 1 {
		t.Errorf("expected 1 GetLeastUtilizedBackend call, got %d", store.getLeastUtilizedCalls)
	}
	if store.getBackendWithSpaceCalls != 0 {
		t.Errorf("expected 0 GetBackendWithSpace calls, got %d", store.getBackendWithSpaceCalls)
	}
}

// -------------------------------------------------------------------------
// CanAcceptWrite
// -------------------------------------------------------------------------

// TestCanAcceptWrite_HasCapacity verifies the can accept write has capacity behaviour described by the test name.
func TestCanAcceptWrite_HasCapacity(t *testing.T) {
	t.Parallel()
	store := &mockStore{getBackendResp: "b1"}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	if !mgr.ObjectManager.CanAcceptWrite(100) {
		t.Error("CanAcceptWrite should return true when backend has capacity")
	}
}

// TestCanAcceptWrite_NoCapacity verifies the can accept write no capacity behaviour described by the test name.
func TestCanAcceptWrite_NoCapacity(t *testing.T) {
	t.Parallel()
	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 1},
	}
	store := &mockStore{}
	mgr := newTestManagerWithLimits(store, map[string]*mockBackend{"b1": newMockBackend()}, limits)

	// Push b1 over its API request limit
	mgr.ObjectManager.usage.SetBaseline("b1", core.UsageStat{APIRequests: 1})

	if mgr.ObjectManager.CanAcceptWrite(100) {
		t.Error("CanAcceptWrite should return false when no backend has capacity")
	}
}

// TestBackendCapacityStats_PassesThroughStoreSnapshot verifies that
// BackendCapacityStats forwards the QuotaStore snapshot to the caller
// for use in the InsufficientStorage error body.
func TestBackendCapacityStats_PassesThroughStoreSnapshot(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		getQuotaStatsResp: map[string]core.QuotaStat{
			"b1": {BackendName: "b1", BytesUsed: 100, BytesLimit: 1000},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	got := mgr.ObjectManager.BackendCapacityStats(context.Background())
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got["b1"].BytesUsed != 100 || got["b1"].BytesLimit != 1000 {
		t.Errorf("snapshot mismatch: %+v", got["b1"])
	}
}

// TestBackendCapacityStats_DBFailureReturnsNil verifies that a
// QuotaStore lookup error degrades to nil so the caller falls back
// to its terse default error message instead of failing the
// response.
func TestBackendCapacityStats_DBFailureReturnsNil(t *testing.T) {
	t.Parallel()
	store := &mockStore{getQuotaStatsErr: core.ErrDBUnavailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	if got := mgr.ObjectManager.BackendCapacityStats(context.Background()); got != nil {
		t.Errorf("BackendCapacityStats on DB failure = %+v, want nil", got)
	}
}

// TestPutObject_QuotaExhausted verifies the put object quota exhausted contract.
// Asserts that expected st.ErrInsufficientStorage, got.
func TestPutObject_QuotaExhausted(t *testing.T) {
	t.Parallel()
	store := &mockStore{getBackendErr: core.ErrNoSpaceAvailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("x")), 1, "", nil)
	if !errors.Is(err, core.ErrInsufficientStorage) {
		t.Fatalf("expected st.ErrInsufficientStorage, got %v", err)
	}
}

// TestPutObject_DBUnavailable verifies the put object dbunavailable contract.
// Asserts that expected st.ErrServiceUnavailable, got.
func TestPutObject_DBUnavailable(t *testing.T) {
	t.Parallel()
	store := &mockStore{getBackendErr: core.ErrDBUnavailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("x")), 1, "", nil)
	if !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// TestPutObject_BackendFailure_StillRecordsUsage verifies the put object backend failure still records usage contract.
// Asserts that apiRequests = , want 1 (failed call still counts).
func TestPutObject_BackendFailure_StillRecordsUsage(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.putErr = errors.New("backend timeout")
	store := &mockStore{getBackendResp: "b1"}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	_, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	if err == nil {
		t.Fatal("expected error from backend failure")
	}

	// Even on failure, 1 API call should be recorded (the attempt was made)
	if got := mgr.usage.Backend().Load("b1", counter.FieldAPIRequests); got != 1 {
		t.Errorf("apiRequests = %d, want 1 (failed call still counts)", got)
	}
	// No ingress should be recorded since the upload failed
	if got := mgr.usage.Backend().Load("b1", counter.FieldIngressBytes); got != 0 {
		t.Errorf("ingressBytes = %d, want 0 (upload failed)", got)
	}
}

// TestPutObject_RecordFailure_LeavesBackendBytesAndPendingIntent is the
// regression test for the data-loss window in issue #657. It verifies the
// pending-row pattern: on metadata commit failure the backend bytes stay
// in place and a pending intent is left for the reaper to resolve.
func TestPutObject_RecordFailure_LeavesBackendBytesAndPendingIntent(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{
		getBackendResp:  "b1",
		recordObjectErr: errors.New("db write failed"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	_, err := mgr.ObjectManager.PutObject(context.Background(), "cleanup-key", bytes.NewReader([]byte("data")), 4, "", nil)
	if err == nil {
		t.Fatal("expected error from RecordObjectAndClearPending failure")
	}
	// With the pending-row pattern, the backend bytes are intentionally
	// retained on commit failure: the pending intent remains and the
	// reaper resolves it on a later tick. Deleting the bytes here would
	// reintroduce the data-loss window the pattern exists to close.
	if !backend.hasObject("cleanup-key") {
		t.Error("backend bytes should be retained for the pending reaper to resolve")
	}
	if len(store.insertPendingCalls) != 1 {
		t.Fatalf("expected 1 InsertPending call, got %d", len(store.insertPendingCalls))
	}
	if store.insertPendingCalls[0].ObjectKey != "cleanup-key" || store.insertPendingCalls[0].BackendName != "b1" {
		t.Errorf("InsertPending called with %+v", store.insertPendingCalls[0])
	}

	// Usage: 1 API call  -  the successful PUT. The cleanup DELETE no longer
	// runs because the bytes are intentionally left for reaper reconciliation.
	if got := mgr.usage.Backend().Load("b1", counter.FieldAPIRequests); got != 1 {
		t.Errorf("apiRequests = %d, want 1 (PUT only)", got)
	}
}

// TestPutObject_RecordFailure_LegacyPath verifies that when the pending
// store is not wired (feature gate off), the write path falls back to the
// legacy delete-on-record-failure behaviour: backend bytes are removed and
// no pending intent is recorded.
func TestPutObject_RecordFailure_LegacyPath(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{
		getBackendResp:  "b1",
		recordObjectErr: errors.New("db write failed"),
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})
	mgr.stores.Pending = nil // simulate feature-disabled wiring

	_, err := mgr.ObjectManager.PutObject(context.Background(), "legacy-key", bytes.NewReader([]byte("data")), 4, "", nil)
	if err == nil {
		t.Fatal("expected error from RecordObject failure")
	}
	if backend.hasObject("legacy-key") {
		t.Error("legacy path should delete the orphan from the backend")
	}
	if len(store.insertPendingCalls) != 0 {
		t.Errorf("legacy path should not insert pending intents, got %d", len(store.insertPendingCalls))
	}
}

// errReader is an io.Reader that always returns the configured error.
type errReader struct{ err error }

// Read reads .
func (r *errReader) Read([]byte) (int, error) { return 0, r.err }

// newTestManagerWithOrder creates a BackendManager with an explicit backend order
// (deterministic, unlike newTestManager which iterates a map).
func newTestManagerWithOrder(store *mockStore, backends map[string]*mockBackend, order []string) *BackendManager {
	obs := make(map[string]s3be.ObjectBackend, len(backends))
	for name, b := range backends {
		obs[name] = b
	}
	return wireWorkersForTest(NewBackendManager(&BackendManagerConfig{
		Backends:        obs,
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           order,
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	}))
}

// -------------------------------------------------------------------------
// PutObject Write Failover
// -------------------------------------------------------------------------

// TestPutObject_WriteFailover_Success verifies the put object write failover success contract.
// Asserts that PutObject should succeed via failover:.
func TestPutObject_WriteFailover_Success(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.putErr = errors.New("connection refused")
	b2 := newMockBackend()

	store := &mockStore{getBackendFromEligible: true}
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": b1, "b2": b2}, []string{"b1", "b2"})

	etag, err := mgr.ObjectManager.PutObject(context.Background(), "failover-key", bytes.NewReader([]byte("hello")), 5, "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject should succeed via failover: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}

	// Object should be on b2, not b1
	if b1.hasObject("failover-key") {
		t.Error("object should NOT be on failed backend b1")
	}
	if !b2.hasObject("failover-key") {
		t.Error("object should be on failover backend b2")
	}

	// RecordObject should be called once for the successful backend
	if len(store.recordObjectCalls) != 1 {
		t.Fatalf("expected 1 RecordObject call, got %d", len(store.recordObjectCalls))
	}
	if store.recordObjectCalls[0].Backend != "b2" {
		t.Errorf("RecordObject backend = %s, want b2", store.recordObjectCalls[0].Backend)
	}
}

// TestPutObject_WriteFailover_AllBackendsFail verifies the put object write failover all backends fail contract.
// Asserts that total API requests = , want 3 (one per failed backend).
func TestPutObject_WriteFailover_AllBackendsFail(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.putErr = errors.New("b1 down")
	b2 := newMockBackend()
	b2.putErr = errors.New("b2 down")
	b3 := newMockBackend()
	b3.putErr = errors.New("b3 down")

	store := &mockStore{getBackendFromEligible: true}
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": b1, "b2": b2, "b3": b3}, []string{"b1", "b2", "b3"})

	_, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	if err == nil {
		t.Fatal("expected error when all backends fail")
	}

	// All three backends should have been tried (3 API call records)
	total := mgr.usage.Backend().Load("b1", counter.FieldAPIRequests) +
		mgr.usage.Backend().Load("b2", counter.FieldAPIRequests) +
		mgr.usage.Backend().Load("b3", counter.FieldAPIRequests)
	if total != 3 {
		t.Errorf("total API requests = %d, want 3 (one per failed backend)", total)
	}
}

// TestPutObject_WriteFailover_SkipsMultipleFailedBackends verifies the put object write failover skips multiple failed backends contract.
// Asserts that PutObject should succeed on b3:.
func TestPutObject_WriteFailover_SkipsMultipleFailedBackends(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.putErr = errors.New("b1 down")
	b2 := newMockBackend()
	b2.putErr = errors.New("b2 down")
	b3 := newMockBackend()

	store := &mockStore{getBackendFromEligible: true}
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
	// 2 failed attempts + 1 success = 3 total GetBackendWithSpace calls
	if store.getBackendWithSpaceCalls != 3 {
		t.Errorf("GetBackendWithSpace calls = %d, want 3", store.getBackendWithSpaceCalls)
	}
}

// TestPutObject_WriteFailover_Metrics verifies the put object write failover metrics contract.
// Asserts that PutObject:.
func TestPutObject_WriteFailover_Metrics(t *testing.T) {
	telemetry.WriteFailoverTotal.Reset()

	b1 := newMockBackend()
	b1.putErr = errors.New("b1 down")
	b2 := newMockBackend()

	store := &mockStore{getBackendFromEligible: true}
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": b1, "b2": b2}, []string{"b1", "b2"})

	_, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	got := testutil.ToFloat64(telemetry.WriteFailoverTotal.WithLabelValues("PutObject", "b1", "b2"))
	if got != 1 {
		t.Errorf("WriteFailoverTotal{PutObject,b1,b2} = %v, want 1", got)
	}
}

// TestPutObject_WriteFailover_UsageTracking verifies the put object write failover usage tracking contract.
// Asserts that PutObject:.
func TestPutObject_WriteFailover_UsageTracking(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.putErr = errors.New("b1 timeout")
	b2 := newMockBackend()

	store := &mockStore{getBackendFromEligible: true}
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": b1, "b2": b2}, []string{"b1", "b2"})

	_, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// b1: 1 API call (failed attempt), 0 ingress
	if got := mgr.usage.Backend().Load("b1", counter.FieldAPIRequests); got != 1 {
		t.Errorf("b1 apiRequests = %d, want 1", got)
	}
	if got := mgr.usage.Backend().Load("b1", counter.FieldIngressBytes); got != 0 {
		t.Errorf("b1 ingressBytes = %d, want 0", got)
	}

	// b2: 1 API call (success) + 4 bytes ingress
	if got := mgr.usage.Backend().Load("b2", counter.FieldAPIRequests); got != 1 {
		t.Errorf("b2 apiRequests = %d, want 1", got)
	}
	if got := mgr.usage.Backend().Load("b2", counter.FieldIngressBytes); got != 4 {
		t.Errorf("b2 ingressBytes = %d, want 4", got)
	}
}

// TestPutObject_WriteFailover_DataIntegrity verifies the put object write failover data integrity contract.
// Asserts that PutObject:.
func TestPutObject_WriteFailover_DataIntegrity(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.putErr = errors.New("b1 down")
	b2 := newMockBackend()

	store := &mockStore{getBackendFromEligible: true}
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": b1, "b2": b2}, []string{"b1", "b2"})

	payload := []byte("the quick brown fox jumps over the lazy dog")
	_, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader(payload), int64(len(payload)), "text/plain", map[string]string{"x-custom": "value"})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Verify the data written to b2 matches the original payload
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

// TestPutObject_WriteFailover_BufferBodyError verifies the put object write failover buffer body error contract.
// Asserts that error = , want.
func TestPutObject_WriteFailover_BufferBodyError(t *testing.T) {
	t.Parallel()
	store := &mockStore{getBackendFromEligible: true}
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": newMockBackend()}, []string{"b1"})

	// errReader returns an error on Read, triggering the body buffer failure path
	_, err := mgr.ObjectManager.PutObject(context.Background(), "key", &errReader{err: errors.New("read failed")}, 4, "text/plain", nil)
	if err == nil {
		t.Fatal("expected error from body buffer failure")
	}
	if !errors.Is(err, fmt.Errorf("buffer request body: %w", errors.New("read failed"))) {
		// Just check the error message contains the expected text
		if got := err.Error(); got != "buffer request body: read failed" {
			t.Errorf("error = %q, want %q", got, "buffer request body: read failed")
		}
	}
}

// TestPutObject_WriteFailover_SelectBackendErrorDuringRetry verifies the put object write failover select backend error during retry contract.
// Asserts that expected st.ErrServiceUnavailable, got.
func TestPutObject_WriteFailover_SelectBackendErrorDuringRetry(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.putErr = errors.New("b1 down")

	callCount := 0
	store := &mockStore{
		getBackendFunc: func(_ int64, eligible []string) (string, error) {
			callCount++
			if callCount == 1 {
				return eligible[0], nil // first call succeeds, returns b1
			}
			return "", core.ErrDBUnavailable // second call fails (DB went down mid-retry)
		},
	}
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": b1, "b2": newMockBackend()}, []string{"b1", "b2"})

	_, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	if !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// TestPutObject_WriteFailover_BackendNotInMap verifies the put object write failover backend not in map path by exercising context.Background, bytes.NewReader.
func TestPutObject_WriteFailover_BackendNotInMap(t *testing.T) {
	t.Parallel()
	// Store returns a backend name that doesn't exist in the backends map
	store := &mockStore{getBackendResp: "ghost"}
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": newMockBackend()}, []string{"b1"})

	_, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	if err == nil {
		t.Fatal("expected error when backend not in map")
	}
}

// TestPutObject_WriteFailover_WithEncryption verifies the put object write failover with encryption contract.
// Asserts that NewConfigKeyProvider:.
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

	store := &mockStore{getBackendFromEligible: true}
	obs := map[string]s3be.ObjectBackend{"b1": b1, "b2": b2}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        obs,
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

	// Object should be on b2, not b1
	if b1.hasObject("enc-key") {
		t.Error("object should NOT be on failed backend b1")
	}
	if !b2.hasObject("enc-key") {
		t.Error("object should be on failover backend b2")
	}

	// Verify the recorded object has encryption metadata
	if len(store.recordObjectCalls) != 1 {
		t.Fatalf("expected 1 RecordObject call, got %d", len(store.recordObjectCalls))
	}
	if store.recordObjectCalls[0].Backend != "b2" {
		t.Errorf("RecordObject backend = %s, want b2", store.recordObjectCalls[0].Backend)
	}

	// Ciphertext should be larger than plaintext (envelope overhead)
	b2.mu.Lock()
	ciphertextLen := len(b2.objects["enc-key"].data)
	b2.mu.Unlock()
	if ciphertextLen <= len(payload) {
		t.Errorf("ciphertext len %d should be > plaintext len %d", ciphertextLen, len(payload))
	}
}

// TestGetObject_WithEncryption_UsesLocationMap verifies the get object with encryption uses location map contract.
// Asserts that NewConfigKeyProvider:.
func TestGetObject_WithEncryption_UsesLocationMap(t *testing.T) {
	t.Parallel()
	// This test verifies the locByBackend map is built and used during
	// encrypted GetObject. We use HeadObject-level verification since
	// full decrypt round-trip requires wiring encryption metadata through
	// the mock store which is tested in integration tests.
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

	// Non-encrypted location  -  encryptor is set but object is not encrypted.
	// This exercises the locByBackend map build + lookup path without needing
	// valid encryption metadata.
	store := &mockStore{
		getBackendResp: "b1",
		getAllLocationsResp: []core.ObjectLocation{
			{ObjectKey: "enc-key", BackendName: "b1", SizeBytes: 4, Encrypted: false},
		},
	}
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

// TestHeadObject_WithEncryption verifies the head object with encryption contract.
// Asserts that NewConfigKeyProvider:.
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

	store := &mockStore{
		getBackendResp: "b1",
		getAllLocationsResp: []core.ObjectLocation{
			{ObjectKey: "enc-key", BackendName: "b1", SizeBytes: 100, Encrypted: true, PlaintextSize: 25},
		},
	}
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

	// Put an encrypted object
	payload := []byte("head-encryption-test-data")
	_, err = mgr.ObjectManager.PutObject(context.Background(), "enc-key", bytes.NewReader(payload), int64(len(payload)), "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Head should return plaintext size
	head, err := mgr.ObjectManager.HeadObject(context.Background(), "enc-key")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if head.Size != 25 {
		t.Errorf("HeadObject size = %d, want 25 (plaintext size from location)", head.Size)
	}
}

// TestPutObject_WriteFailover_NoFailoverMetricOnFirstSuccess verifies the put object write failover no failover metric on first success contract.
// Asserts that PutObject:.
func TestPutObject_WriteFailover_NoFailoverMetricOnFirstSuccess(t *testing.T) {
	telemetry.WriteFailoverTotal.Reset()

	b1 := newMockBackend()
	b2 := newMockBackend()

	store := &mockStore{getBackendFromEligible: true}
	mgr := newTestManagerWithOrder(store, map[string]*mockBackend{"b1": b1, "b2": b2}, []string{"b1", "b2"})

	_, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// No failover occurred  -  metric should be 0
	got := testutil.ToFloat64(telemetry.WriteFailoverTotal.WithLabelValues("PutObject", "b1", "b2"))
	if got != 0 {
		t.Errorf("WriteFailoverTotal should be 0 when no failover occurs, got %v", got)
	}
}

// -------------------------------------------------------------------------
// GetObject
// -------------------------------------------------------------------------

// TestGetObject_Success verifies the get object success contract.
// Asserts that GetObject:.
func TestGetObject_Success(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "key", bytes.NewReader([]byte("hello")), 5, "text/plain", nil)

	store := &mockStore{
		getAllLocationsResp: []core.ObjectLocation{{ObjectKey: "key", BackendName: "b1"}},
	}
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

// TestGetObject_NotFound verifies the get object not found contract.
// Asserts that expected st.ErrObjectNotFound, got.
func TestGetObject_NotFound(t *testing.T) {
	t.Parallel()
	store := &mockStore{getAllLocationsErr: core.ErrObjectNotFound}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.ObjectManager.GetObject(context.Background(), "missing", "")
	if !errors.Is(err, core.ErrObjectNotFound) {
		t.Fatalf("expected st.ErrObjectNotFound, got %v", err)
	}
}

// TestGetObject_FailoverToReplica verifies the get object failover to replica contract.
// Asserts that GetObject should failover:.
func TestGetObject_FailoverToReplica(t *testing.T) {
	t.Parallel()
	primary := newMockBackend()
	primary.getErr = errors.New("backend down") // primary fails
	replica := newMockBackend()
	_, _ = replica.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	store := &mockStore{
		getAllLocationsResp: []core.ObjectLocation{
			{ObjectKey: "key", BackendName: "primary"},
			{ObjectKey: "key", BackendName: "replica"},
		},
	}
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

// TestGetObject_DBUnavailable_BroadcastHit verifies the get object dbunavailable broadcast hit contract.
// Asserts that GetObject broadcast should succeed:.
func TestGetObject_DBUnavailable_BroadcastHit(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "key", bytes.NewReader([]byte("broadcast")), 9, "text/plain", nil)

	store := &mockStore{getAllLocationsErr: core.ErrDBUnavailable}
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

// TestGetObject_DBUnavailable_CacheHit verifies the get object dbunavailable cache hit contract.
// Asserts that first GetObject:.
func TestGetObject_DBUnavailable_CacheHit(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	_, _ = b2.PutObject(context.Background(), "cached-key", bytes.NewReader([]byte("cached")), 6, "text/plain", nil)

	store := &mockStore{getAllLocationsErr: core.ErrDBUnavailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": b1, "b2": b2})

	// First call populates cache via broadcast
	r1, err := mgr.ObjectManager.GetObject(context.Background(), "cached-key", "")
	if err != nil {
		t.Fatalf("first GetObject: %v", err)
	}
	_ = r1.Body.Close()

	// Second call should use cache
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

// TestGetObject_DBUnavailable_AllFail verifies the get object dbunavailable all fail path by exercising context.Background, errors.Is.
func TestGetObject_DBUnavailable_AllFail(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend() // empty  -  no object
	b2 := newMockBackend() // empty  -  no object

	store := &mockStore{getAllLocationsErr: core.ErrDBUnavailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": b1, "b2": b2})

	_, err := mgr.ObjectManager.GetObject(context.Background(), "nowhere", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Should NOT be st.ErrObjectNotFound  -  real backend errors should propagate
	// so the server maps them to 502 instead of a misleading 404.
	if errors.Is(err, core.ErrObjectNotFound) {
		t.Fatal("should not mask backend errors as st.ErrObjectNotFound")
	}
}

// TestGetObject_DBUnavailable_EncryptedRejects503 verifies the get object dbunavailable encrypted rejects503 contract.
// Asserts that NewConfigKeyProvider:.
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

	store := &mockStore{getAllLocationsErr: core.ErrDBUnavailable}
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

	// With encryption enabled and DB down, GetObject should return 503
	// instead of serving raw ciphertext to the client.
	_, err = mgr.ObjectManager.GetObject(context.Background(), "enc-key", "")
	if err == nil {
		t.Fatal("expected error for encrypted read with DB unavailable")
	}
	var s3err *core.S3Error
	if !errors.As(err, &s3err) || s3err.StatusCode != 503 {
		t.Errorf("expected 503 S3Error, got: %v", err)
	}
}

// -------------------------------------------------------------------------
// HeadObject
// -------------------------------------------------------------------------

// TestHeadObject_Success verifies the head object success contract.
// Asserts that HeadObject:.
func TestHeadObject_Success(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "key", bytes.NewReader([]byte("headme")), 6, "application/json", nil)

	store := &mockStore{
		getAllLocationsResp: []core.ObjectLocation{{ObjectKey: "key", BackendName: "b1"}},
	}
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

// TestHeadObject_DBUnavailable_Broadcast verifies the head object dbunavailable broadcast contract.
// Asserts that HeadObject broadcast should succeed:.
func TestHeadObject_DBUnavailable_Broadcast(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "key", bytes.NewReader([]byte("head")), 4, "text/plain", nil)

	store := &mockStore{getAllLocationsErr: core.ErrDBUnavailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	result, err := mgr.ObjectManager.HeadObject(context.Background(), "key")
	if err != nil {
		t.Fatalf("HeadObject broadcast should succeed: %v", err)
	}
	if result.Size != 4 {
		t.Errorf("size = %d, want 4", result.Size)
	}
}

// -------------------------------------------------------------------------
// DeleteObject
// -------------------------------------------------------------------------

// TestDeleteObject_Success verifies the delete object success contract.
// Asserts that DeleteObject:.
func TestDeleteObject_Success(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "del-key", bytes.NewReader([]byte("rm")), 2, "", nil)

	store := &mockStore{
		deleteObjectResp: []core.DeletedCopy{{BackendName: "b1", SizeBytes: 2}},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	err := mgr.ObjectManager.DeleteObject(context.Background(), "del-key")
	if err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if backend.hasObject("del-key") {
		t.Error("object should be deleted from backend")
	}
}

// TestDeleteObject_NotFound_Idempotent verifies the delete object not found idempotent contract.
// Asserts that DeleteObject of nonexistent key should succeed (idempotent):.
func TestDeleteObject_NotFound_Idempotent(t *testing.T) {
	t.Parallel()
	store := &mockStore{deleteObjectErr: core.ErrObjectNotFound}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	err := mgr.ObjectManager.DeleteObject(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("DeleteObject of nonexistent key should succeed (idempotent): %v", err)
	}
}

// TestDeleteObject_DBUnavailable verifies the delete object dbunavailable contract.
// Asserts that expected st.ErrServiceUnavailable, got.
func TestDeleteObject_DBUnavailable(t *testing.T) {
	t.Parallel()
	store := &mockStore{deleteObjectErr: core.ErrDBUnavailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	err := mgr.ObjectManager.DeleteObject(context.Background(), "key")
	if !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// -------------------------------------------------------------------------
// DeleteObjects (batch)
// -------------------------------------------------------------------------

// TestDeleteObjects_AllSuccess verifies the delete objects all success contract.
// Asserts that expected 3 results, got.
func TestDeleteObjects_AllSuccess(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	for _, k := range []string{"a", "b", "c"} {
		_, _ = backend.PutObject(context.Background(), k, bytes.NewReader([]byte("x")), 1, "", nil)
	}

	store := &mockStore{
		deleteObjectsBatchFunc: func(keys []string) (map[string][]core.DeletedCopy, error) {
			out := make(map[string][]core.DeletedCopy, len(keys))
			for _, k := range keys {
				out[k] = []core.DeletedCopy{{BackendName: "b1", SizeBytes: 1}}
			}
			return out, nil
		},
	}
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

// TestDeleteObjects_DBFailureFailsAll verifies the batch's all-or-
// nothing semantics: a DB error during the single transaction surfaces
// to every result. The earlier "partial DB failure" case no longer
// applies under one-tx semantics.
func TestDeleteObjects_DBFailureFailsAll(t *testing.T) {
	t.Parallel()
	store := &mockStore{deleteObjectsBatchErr: errors.New("db error")}
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

// TestDeleteObjects_NotFoundIsSuccess verifies the delete objects not found is success contract.
// Asserts that results[]: not-found should be success, got.
func TestDeleteObjects_NotFoundIsSuccess(t *testing.T) {
	t.Parallel()
	// Empty map (default) means every key was not found; single-tx
	// returned no displaced copies. S3 spec: not-found is success.
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	results := mgr.ObjectManager.DeleteObjects(context.Background(), []string{"gone1", "gone2"})

	for i, r := range results {
		if r.Err != nil {
			t.Errorf("results[%d]: not-found should be success, got %v", i, r.Err)
		}
	}
}

// TestDeleteObjects_BackendFailureEnqueuesCleanup verifies the delete objects backend failure enqueues cleanup contract.
// Asserts that results[]: unexpected error:.
func TestDeleteObjects_BackendFailureEnqueuesCleanup(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	backend.delErr = errors.New("backend down")

	store := &mockStore{
		deleteObjectsBatchFunc: func(keys []string) (map[string][]core.DeletedCopy, error) {
			out := make(map[string][]core.DeletedCopy, len(keys))
			for _, k := range keys {
				out[k] = []core.DeletedCopy{{BackendName: "b1", SizeBytes: 1}}
			}
			return out, nil
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	results := mgr.ObjectManager.DeleteObjects(context.Background(), []string{"k1", "k2"})

	// Results should still show success (backend failure is best-effort)
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("results[%d]: unexpected error: %v", i, r.Err)
		}
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.enqueueCleanupCalls) != 2 {
		t.Fatalf("expected 2 enqueue calls, got %d", len(store.enqueueCleanupCalls))
	}
	for _, c := range store.enqueueCleanupCalls {
		if c.reason != "batch_delete_failed" {
			t.Errorf("expected reason=batch_delete_failed, got %q", c.reason)
		}
	}
}

// TestDeleteObjects_EmptyKeys verifies the delete objects empty keys contract.
// Asserts that expected 0 results, got.
func TestDeleteObjects_EmptyKeys(t *testing.T) {
	t.Parallel()
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	results := mgr.ObjectManager.DeleteObjects(context.Background(), []string{})

	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

// TestDeleteObjects_BackendNotInMap verifies the delete objects backend not in map contract.
// Asserts that expected 1 result, got.
func TestDeleteObjects_BackendNotInMap(t *testing.T) {
	t.Parallel()
	// DB returns a deleted copy pointing to a backend that doesn't exist
	store := &mockStore{
		deleteObjectsBatchFunc: func(keys []string) (map[string][]core.DeletedCopy, error) {
			out := make(map[string][]core.DeletedCopy, len(keys))
			for _, k := range keys {
				out[k] = []core.DeletedCopy{{BackendName: "ghost", SizeBytes: 1}}
			}
			return out, nil
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	results := mgr.ObjectManager.DeleteObjects(context.Background(), []string{"k1"})

	// Should still succeed (backend not found is non-fatal for deletes)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Errorf("expected no error (missing backend is non-fatal), got %v", results[0].Err)
	}
}

// -------------------------------------------------------------------------
// CopyObject
// -------------------------------------------------------------------------

// TestCopyObject_Success verifies the copy object success contract.
// Asserts that CopyObject:.
func TestCopyObject_Success(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "src", bytes.NewReader([]byte("copy-me")), 7, "text/plain", nil)

	store := &mockStore{
		getAllLocationsResp: []core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}},
		getBackendResp:      "b1",
	}
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

// TestCopyObject_SourceNotFound verifies the copy object source not found contract.
// Asserts that expected st.ErrObjectNotFound, got.
func TestCopyObject_SourceNotFound(t *testing.T) {
	t.Parallel()
	store := &mockStore{getAllLocationsErr: core.ErrObjectNotFound}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.ObjectManager.CopyObject(context.Background(), "missing", "dst")
	if !errors.Is(err, core.ErrObjectNotFound) {
		t.Fatalf("expected st.ErrObjectNotFound, got %v", err)
	}
}

// TestCopyObject_DBUnavailable_SourceLookup verifies the copy object dbunavailable source lookup contract.
// Asserts that expected st.ErrServiceUnavailable, got.
func TestCopyObject_DBUnavailable_SourceLookup(t *testing.T) {
	t.Parallel()
	store := &mockStore{getAllLocationsErr: core.ErrDBUnavailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.ObjectManager.CopyObject(context.Background(), "src", "dst")
	if !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// TestCopyObject_DBUnavailable_DestLookup verifies the copy object dbunavailable dest lookup contract.
// Asserts that expected st.ErrServiceUnavailable, got.
func TestCopyObject_DBUnavailable_DestLookup(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "src", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	store := &mockStore{
		getAllLocationsResp: []core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}},
		getBackendErr:       core.ErrDBUnavailable,
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	_, err := mgr.ObjectManager.CopyObject(context.Background(), "src", "dst")
	if !errors.Is(err, core.ErrServiceUnavailable) {
		t.Fatalf("expected st.ErrServiceUnavailable, got %v", err)
	}
}

// -------------------------------------------------------------------------
// ListObjects
// -------------------------------------------------------------------------

// TestListObjects_Success verifies the list objects success contract.
// Asserts that ListObjects:.
func TestListObjects_Success(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		listObjectsResp: &core.ListObjectsResult{
			Objects: []core.ObjectLocation{
				{ObjectKey: "a/1", BackendName: "b1", SizeBytes: 10},
				{ObjectKey: "a/2", BackendName: "b1", SizeBytes: 20},
			},
		},
	}
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

// TestListObjects_WithDelimiter verifies the list objects with delimiter contract.
// Asserts that ListObjects:.
func TestListObjects_WithDelimiter(t *testing.T) {
	t.Parallel()
	store := &mockStore{
		listObjectsResp: &core.ListObjectsResult{
			Objects: []core.ObjectLocation{
				{ObjectKey: "photos/2024/a.jpg", BackendName: "b1"},
				{ObjectKey: "photos/2024/b.jpg", BackendName: "b1"},
				{ObjectKey: "photos/2025/c.jpg", BackendName: "b1"},
				{ObjectKey: "photos/top.jpg", BackendName: "b1"},
			},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	result, err := mgr.ObjectManager.ListObjects(context.Background(), "photos/", "/", "", 1000)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	// "photos/top.jpg" is a direct child (no more delimiters)
	// "photos/2024/" and "photos/2025/" are common prefixes
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

// TestListObjects_DelimiterPagination verifies the list objects delimiter pagination contract.
// Asserts that ListObjects:.
func TestListObjects_DelimiterPagination(t *testing.T) {
	t.Parallel()
	// Many objects collapse into one common prefix per page. The manager
	// must loop-fetch from the store to fill the requested maxKeys.
	store := &mockStore{
		listObjectsPages: []core.ListObjectsResult{
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
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// Request maxKeys=3 with delimiter "/". The first page produces 1 prefix
	// ("dir/a/"), the second produces 1 ("dir/b/"), the third produces
	// 1 prefix ("dir/c/") which fills 3.
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
	// There are still objects remaining, so IsTruncated should be true
	if !result.IsTruncated {
		t.Error("expected IsTruncated=true since dir/top.txt remains")
	}
}

// TestListObjects_DelimiterDedup verifies the list objects delimiter dedup contract.
// Asserts that ListObjects:.
func TestListObjects_DelimiterDedup(t *testing.T) {
	t.Parallel()
	// Objects in the same virtual directory across pages should not produce
	// duplicate common prefixes.
	store := &mockStore{
		listObjectsPages: []core.ListObjectsResult{
			{
				Objects: []core.ObjectLocation{
					{ObjectKey: "p/a/1", BackendName: "b1"},
					{ObjectKey: "p/a/2", BackendName: "b1"},
				},
				IsTruncated: true,
			},
			{
				// Same prefix continues into the next page
				Objects: []core.ObjectLocation{
					{ObjectKey: "p/a/3", BackendName: "b1"},
					{ObjectKey: "p/b/1", BackendName: "b1"},
				},
				IsTruncated: false,
			},
		},
	}
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

// TestListObjects_DelimiterTruncationSkipsSeen verifies the list objects delimiter truncation skips seen contract.
// Asserts that ListObjects:.
func TestListObjects_DelimiterTruncationSkipsSeen(t *testing.T) {
	t.Parallel()
	// Regression: when maxKeys is reached and remaining objects belong to
	// an already-counted CommonPrefix, the continuation token must land
	// past the entire prefix group so the next page doesn't re-emit it.
	store := &mockStore{
		listObjectsResp: &core.ListObjectsResult{
			Objects: []core.ObjectLocation{
				{ObjectKey: "a/1", BackendName: "b1"},
				{ObjectKey: "a/2", BackendName: "b1"},
				{ObjectKey: "a/3", BackendName: "b1"},
				{ObjectKey: "b/1", BackendName: "b1"},
			},
			IsTruncated: false,
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	// maxKeys=1 with delimiter: should emit one CommonPrefix ("a/") and
	// set the token past a/3 so the next page starts at b/1.
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
	// Token must be past the last object in the "a/" group
	if result.NextContinuationToken != "a/3" {
		t.Errorf("NextContinuationToken = %q, want %q", result.NextContinuationToken, "a/3")
	}
}

// TestListObjects_ExactPageTruncation verifies the list objects exact page truncation contract.
// Asserts that ListObjects:.
func TestListObjects_ExactPageTruncation(t *testing.T) {
	t.Parallel()
	// Regression: when the store returns exactly maxKeys objects with
	// IsTruncated=true, the manager must propagate truncation. Previously
	// the outer loop exited (KeyCount == maxKeys) without marking
	// IsTruncated, so clients never fetched subsequent pages.
	objs := make([]core.ObjectLocation, 3)
	for i := range objs {
		objs[i] = core.ObjectLocation{
			ObjectKey:   fmt.Sprintf("pfx/%03d", i),
			BackendName: "b1",
			SizeBytes:   100,
		}
	}
	store := &mockStore{
		listObjectsResp: &core.ListObjectsResult{
			Objects:     objs,
			IsTruncated: true,
		},
	}
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

// TestAdvancePastEmittedCommonPrefix_TableDriven covers every branch of the
// helper that rewrites pagination cursors so a CommonPrefix already returned
// in the current call cannot be re-emitted by the next.
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
		{
			name:   "empty delimiter returns cursor unchanged",
			cursor: "tenant/a/k1", delimiter: "",
			want: "tenant/a/k1",
		},
		{
			name:      "empty cursor returns cursor unchanged",
			delimiter: "/", cursor: "",
			want: "",
		},
		{
			name:   "cursor not under prefix returns unchanged",
			prefix: "users/", delimiter: "/", cursor: "other/x",
			seen: map[string]bool{"users/0010/": true},
			want: "other/x",
		},
		{
			name:   "no delimiter in cursor's tail returns unchanged",
			prefix: "users/", delimiter: "/", cursor: "users/standalone-key",
			seen: map[string]bool{},
			want: "users/standalone-key",
		},
		{
			name:   "cursor inside un-emitted CP returns unchanged",
			prefix: "users/", delimiter: "/", cursor: "users/0010/k1",
			seen: map[string]bool{},
			want: "users/0010/k1",
		},
		{
			// "/" (0x2F) + 1 = "0" (0x30); CP "users/0010/" -> "users/00100"
			name:   "cursor inside emitted CP advances past group",
			prefix: "users/", delimiter: "/", cursor: "users/0010/k99",
			seen: map[string]bool{"users/0010/": true},
			want: "users/00100",
		},
		{
			// last byte "-" (0x2D) -> "." (0x2E); CP "u-0010--" -> "u-0010-."
			name:   "multi-byte delimiter advances correctly",
			prefix: "u-", delimiter: "--", cursor: "u-0010--k1",
			seen: map[string]bool{"u-0010--": true},
			want: "u-0010-.",
		},
		{
			name:   "0xff last byte cannot advance, returns cursor unchanged",
			prefix: "p", delimiter: "\xff", cursor: "p\xffk",
			seen: map[string]bool{"p\xff": true},
			want: "p\xffk",
		},
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

// TestListObjects_PageBoundaryMidCommonPrefix is the regression test for
// the cross-call CommonPrefix re-emission bug. With a deep prefix group
// whose keys span store-page boundaries and a maxKeys that aligns the
// boundary inside the group, the post-loop NextContinuationToken used
// to be the mid-group object key  -  and the next ListObjects call with
// that token re-emitted the same CommonPrefix because its `seen` map
// is local to a single call. The fix rewrites the cursor to the
// lex-upper-bound of the group so the next page skips it cleanly.
func TestListObjects_PageBoundaryMidCommonPrefix(t *testing.T) {
	t.Parallel()
	// Page 1 finishes the "a/" group quickly; page 2 opens "b/" and
	// reports more data. With maxKeys=2 the outer loop accumulates two
	// CommonPrefixes (a/ and b/) and exits because KeyCount == maxKeys,
	// while the store still has more b/* keys queued  -  the post-loop
	// truncation branch fires.
	store := &mockStore{
		listObjectsPages: []core.ListObjectsResult{
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
		},
	}
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
	// Without the fix, NextContinuationToken would be "b/3" (mid-b/ group)
	// and the next call would re-emit "b/" against a fresh seen map.
	// The fix advances to "b" + (delimiter+1) = "b0".
	if result.NextContinuationToken != "b0" {
		t.Errorf("NextContinuationToken = %q, want %q (advanced past b/ group)", result.NextContinuationToken, "b0")
	}
}

// TestListObjects_MaxPagesCapMidCommonPrefix exercises the second emission
// site (maxPages cap reached while still inside an emitted CP). Lowers the
// cap so the test only needs a handful of mock pages to drive the branch.
//
// Not t.Parallel(): mutates the package-global listObjectsMaxPages, which
// every concurrent ListObjects caller in this package reads.
func TestListObjects_MaxPagesCapMidCommonPrefix(t *testing.T) {
	originalCap := listObjectsMaxPages
	listObjectsMaxPages = 2
	defer func() { listObjectsMaxPages = originalCap }()

	// Both store pages return only "users/0001/<k>" keys: the inner loop
	// emits CP "users/0001/" once, then silently skips; the outer loop
	// hits the maxPages=2 cap with the store still reporting more data.
	store := &mockStore{
		listObjectsPages: []core.ListObjectsResult{
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
		},
	}
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
	// Without the fix the token would be "users/0001/k04" (mid-group) and
	// the next call would re-emit "users/0001/". The fix advances to the
	// CP's lex-upper-bound: "users/0001/" with last byte "/" (0x2F)
	// replaced by "0" (0x30) -> "users/00010".
	if result.NextContinuationToken != "users/00010" {
		t.Errorf("NextContinuationToken = %q, want %q (advanced past users/0001/ group)",
			result.NextContinuationToken, "users/00010")
	}
}

// TestListObjects_CrossCallWalkDoesNotDuplicateCommonPrefix simulates a
// real S3 client paginating through ListObjects and asserts the
// CommonPrefix the first call emitted is not re-emitted by the second.
// Direct demonstration that the cursor rewrite restores forward progress.
func TestListObjects_CrossCallWalkDoesNotDuplicateCommonPrefix(t *testing.T) {
	t.Parallel()
	// Six pages chained in mock order so each ListObjects call consumes
	// exactly the pages it would in production. With maxKeys=2 and the
	// b/ group spanning two store batches, the first call emits [a/, b/]
	// and exits via the post-loop branch; the second call must not
	// re-emit b/ when handed the (advanced) token.
	store := &mockStore{
		listObjectsPages: []core.ListObjectsResult{
			// First call's pages
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
			// Second call's pages  -  store is queried with startAfter="b0"
			// so it returns only c/ and beyond.
			{
				Objects: []core.ObjectLocation{
					{ObjectKey: "c/1", BackendName: "b1"},
					{ObjectKey: "d/1", BackendName: "b1"},
				},
				IsTruncated: false,
			},
		},
	}
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

	// Union of CommonPrefixes across both calls must contain no duplicates.
	combined := append([]string{}, first.CommonPrefixes...)
	combined = append(combined, second.CommonPrefixes...)
	seen := map[string]bool{}
	for _, cp := range combined {
		if seen[cp] {
			t.Errorf("CommonPrefix %q emitted twice across paginated calls", cp)
		}
		seen[cp] = true
	}
	// Sanity: second call should produce c/ and d/, never re-encounter b/.
	for _, cp := range second.CommonPrefixes {
		if cp == "b/" {
			t.Error("second call re-emitted b/  -  cross-call dedup broken")
		}
	}
}

// TestListObjects_DBUnavailable verifies the list objects dbunavailable contract.
// Asserts that expected st.S3Error, got :.
func TestListObjects_DBUnavailable(t *testing.T) {
	t.Parallel()
	store := &mockStore{listObjectsErr: core.ErrDBUnavailable}
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

// -------------------------------------------------------------------------
// Backend Timeout
// -------------------------------------------------------------------------

// TestPutObject_BackendTimeout verifies the put object backend timeout contract.
// Asserts that expected context.DeadlineExceeded, got.
func TestPutObject_BackendTimeout(t *testing.T) {
	t.Parallel()
	backend := &mockBackend{
		objects: make(map[string]mockObject),
		putErr:  nil,
	}
	// Override PutObject behavior with a slow backend via a wrapper
	slowBackend := &slowMockBackend{mockBackend: backend, delay: 200 * time.Millisecond}

	store := &mockStore{getBackendResp: "b1"}
	obs := map[string]s3be.ObjectBackend{"b1": slowBackend}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        obs,
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

// slowMockBackend wraps a mockBackend and adds a delay to PutObject.
type slowMockBackend struct {
	*mockBackend
	delay time.Duration
}

// PutObject satisfies backend.ObjectBackend for the manager-test
// fakes; records the call args and returns the test-configured
// error or success.
func (s *slowMockBackend) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string, metadata map[string]string) (string, error) {
	select {
	case <-time.After(s.delay):
		return s.mockBackend.PutObject(ctx, key, body, size, contentType, metadata)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// -------------------------------------------------------------------------
// Location Cache
// -------------------------------------------------------------------------

// TestLocationCache_SetAndGet verifies the location cache set and get contract.
// Asserts that cached backend = , want.
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

// TestLocationCache_Expiry verifies the location cache expiry path by exercising mgr.Close, time.Sleep.
func TestLocationCache_Expiry(t *testing.T) {
	t.Parallel()
	mgr := NewBackendManager(&BackendManagerConfig{CacheTTL: 10 * time.Millisecond, RoutingStrategy: config.RoutingPack})
	wireWorkersForTest(mgr)
	wireWorkersForTest(mgr)
	defer mgr.Close()
	mgr.ObjectManager.cache.Set("key1", "backend-a")

	time.Sleep(15 * time.Millisecond)

	_, ok := mgr.ObjectManager.cache.Get("key1")
	if ok {
		t.Fatal("expected cache miss after TTL")
	}
}

// TestLocationCache_Overwrite verifies the location cache overwrite contract.
// Asserts that cached backend = , want.
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

// TestPutObject_InvalidatesCache verifies the put object invalidates cache contract.
// Asserts that PutObject:.
func TestPutObject_InvalidatesCache(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{getBackendResp: "b1"}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})
	defer mgr.Close()

	// Pre-populate cache
	mgr.ObjectManager.cache.Set("mykey", "old-backend")

	_, err := mgr.ObjectManager.PutObject(context.Background(), "mykey", bytes.NewReader([]byte("hello")), 5, "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Cache entry should be gone after PutObject
	if _, ok := mgr.ObjectManager.cache.Get("mykey"); ok {
		t.Error("cache should be invalidated after PutObject")
	}
}

// TestDeleteObject_InvalidatesCache verifies the delete object invalidates cache contract.
// Asserts that DeleteObject:.
func TestDeleteObject_InvalidatesCache(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "del-key", bytes.NewReader([]byte("rm")), 2, "", nil)
	store := &mockStore{
		deleteObjectResp: []core.DeletedCopy{{BackendName: "b1", SizeBytes: 2}},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})
	defer mgr.Close()

	// Pre-populate cache
	mgr.ObjectManager.cache.Set("del-key", "b1")

	err := mgr.ObjectManager.DeleteObject(context.Background(), "del-key")
	if err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	// Cache entry should be gone after DeleteObject
	if _, ok := mgr.ObjectManager.cache.Get("del-key"); ok {
		t.Error("cache should be invalidated after DeleteObject")
	}
}

// -------------------------------------------------------------------------
// Usage Limit Enforcement
// -------------------------------------------------------------------------

// newTestManagerWithLimits constructs a new test manager with limits.
func newTestManagerWithLimits(store *mockStore, backends map[string]*mockBackend, limits map[string]core.UsageLimits) *BackendManager {
	obs := make(map[string]s3be.ObjectBackend, len(backends))
	var order []string
	for name, b := range backends {
		obs[name] = b
		order = append(order, name)
	}
	return wireWorkersForTest(NewBackendManager(&BackendManagerConfig{
		Backends:        obs,
		Stores:          testStoresFromMock(store),
		Dashboard:       store,
		Metrics:         store,
		Order:           order,
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		UsageLimits:     limits,
		RoutingStrategy: config.RoutingPack,
	}))
}

// TestPutObject_UsageLimitOverflow verifies the put object usage limit overflow contract.
// Asserts that PutObject should overflow to b2:.
func TestPutObject_UsageLimitOverflow(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()

	// b1 over API limit, b2 still has room
	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 10},
		"b2": {APIRequestLimit: 100},
	}
	store := &mockStore{getBackendResp: "b2"}
	mgr := newTestManagerWithLimits(store, map[string]*mockBackend{"b1": b1, "b2": b2}, limits)

	// Push b1 over limit
	mgr.usage.SetBaseline("b1", core.UsageStat{APIRequests: 10})

	etag, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	if err != nil {
		t.Fatalf("PutObject should overflow to b2: %v", err)
	}
	if etag == "" {
		t.Error("expected non-empty etag")
	}
	// Object should be on b2, not b1
	if b1.hasObject("key") {
		t.Error("object should NOT be on b1 (over limit)")
	}
	if !b2.hasObject("key") {
		t.Error("object should be on b2 (overflow)")
	}
}

// TestGetObject_UsageLimitSkipsBackend verifies the get object usage limit skips backend contract.
// Asserts that GetObject should skip b1 and use b2:.
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
	store := &mockStore{
		getAllLocationsResp: []core.ObjectLocation{
			{ObjectKey: "key", BackendName: "b1"},
			{ObjectKey: "key", BackendName: "b2"},
		},
	}
	mgr := newTestManagerWithLimits(store, map[string]*mockBackend{"b1": b1, "b2": b2}, limits)

	// Push b1 over limit
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

// TestGetObject_AllCopiesOverLimit verifies the get object all copies over limit contract.
// Asserts that expected st.ErrUsageLimitExceeded, got.
func TestGetObject_AllCopiesOverLimit(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 10},
	}
	store := &mockStore{
		getAllLocationsResp: []core.ObjectLocation{
			{ObjectKey: "key", BackendName: "b1"},
		},
	}
	mgr := newTestManagerWithLimits(store, map[string]*mockBackend{"b1": b1}, limits)

	mgr.usage.SetBaseline("b1", core.UsageStat{APIRequests: 10})

	_, err := mgr.ObjectManager.GetObject(context.Background(), "key", "")
	if !errors.Is(err, core.ErrUsageLimitExceeded) {
		t.Fatalf("expected st.ErrUsageLimitExceeded, got %v", err)
	}
}

// TestDeleteObject_AlwaysAllowed verifies the delete object always allowed contract.
// Asserts that DeleteObject should always succeed regardless of limits:.
func TestDeleteObject_AlwaysAllowed(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	_, _ = backend.PutObject(context.Background(), "del-key", bytes.NewReader([]byte("rm")), 2, "", nil)

	// All limits exceeded
	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 1, EgressByteLimit: 1, IngressByteLimit: 1},
	}
	store := &mockStore{
		deleteObjectResp: []core.DeletedCopy{{BackendName: "b1", SizeBytes: 2}},
	}
	mgr := newTestManagerWithLimits(store, map[string]*mockBackend{"b1": backend}, limits)

	mgr.usage.SetBaseline("b1", core.UsageStat{APIRequests: 100, EgressBytes: 100, IngressBytes: 100})

	err := mgr.ObjectManager.DeleteObject(context.Background(), "del-key")
	if err != nil {
		t.Fatalf("DeleteObject should always succeed regardless of limits: %v", err)
	}
	if backend.hasObject("del-key") {
		t.Error("object should be deleted from backend")
	}
}

// -------------------------------------------------------------------------
// Usage Limit Rejection Metric
// -------------------------------------------------------------------------

// TestPutObject_UsageLimitRejectionsMetric verifies the put object usage limit rejections metric contract.
// Asserts that UsageLimitRejectionsTotal[PutObject,write] did not increment: before=, after=.
func TestPutObject_UsageLimitRejectionsMetric(t *testing.T) {
	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 10},
	}
	store := &mockStore{getBackendResp: "b1"}
	mgr := newTestManagerWithLimits(store, map[string]*mockBackend{"b1": newMockBackend()}, limits)
	defer mgr.Close()

	mgr.usage.SetBaseline("b1", core.UsageStat{APIRequests: 10})

	before := testutil.ToFloat64(telemetry.UsageLimitRejectionsTotal.WithLabelValues("PutObject", "write"))

	_, err := mgr.ObjectManager.PutObject(context.Background(), "key", bytes.NewReader([]byte("x")), 1, "text/plain", nil)
	if err == nil {
		t.Fatal("expected error from PutObject with all backends over limit")
	}

	after := testutil.ToFloat64(telemetry.UsageLimitRejectionsTotal.WithLabelValues("PutObject", "write"))
	if after <= before {
		t.Errorf("UsageLimitRejectionsTotal[PutObject,write] did not increment: before=%v, after=%v", before, after)
	}
}

// TestGetObject_UsageLimitRejectionsMetric verifies the get object usage limit rejections metric contract.
// Asserts that expected st.ErrUsageLimitExceeded, got.
func TestGetObject_UsageLimitRejectionsMetric(t *testing.T) {
	b1 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	limits := map[string]core.UsageLimits{
		"b1": {APIRequestLimit: 10},
	}
	store := &mockStore{
		getAllLocationsResp: []core.ObjectLocation{
			{ObjectKey: "key", BackendName: "b1"},
		},
	}
	mgr := newTestManagerWithLimits(store, map[string]*mockBackend{"b1": b1}, limits)
	defer mgr.Close()

	mgr.usage.SetBaseline("b1", core.UsageStat{APIRequests: 10})

	before := testutil.ToFloat64(telemetry.UsageLimitRejectionsTotal.WithLabelValues("GetObject", "read"))

	_, err := mgr.ObjectManager.GetObject(context.Background(), "key", "")
	if !errors.Is(err, core.ErrUsageLimitExceeded) {
		t.Fatalf("expected st.ErrUsageLimitExceeded, got %v", err)
	}

	after := testutil.ToFloat64(telemetry.UsageLimitRejectionsTotal.WithLabelValues("GetObject", "read"))
	if after <= before {
		t.Errorf("UsageLimitRejectionsTotal[GetObject,read] did not increment: before=%v, after=%v", before, after)
	}
}

// -------------------------------------------------------------------------
// Parallel Broadcast Reads
// -------------------------------------------------------------------------

// newTestManagerParallel creates a BackendManager with parallel broadcast enabled
// and explicit backend ordering.
func newTestManagerParallel(store *mockStore, orderedBackends []struct {
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

// slowGetBackend wraps a mockBackend and adds a delay to GetObject and HeadObject.
type slowGetBackend struct {
	*mockBackend
	delay time.Duration
}

// GetObject returns object.
func (s *slowGetBackend) GetObject(ctx context.Context, key string, rangeHeader string) (*s3be.GetObjectResult, error) {
	select {
	case <-time.After(s.delay):
		return s.mockBackend.GetObject(ctx, key, rangeHeader)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// HeadObject satisfies backend.ObjectBackend for the manager-test
// fakes; returns either the configured fixture metadata or the
// configured error.
func (s *slowGetBackend) HeadObject(ctx context.Context, key string) (*s3be.HeadObjectResult, error) {
	select {
	case <-time.After(s.delay):
		return s.mockBackend.HeadObject(ctx, key)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestGetObject_ParallelBroadcast_FirstSuccessWins verifies the get object parallel broadcast first success wins contract.
// Asserts that parallel broadcast should succeed:.
func TestGetObject_ParallelBroadcast_FirstSuccessWins(t *testing.T) {
	t.Parallel()
	slow := newMockBackend()
	fast := newMockBackend()
	_, _ = slow.PutObject(context.Background(), "key", bytes.NewReader([]byte("slow-data")), 9, "text/plain", nil)
	_, _ = fast.PutObject(context.Background(), "key", bytes.NewReader([]byte("fast-data")), 9, "text/plain", nil)

	store := &mockStore{getAllLocationsErr: core.ErrDBUnavailable}
	// slow backend is first in order, but fast backend should win
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

	// Should be much faster than 200ms (the slow backend's delay)
	if elapsed > 150*time.Millisecond {
		t.Errorf("parallel broadcast took %v, expected < 150ms", elapsed)
	}
}

// TestGetObject_ParallelBroadcast_AllFail verifies the get object parallel broadcast all fail path by exercising mgr.Close, context.Background, errors.Is.
func TestGetObject_ParallelBroadcast_AllFail(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend() // empty  -  no object
	b2 := newMockBackend() // empty  -  no object

	store := &mockStore{getAllLocationsErr: core.ErrDBUnavailable}
	mgr := newTestManagerParallel(store, []struct {
		name    string
		backend s3be.ObjectBackend
	}{
		{"b1", b1},
		{"b2", b2},
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

// TestGetObject_ParallelBroadcast_CacheHitSkipsParallel verifies the get object parallel broadcast cache hit skips parallel contract.
// Asserts that first GetObject:.
func TestGetObject_ParallelBroadcast_CacheHitSkipsParallel(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	_, _ = b2.PutObject(context.Background(), "cached-key", bytes.NewReader([]byte("cached")), 6, "text/plain", nil)

	store := &mockStore{getAllLocationsErr: core.ErrDBUnavailable}
	mgr := newTestManagerParallel(store, []struct {
		name    string
		backend s3be.ObjectBackend
	}{
		{"b1", b1},
		{"b2", b2},
	})
	defer mgr.Close()

	// First call populates cache via parallel broadcast
	r1, err := mgr.ObjectManager.GetObject(context.Background(), "cached-key", "")
	if err != nil {
		t.Fatalf("first GetObject: %v", err)
	}
	_ = r1.Body.Close()

	// Second call should use cache (not broadcast again)
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

// TestGetObject_SequentialBroadcast_WhenDisabled verifies the get object sequential broadcast when disabled contract.
// Asserts that sequential broadcast should succeed:.
func TestGetObject_SequentialBroadcast_WhenDisabled(t *testing.T) {
	t.Parallel()
	slow := newMockBackend()
	fast := newMockBackend()
	_, _ = slow.PutObject(context.Background(), "key", bytes.NewReader([]byte("slow-data")), 9, "text/plain", nil)
	_, _ = fast.PutObject(context.Background(), "key", bytes.NewReader([]byte("fast-data")), 9, "text/plain", nil)

	store := &mockStore{getAllLocationsErr: core.ErrDBUnavailable}
	// ParallelBroadcast=false (default), slow is first in order
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
	// Sequential: slow backend is tried first and succeeds (just slowly)
	if string(got) != "slow-data" {
		t.Errorf("body = %q, want %q (slow backend tried first sequentially)", got, "slow-data")
	}

	// Sequential should take at least 100ms (the slow backend's delay)
	if elapsed < 100*time.Millisecond {
		t.Errorf("sequential broadcast took %v, expected >= 100ms", elapsed)
	}
}

// -------------------------------------------------------------------------
// withReadFailover edge cases
// -------------------------------------------------------------------------

// TestGetObject_BackendNotFound_FailsOverToNext verifies the get object backend not found fails over to next contract.
// Asserts that GetObject should failover past missing backend:.
func TestGetObject_BackendNotFound_FailsOverToNext(t *testing.T) {
	t.Parallel()
	b2 := newMockBackend()
	_, _ = b2.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "text/plain", nil)

	// Location references "gone-backend" which doesn't exist in backends map
	store := &mockStore{
		getAllLocationsResp: []core.ObjectLocation{
			{ObjectKey: "key", BackendName: "gone-backend"},
			{ObjectKey: "key", BackendName: "b2"},
		},
	}
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

// TestGetObject_GenericStoreError verifies the get object generic store error path by exercising errors.New, context.Background, errors.Is.
func TestGetObject_GenericStoreError(t *testing.T) {
	t.Parallel()
	store := &mockStore{getAllLocationsErr: errors.New("unexpected db error")}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.ObjectManager.GetObject(context.Background(), "key", "")
	if err == nil {
		t.Fatal("expected error from generic store failure")
	}
	// Should NOT be st.ErrObjectNotFound or st.ErrServiceUnavailable
	if errors.Is(err, core.ErrObjectNotFound) {
		t.Error("should not be st.ErrObjectNotFound")
	}
}

// TestGetObject_DBUnavailable_CacheHitFails_FallsThrough verifies the get object dbunavailable cache hit fails falls through contract.
// Asserts that should fall through to broadcast after cache hit failure:.
func TestGetObject_DBUnavailable_CacheHitFails_FallsThrough(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b2 := newMockBackend()
	_, _ = b2.PutObject(context.Background(), "key", bytes.NewReader([]byte("from-b2")), 7, "text/plain", nil)

	store := &mockStore{getAllLocationsErr: core.ErrDBUnavailable}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": b1, "b2": b2})

	// Pre-populate cache pointing to b1 (which does NOT have the object)
	mgr.ObjectManager.cache.Set("key", "b1")

	// Cache hit on b1 should fail, then broadcast should find it on b2
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

// -------------------------------------------------------------------------
// DeleteObject edge cases
// -------------------------------------------------------------------------

// TestDeleteObject_BackendNotFound_ContinuesOtherCopies verifies the delete object backend not found continues other copies contract.
// Asserts that DeleteObject should succeed even with missing backend:.
func TestDeleteObject_BackendNotFound_ContinuesOtherCopies(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	_, _ = b1.PutObject(context.Background(), "key", bytes.NewReader([]byte("data")), 4, "", nil)

	store := &mockStore{
		deleteObjectResp: []core.DeletedCopy{
			{BackendName: "gone-backend", SizeBytes: 4},
			{BackendName: "b1", SizeBytes: 4},
		},
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": b1})

	err := mgr.ObjectManager.DeleteObject(context.Background(), "key")
	if err != nil {
		t.Fatalf("DeleteObject should succeed even with missing backend: %v", err)
	}
	// b1 copy should still be deleted
	if b1.hasObject("key") {
		t.Error("expected b1 copy to be deleted")
	}
}

// -------------------------------------------------------------------------
// CopyObject edge cases
// -------------------------------------------------------------------------

// TestCopyObject_AllSourceHeadsFail verifies the copy object all source heads fail path by exercising errors.New, context.Background.
func TestCopyObject_AllSourceHeadsFail(t *testing.T) {
	t.Parallel()
	b1 := newMockBackend()
	b1.headErr = errors.New("head failed")

	store := &mockStore{
		getAllLocationsResp: []core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}},
		getBackendResp:      "b1",
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": b1})

	_, err := mgr.ObjectManager.CopyObject(context.Background(), "src", "dst")
	if err == nil {
		t.Fatal("expected error when all source HeadObjects fail")
	}
}

// TestCopyObject_DestWriteFails verifies the copy object dest write fails path by exercising src.PutObject, context.Background, bytes.NewReader.
func TestCopyObject_DestWriteFails(t *testing.T) {
	t.Parallel()
	src := newMockBackend()
	_, _ = src.PutObject(context.Background(), "src", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dst := newMockBackend()
	dst.putErr = errors.New("write failed")

	store := &mockStore{
		getAllLocationsResp: []core.ObjectLocation{{ObjectKey: "src", BackendName: "src-be"}},
		getBackendResp:      "dst-be",
	}
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

	_, err := mgr.ObjectManager.CopyObject(context.Background(), "src", "dst")
	if err == nil {
		t.Fatal("expected error when dest PutObject fails")
	}
}

// TestCopyObject_ExcludesDrainingBackend verifies the copy object excludes draining backend contract.
// Asserts that expected st.ErrInsufficientStorage when all backends are draining, got.
func TestCopyObject_ExcludesDrainingBackend(t *testing.T) {
	t.Parallel()
	src := newMockBackend()
	_, _ = src.PutObject(context.Background(), "src", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	dst := newMockBackend()

	store := &mockStore{
		getAllLocationsResp: []core.ObjectLocation{{ObjectKey: "src", BackendName: "src-be"}},
		getBackendResp:      "dst-be",
	}
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

	// Mark both backends as draining so no eligible destination remains
	mgr.DrainManager.SeedActiveForTest("src-be")
	mgr.DrainManager.SeedActiveForTest("dst-be")

	// CopyObject should fail  -  all backends are draining
	_, err := mgr.ObjectManager.CopyObject(context.Background(), "src", "dst")
	if !errors.Is(err, core.ErrInsufficientStorage) {
		t.Fatalf("expected st.ErrInsufficientStorage when all backends are draining, got %v", err)
	}

	// dst should NOT have the object
	if dst.hasObject("dst") {
		t.Error("object should not have been copied to draining backend")
	}
}

// TestCopyObject_SourceReadFails verifies the copy object source read fails path by exercising src.PutObject, context.Background, bytes.NewReader.
func TestCopyObject_SourceReadFails(t *testing.T) {
	t.Parallel()
	src := newMockBackend()
	_, _ = src.PutObject(context.Background(), "src", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	src.getReadErr = errors.New("disk I/O error")

	store := &mockStore{
		getAllLocationsResp: []core.ObjectLocation{{ObjectKey: "src", BackendName: "src-be"}},
		getBackendResp:      "dst-be",
	}
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

	_, err := mgr.ObjectManager.CopyObject(context.Background(), "src", "dst")
	if err == nil {
		t.Fatal("expected error when source body read fails")
	}
}

// TestCopyObject_AllSourceGetObjectsFail verifies the copy object all source get objects fail path by exercising src.PutObject, context.Background, bytes.NewReader.
func TestCopyObject_AllSourceGetObjectsFail(t *testing.T) {
	t.Parallel()
	src := newMockBackend()
	_, _ = src.PutObject(context.Background(), "src", bytes.NewReader([]byte("data")), 4, "text/plain", nil)
	src.getErr = errors.New("get unavailable")

	store := &mockStore{
		getAllLocationsResp: []core.ObjectLocation{{ObjectKey: "src", BackendName: "src-be"}},
		getBackendResp:      "dst-be",
	}
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

	_, err := mgr.ObjectManager.CopyObject(context.Background(), "src", "dst")
	if err == nil {
		t.Fatal("expected error when all source GetObjects fail")
	}
}

// -------------------------------------------------------------------------
// ListObjects edge cases
// -------------------------------------------------------------------------

// TestListObjects_GenericError verifies the list objects generic error contract.
// Asserts that generic error should not be st.S3Error, got v.
func TestListObjects_GenericError(t *testing.T) {
	t.Parallel()
	store := &mockStore{listObjectsErr: errors.New("unexpected query error")}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": newMockBackend()})

	_, err := mgr.ObjectManager.ListObjects(context.Background(), "", "", "", 1000)
	if err == nil {
		t.Fatal("expected error from generic store failure")
	}
	// Should NOT be an st.S3Error with 503 (that's for DB unavailable)
	var s3err *core.S3Error
	if errors.As(err, &s3err) {
		t.Errorf("generic error should not be st.S3Error, got %+v", s3err)
	}
}

// TestHeadObject_ParallelBroadcast verifies the head object parallel broadcast contract.
// Asserts that HeadObject parallel broadcast should succeed:.
func TestHeadObject_ParallelBroadcast(t *testing.T) {
	t.Parallel()
	slow := newMockBackend()
	fast := newMockBackend()
	_, _ = slow.PutObject(context.Background(), "key", bytes.NewReader([]byte("head")), 4, "text/plain", nil)
	_, _ = fast.PutObject(context.Background(), "key", bytes.NewReader([]byte("head")), 4, "text/plain", nil)

	store := &mockStore{getAllLocationsErr: core.ErrDBUnavailable}
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

// -------------------------------------------------------------------------
// parsePlaintextRange
// -------------------------------------------------------------------------

// TestParsePlaintextRange_SuffixLargerThanFile verifies the parse plaintext range suffix larger than file contract.
// Asserts that start = , want 0 (clamped).
func TestParsePlaintextRange_SuffixLargerThanFile(t *testing.T) {
	t.Parallel()
	// bytes=-1000 on a 100-byte file should clamp start to 0
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

// TestParsePlaintextRange_ClampsEndToSize verifies the parse plaintext range clamps end to size contract.
// Asserts that start = , want 0.
func TestParsePlaintextRange_ClampsEndToSize(t *testing.T) {
	t.Parallel()
	// Explicit range where end exceeds plaintextSize
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

// TestParsePlaintextRange_ExactEndNotClamped verifies the parse plaintext range exact end not clamped contract.
// Asserts that start= end=, want 0,99.
func TestParsePlaintextRange_ExactEndNotClamped(t *testing.T) {
	t.Parallel()
	// End is exactly the last byte  -  should not be clamped
	start, end, ok := parsePlaintextRange("bytes=0-99", 100)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if start != 0 || end != 99 {
		t.Errorf("start=%d end=%d, want 0,99", start, end)
	}
}

// TestParsePlaintextRange_InvertedRange verifies that an inverted range
// (start > end) is rejected per RFC 7233.
func TestParsePlaintextRange_InvertedRange(t *testing.T) {
	t.Parallel()
	_, _, ok := parsePlaintextRange("bytes=99-0", 100)
	if ok {
		t.Error("expected ok=false for inverted range")
	}
}

// TestParsePlaintextRange_StartBeyondFile verifies that a range starting
// past the end of the file is rejected.
func TestParsePlaintextRange_StartBeyondFile(t *testing.T) {
	t.Parallel()
	_, _, ok := parsePlaintextRange("bytes=100-200", 100)
	if ok {
		t.Error("expected ok=false when start >= plaintextSize")
	}
}

// TestParsePlaintextRange_OpenEndedBeyondFile verifies that an open-ended
// range starting past the end of the file is rejected.
func TestParsePlaintextRange_OpenEndedBeyondFile(t *testing.T) {
	t.Parallel()
	_, _, ok := parsePlaintextRange("bytes=100-", 100)
	if ok {
		t.Error("expected ok=false for open-ended range beyond file")
	}
}

// TestCopyObject_SourceGetPanics verifies that a panic inside the source-reader
// goroutine is recovered and surfaced as an error instead of deadlocking the
// request on the io.Pipe.
func TestCopyObject_SourceGetPanics(t *testing.T) {
	t.Parallel()
	srcBackend := newMockBackend()
	srcBackend.getPanic = true // causes GetObject to panic

	store := &mockStore{
		getAllLocationsResp: []core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}},
		getBackendResp:      "b1",
	}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": srcBackend})

	_, err := mgr.ObjectManager.CopyObject(context.Background(), "src", "dst")
	if err == nil {
		t.Fatal("expected error from panicking source reader, got nil")
	}
}

// -------------------------------------------------------------------------
// REDIS COUNTER PROBES
// -------------------------------------------------------------------------

// TestRedisCounterConfigured_LocalBackendReturnsFalse verifies that a
// manager wired with the default local counter backend reports
// RedisCounterConfigured = false. The flush service uses this probe to
// decide whether the advisory lock is required; a false here means no
// advisory lock is acquired, which is correct for single-instance
// deployments.
func TestRedisCounterConfigured_LocalBackendReturnsFalse(t *testing.T) {
	t.Parallel()
	backend := newMockBackend()
	store := &mockStore{}
	mgr := newTestManager(store, map[string]*mockBackend{"b1": backend})

	if mgr.RedisCounterConfigured() {
		t.Errorf("RedisCounterConfigured = true, want false for local counter backend")
	}
}
