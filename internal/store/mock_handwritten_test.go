// Package store - shim over storetest.MockMetadataStore for CB tests.
package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// mockStore is a thin field-configurable shim over the canonical
// storetest.MockMetadataStore. The embedded mock supplies Permissive
// .AnyTimes() defaults for every interface method; mockStore overrides
// only the handful of methods whose return values or call counts the CB
// state-machine tests need to drive imperatively (mutate response/error
// between calls, count invocations across goroutines).
type mockStore struct {
	*storetest.MockMetadataStore

	mu        sync.Mutex
	callCount int

	getAllLocationsResp []core.ObjectLocation
	getAllLocationsErr  error

	listObjectsByBackendKeyAscResp []core.ObjectLocation
	listObjectsByBackendKeyAscErr  error

	getQuotaStatsResp map[string]core.QuotaStat

	getBackendsForKeysResp map[string][]string

	pendingDepthResp        int64
	getStalePendingResp     []core.PendingObject
	promotePendingResult    core.PendingPromoteResult
	promotePendingDisplaced []core.DeletedCopy
}

// newMockStore builds a mockStore wired to a Permissive
// storetest.MockMetadataStore. Each test owns its own *gomock.Controller
// (bound to t) so expectation assertions run during cleanup.
func newMockStore(t *testing.T) *mockStore {
	t.Helper()
	inner := storetest.NewMockMetadataStore(gomock.NewController(t))
	storetest.Permissive(inner)
	return &mockStore{MockMetadataStore: inner}
}

// GetAllObjectLocations overrides the embedded mock so tests can mutate
// the response/error between calls and observe a per-mock call count.
// CB state-machine tests rely on this method being the trip mechanism.
func (m *mockStore) GetAllObjectLocations(_ context.Context, _ string) ([]core.ObjectLocation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callCount++
	return m.getAllLocationsResp, m.getAllLocationsErr
}

// ListObjectsByBackendKeyAsc returns the configured response/error so the
// post-check coverage tests can assert the CB wrapper forwards rows
// verbatim and surfaces the inner error before the breaker trips.
func (m *mockStore) ListObjectsByBackendKeyAsc(_ context.Context, _, _ string, _ int) ([]core.ObjectLocation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listObjectsByBackendKeyAscResp, m.listObjectsByBackendKeyAscErr
}

// GetQuotaStats returns the configured map so TestCBForwarders_QuotaStore
// can assert the CB QuotaStore forwarder returns the inner store's
// values. Permissive's default is nil.
func (m *mockStore) GetQuotaStats(_ context.Context) (map[string]core.QuotaStat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getQuotaStatsResp, nil
}

// GetObjectBackendsForKeys returns the configured map so the pending-CB
// forwarder coverage test can verify the wrapper passes rows through.
func (m *mockStore) GetObjectBackendsForKeys(_ context.Context, _ []string) (map[string][]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getBackendsForKeysResp, nil
}

// PendingDepth returns the configured depth so the pending-CB forwarder
// coverage test can assert the wrapper threads the value through.
func (m *mockStore) PendingDepth(_ context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pendingDepthResp, nil
}

// GetStalePending returns the configured rows so the pending-CB
// forwarder coverage test can assert it gets back the row it set.
func (m *mockStore) GetStalePending(_ context.Context, _ time.Time, _ int) ([]core.PendingObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getStalePendingResp, nil
}

// PromotePending returns the configured result and displaced copies so
// the pending-CB forwarder coverage test can assert both fields are
// surfaced through the wrapper unchanged.
func (m *mockStore) PromotePending(_ context.Context, _ *core.PendingObject) (core.PendingPromoteResult, []core.DeletedCopy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.promotePendingResult, m.promotePendingDisplaced, nil
}
