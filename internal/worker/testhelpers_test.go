// -------------------------------------------------------------------------------
// Worker Test Helpers
//
// Author: Alex Freidah
//
// Shared mocks and fixtures for the worker package's unit tests:
// mockMetadataStore (a narrow store stub that embeds every role
// interface as nil so unstubbed calls panic loudly), the dlqMoveCall
// recorder used by cleanup-worker tests, and small constructors for
// the in-memory usage tracker. Lives in its own file so each
// per-worker test file stays focused on its scenarios.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// mockMetadataStore is a minimal stub for worker tests. It embeds every
// narrow store role as a nil interface so any worker signature accepts it;
// tests override only the methods they exercise, and any unstubbed call
// panics, which surfaces test-fixture gaps loudly.
type mockMetadataStore struct {
	core.ObjectStore
	core.QuotaStore
	core.MultipartStore
	core.ReplicationStore
	core.CleanupStore
	core.PendingStore
	core.IntegrityStore
	core.ExpiredObjectsLister
	core.BackendLifecycleStore
	core.DashboardStore
	core.UsageFlusher
	core.AdvisoryLocker
	pendingCleanups     []core.CleanupItem
	completedIDs        []int64
	dlqMoves            []dlqMoveCall
	moveDLQResult       bool
	moveDLQErr          error
	dlqDepthVal         int64
	dlqDepthErr         error
	randomHashedObjects []core.ObjectLocation
	objectsWithoutHash  []core.ObjectLocation
	lastUpdatedHash     string
	underReplicated     []core.ObjectLocation
	overReplicated      []core.ObjectLocation
	overReplicatedCount int64
	quotaStats          map[string]core.QuotaStat
	recordReplicaOK     bool
	recordReplicaSize   int64
	recordReplicaErr    error
	replicaRecorded     int
	removedCopies       int
	objectsByBackend    map[string][]core.ObjectLocation
	moveSize            int64
	staleDeleted        int

	// Rebalancer planner fixtures: counts batch invocations and lets
	// tests override the (key -> backends) map returned by the new
	// batch query.
	getBackendsForKeysCalls int
	getBackendsForKeysResp  map[string][]string

	// Pending reaper fixtures
	stalePending      []core.PendingObject
	deletedPendingIDs []string
	promotedPending   []core.PendingObject
	promoteResult     core.PendingPromoteResult
	promoteDisplaced  []core.DeletedCopy
	promoteErr        error
	pendingDepthVal   int64
	pendingDepthErr   error
}

// GetPendingCleanups is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetPendingCleanups(_ context.Context, _ int) ([]core.CleanupItem, error) {
	return m.pendingCleanups, nil
}

// CompleteCleanupItem is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) CompleteCleanupItem(_ context.Context, id int64) error {
	m.completedIDs = append(m.completedIDs, id)
	return nil
}

// RetryCleanupItem is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) RetryCleanupItem(_ context.Context, _ int64, _ time.Duration, _ string) error {
	return nil
}

// DecrementOrphanBytes is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) DecrementOrphanBytes(_ context.Context, _ string, _ int64) error {
	return nil
}

// CleanupQueueDepth is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) CleanupQueueDepth(_ context.Context) (int64, error) {
	return 0, nil
}

// dlqMoveCall records a MoveCleanupToDLQ invocation so cleanup-worker
// tests can assert which (id, last_error) tuples were graduated.
type dlqMoveCall struct {
	id        int64
	lastError string
}

// MoveCleanupToDLQ is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) MoveCleanupToDLQ(_ context.Context, id int64, lastError string) (bool, error) {
	m.dlqMoves = append(m.dlqMoves, dlqMoveCall{id: id, lastError: lastError})
	if m.moveDLQErr != nil {
		return false, m.moveDLQErr
	}
	if !m.moveDLQResult {
		// Default to true so the happy path requires no per-test setup.
		return true, nil
	}
	return m.moveDLQResult, nil
}

// CleanupDLQDepth is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) CleanupDLQDepth(_ context.Context) (int64, error) {
	return m.dlqDepthVal, m.dlqDepthErr
}

// GetRandomHashedObjects is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetRandomHashedObjects(_ context.Context, _ int) ([]core.ObjectLocation, error) {
	return m.randomHashedObjects, nil
}

// GetObjectsWithoutHash is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetObjectsWithoutHash(_ context.Context, limit, _ int) ([]core.ObjectLocation, error) {
	if limit > len(m.objectsWithoutHash) {
		return m.objectsWithoutHash, nil
	}
	return m.objectsWithoutHash[:limit], nil
}

// UpdateContentHash is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) UpdateContentHash(_ context.Context, _, _, hash string) error {
	m.lastUpdatedHash = hash
	return nil
}

// newTestUsageTracker creates a UsageTracker with no limits for testing.
func newTestUsageTracker() *counter.UsageTracker {
	return counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2"}), nil)
}

// GetUnderReplicatedObjects is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetUnderReplicatedObjects(_ context.Context, _, _ int) ([]core.ObjectLocation, error) {
	return m.underReplicated, nil
}

// GetUnderReplicatedObjectsExcluding is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetUnderReplicatedObjectsExcluding(_ context.Context, _, _ int, _ []string) ([]core.ObjectLocation, error) {
	return m.underReplicated, nil
}

// GetQuotaStats is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetQuotaStats(_ context.Context) (map[string]core.QuotaStat, error) {
	return m.quotaStats, nil
}

// RecordReplica is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) RecordReplica(_ context.Context, _, _, _ string) (int64, bool, error) {
	m.replicaRecorded++
	if m.recordReplicaErr != nil {
		return 0, false, m.recordReplicaErr
	}
	return m.recordReplicaSize, m.recordReplicaOK, nil
}

// GetOverReplicatedObjects is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetOverReplicatedObjects(_ context.Context, _, _ int) ([]core.ObjectLocation, error) {
	return m.overReplicated, nil
}

// CountOverReplicatedObjects is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) CountOverReplicatedObjects(_ context.Context, _ int) (int64, error) {
	return m.overReplicatedCount, nil
}

// RemoveExcessCopy is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) RemoveExcessCopy(_ context.Context, _, _ string, _ int64) error {
	m.removedCopies++
	return nil
}

// ListObjectsByBackend is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) ListObjectsByBackend(_ context.Context, name string, _ int) ([]core.ObjectLocation, error) {
	return m.objectsByBackend[name], nil
}

// MoveObjectLocation is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) MoveObjectLocation(_ context.Context, _, _, _ string) (int64, error) {
	return m.moveSize, nil
}

// GetAllObjectLocations is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetAllObjectLocations(_ context.Context, _ string) ([]core.ObjectLocation, error) {
	return nil, nil
}

// GetObjectBackendsForKeys is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetObjectBackendsForKeys(_ context.Context, _ []string) (map[string][]string, error) {
	m.getBackendsForKeysCalls++
	if m.getBackendsForKeysResp != nil {
		return m.getBackendsForKeysResp, nil
	}
	return map[string][]string{}, nil
}

// FlushUsageDeltas is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) FlushUsageDeltas(_ context.Context, _, _ string, _, _, _ int64) error {
	return nil
}

// DeleteObjectLocation is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) DeleteObjectLocation(_ context.Context, _, _ string) error {
	m.staleDeleted++
	return nil
}

// --- PendingStore stubs ---

// GetStalePending returns the configured fixture rows; the reaper batch
// limit is ignored to keep tests focused on resolution outcomes rather
// than is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetStalePending(_ context.Context, _ time.Time, _ int) ([]core.PendingObject, error) {
	return m.stalePending, nil
}

// DeletePending records the intent ID so tests can assert reaper deletions.
func (m *mockMetadataStore) DeletePending(_ context.Context, intentID string) error {
	m.deletedPendingIDs = append(m.deletedPendingIDs, intentID)
	return nil
}

// PromotePending captures the input and returns the test's preconfigured
// resolution outcome. The captured slice lets tests verify the reaper
// passed is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) PromotePending(_ context.Context, p *core.PendingObject) (core.PendingPromoteResult, []core.DeletedCopy, error) {
	if p != nil {
		m.promotedPending = append(m.promotedPending, *p)
	}
	return m.promoteResult, m.promoteDisplaced, m.promoteErr
}

// PendingDepth returns the configured depth value (defaults to 0).
func (m *mockMetadataStore) PendingDepth(_ context.Context) (int64, error) {
	return m.pendingDepthVal, m.pendingDepthErr
}
