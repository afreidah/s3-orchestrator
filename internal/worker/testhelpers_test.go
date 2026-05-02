package worker

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

// mockMetadataStore is a minimal stub for worker tests. It embeds every
// narrow store role as a nil interface so any worker signature accepts it;
// tests override only the methods they exercise, and any unstubbed call
// panics, which surfaces test-fixture gaps loudly.
type mockMetadataStore struct {
	store.ObjectStore
	store.QuotaStore
	store.MultipartStore
	store.ReplicationStore
	store.CleanupStore
	store.PendingStore
	store.IntegrityStore
	store.ExpiredObjectsLister
	store.BackendLifecycleStore
	store.DashboardStore
	store.UsageFlusher
	store.AdvisoryLocker
	pendingCleanups     []store.CleanupItem
	completedIDs        []int64
	randomHashedObjects []store.ObjectLocation
	objectsWithoutHash  []store.ObjectLocation
	lastUpdatedHash     string
	underReplicated     []store.ObjectLocation
	overReplicated      []store.ObjectLocation
	overReplicatedCount int64
	quotaStats          map[string]store.QuotaStat
	recordReplicaOK     bool
	recordReplicaErr    error
	replicaRecorded     int
	removedCopies       int
	objectsByBackend    map[string][]store.ObjectLocation
	moveSize            int64
	staleDeleted        int

	// Rebalancer planner fixtures: counts batch invocations and lets
	// tests override the (key -> backends) map returned by the new
	// batch query.
	getBackendsForKeysCalls int
	getBackendsForKeysResp  map[string][]string

	// Pending reaper fixtures
	stalePending           []store.PendingObject
	deletedPendingIDs      []string
	promotedPending        []store.PendingObject
	promoteResult          store.PendingPromoteResult
	promoteDisplaced       []store.DeletedCopy
	promoteErr             error
	pendingDepthVal        int64
	pendingDepthErr        error
}

func (m *mockMetadataStore) GetPendingCleanups(_ context.Context, _ int) ([]store.CleanupItem, error) {
	return m.pendingCleanups, nil
}

func (m *mockMetadataStore) CompleteCleanupItem(_ context.Context, id int64) error {
	m.completedIDs = append(m.completedIDs, id)
	return nil
}

func (m *mockMetadataStore) RetryCleanupItem(_ context.Context, _ int64, _ time.Duration, _ string) error {
	return nil
}

func (m *mockMetadataStore) DecrementOrphanBytes(_ context.Context, _ string, _ int64) error {
	return nil
}

func (m *mockMetadataStore) CleanupQueueDepth(_ context.Context) (int64, error) {
	return 0, nil
}

func (m *mockMetadataStore) GetRandomHashedObjects(_ context.Context, _ int) ([]store.ObjectLocation, error) {
	return m.randomHashedObjects, nil
}

func (m *mockMetadataStore) GetObjectsWithoutHash(_ context.Context, limit, _ int) ([]store.ObjectLocation, error) {
	if limit > len(m.objectsWithoutHash) {
		return m.objectsWithoutHash, nil
	}
	return m.objectsWithoutHash[:limit], nil
}

func (m *mockMetadataStore) UpdateContentHash(_ context.Context, _, _, hash string) error {
	m.lastUpdatedHash = hash
	return nil
}

// newTestUsageTracker creates a UsageTracker with no limits for testing.
func newTestUsageTracker() *counter.UsageTracker {
	return counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2"}), nil)
}

func (m *mockMetadataStore) GetUnderReplicatedObjects(_ context.Context, _, _ int) ([]store.ObjectLocation, error) {
	return m.underReplicated, nil
}

func (m *mockMetadataStore) GetUnderReplicatedObjectsExcluding(_ context.Context, _, _ int, _ []string) ([]store.ObjectLocation, error) {
	return m.underReplicated, nil
}

func (m *mockMetadataStore) GetQuotaStats(_ context.Context) (map[string]store.QuotaStat, error) {
	return m.quotaStats, nil
}

func (m *mockMetadataStore) RecordReplica(_ context.Context, _, _, _ string, _ int64) (bool, error) {
	m.replicaRecorded++
	if m.recordReplicaErr != nil {
		return false, m.recordReplicaErr
	}
	return m.recordReplicaOK, nil
}

func (m *mockMetadataStore) GetOverReplicatedObjects(_ context.Context, _, _ int) ([]store.ObjectLocation, error) {
	return m.overReplicated, nil
}

func (m *mockMetadataStore) CountOverReplicatedObjects(_ context.Context, _ int) (int64, error) {
	return m.overReplicatedCount, nil
}

func (m *mockMetadataStore) RemoveExcessCopy(_ context.Context, _, _ string, _ int64) error {
	m.removedCopies++
	return nil
}

func (m *mockMetadataStore) ListObjectsByBackend(_ context.Context, name string, _ int) ([]store.ObjectLocation, error) {
	return m.objectsByBackend[name], nil
}

func (m *mockMetadataStore) MoveObjectLocation(_ context.Context, _, _, _ string) (int64, error) {
	return m.moveSize, nil
}

func (m *mockMetadataStore) GetAllObjectLocations(_ context.Context, _ string) ([]store.ObjectLocation, error) {
	return nil, nil
}

func (m *mockMetadataStore) GetObjectBackendsForKeys(_ context.Context, _ []string) (map[string][]string, error) {
	m.getBackendsForKeysCalls++
	if m.getBackendsForKeysResp != nil {
		return m.getBackendsForKeysResp, nil
	}
	return map[string][]string{}, nil
}

func (m *mockMetadataStore) FlushUsageDeltas(_ context.Context, _, _ string, _, _, _ int64) error {
	return nil
}

func (m *mockMetadataStore) DeleteObjectLocation(_ context.Context, _, _ string) error {
	m.staleDeleted++
	return nil
}

// --- PendingStore stubs ---

// GetStalePending returns the configured fixture rows; the reaper batch
// limit is ignored to keep tests focused on resolution outcomes rather
// than pagination.
func (m *mockMetadataStore) GetStalePending(_ context.Context, _ time.Time, _ int) ([]store.PendingObject, error) {
	return m.stalePending, nil
}

// DeletePending records the intent ID so tests can assert reaper deletions.
func (m *mockMetadataStore) DeletePending(_ context.Context, intentID string) error {
	m.deletedPendingIDs = append(m.deletedPendingIDs, intentID)
	return nil
}

// PromotePending captures the input and returns the test's preconfigured
// resolution outcome. The captured slice lets tests verify the reaper
// passed the intent through unchanged.
func (m *mockMetadataStore) PromotePending(_ context.Context, p *store.PendingObject) (store.PendingPromoteResult, []store.DeletedCopy, error) {
	if p != nil {
		m.promotedPending = append(m.promotedPending, *p)
	}
	return m.promoteResult, m.promoteDisplaced, m.promoteErr
}

// PendingDepth returns the configured depth value (defaults to 0).
func (m *mockMetadataStore) PendingDepth(_ context.Context) (int64, error) {
	return m.pendingDepthVal, m.pendingDepthErr
}
