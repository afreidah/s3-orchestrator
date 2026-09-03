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
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newTestReplicator builds a Replicator with no stored-form decoders and no
// integrity config, which is the shape most replication tests want: copies move
// verbatim and nothing is hash-checked. Tests that exercise verify_on_replicate
// call SetIntegrityConfig and pass their own decoders through ReplicatorDeps.
func newTestReplicator(ops Ops, pl Placement, store ReplicatorStore) *Replicator {
	return NewReplicator(ReplicatorDeps{Ops: ops, Placement: pl, Store: store})
}

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// mockMetadataStore is a minimal stub for worker tests. It embeds every
// narrow store role as a nil interface so any worker signature accepts it;
// tests override only the methods they exercise, and any unstubbed call
// panics, which surfaces test-fixture gaps loudly.
//
// The fields are grouped by the worker that reads them: the scrub cycle's
// backend filtering, the rebalancer planner's batch-query fixtures, and the
// pending reaper's intents.
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
	core.LifecycleAdmin
	core.EncryptionAdmin
	core.NotificationOutbox
	pendingCleanups     []core.CleanupItem
	completedIDs        []int64
	dlqMoves            []dlqMoveCall
	moveDLQResult       bool
	moveDLQErr          error
	dlqDepthVal         int64
	dlqDepthErr         error
	randomHashedObjects []core.ObjectLocation
	objectsWithoutHash  []core.ObjectLocation
	allLocations        []core.ObjectLocation
	allLocationsErr     error
	scrubbed            []string

	scrubSelectedBackends []string
	scrubDeclinedBackends []string
	deferredCandidates    int64
	deferredCandidatesErr error

	markScrubbedErr     error
	deletedLocations    []string
	deleteLocationErr   error
	oldestUnverifiedErr error
	oldestUnverified    time.Duration
	neverVerified       int64
	deferredCopies      int64
	coverageReachable   []string
	lastUpdatedHash     string
	lastRecordedETag    string
	underReplicated     []core.ObjectLocation
	underReplicatedErr  error
	overReplicated      []core.ObjectLocation
	overReplicatedErr   error
	overReplicatedCount int64
	quotaStats          map[string]core.QuotaStat
	quotaStatsErr       error
	recordReplicaOK     bool
	recordReplicaSize   int64
	recordReplicaErr    error
	replicaRecorded     int
	removedCopies       int
	removeExcessNoOp    bool
	removeExcessErr     error
	objectsByBackend    map[string][]core.ObjectLocation
	moveSize            int64
	staleDeleted        int

	getBackendsForKeysCalls int
	getBackendsForKeysResp  map[string][]string

	stalePending      []core.PendingObject
	deletedPendingIDs []string
	promotedPending   []core.PendingObject
	promoteResult     core.PendingPromoteResult
	promoteDisplaced  []core.DeletedCopy
	promoteErr        error
	pendingDepthVal   int64
	pendingDepthErr   error
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// GetPendingCleanups is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetPendingCleanups(_ context.Context, _ int) ([]core.CleanupItem, error) {
	return m.pendingCleanups, nil
}

// ClaimPendingCleanups is a stub on mockMetadataStore; mirrors
// GetPendingCleanups so existing fixtures exercise the worker's claim path
// without per-test plumbing.
func (m *mockMetadataStore) ClaimPendingCleanups(_ context.Context, _ int, _ string, _ time.Time) ([]core.CleanupItem, error) {
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

// GetLeastRecentlyScrubbedObjects is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value. Records the backend filter so tests can
// assert the scrubber only asked for backends it can afford to read.
func (m *mockMetadataStore) GetLeastRecentlyScrubbedObjects(_ context.Context, _ int, backends []string) ([]core.ObjectLocation, error) {
	m.scrubSelectedBackends = backends
	return m.randomHashedObjects, nil
}

// CountScrubCandidatesOnBackends is a stub on mockMetadataStore; records the
// backends a cycle declined and returns the test-set fixture count.
func (m *mockMetadataStore) CountScrubCandidatesOnBackends(_ context.Context, backends []string) (int64, error) {
	m.scrubDeclinedBackends = backends
	return m.deferredCandidates, m.deferredCandidatesErr
}

// MarkObjectScrubbed is a stub on mockMetadataStore; records the copies a
// scrub cycle stamped so tests can assert the sweep advanced past them.
func (m *mockMetadataStore) MarkObjectScrubbed(_ context.Context, key, backendName string) error {
	m.scrubbed = append(m.scrubbed, key+"@"+backendName)
	return m.markScrubbedErr
}

// IntegrityCoverage is a stub on mockMetadataStore; records the backend set the
// caller scoped the query to and returns the test-set fixture fields.
func (m *mockMetadataStore) IntegrityCoverage(_ context.Context, reachable []string) (core.CoverageStat, error) {
	m.coverageReachable = reachable
	return core.CoverageStat{
		OldestUnverifiedAge: m.oldestUnverified,
		NeverVerified:       m.neverVerified,
		Deferred:            m.deferredCopies,
	}, m.oldestUnverifiedErr
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

// RecordObjectIdentity is a stub on mockMetadataStore; records the ETag the
// backfill computed so a test can assert the object learned one.
func (m *mockMetadataStore) RecordObjectIdentity(_ context.Context, _ string, id *core.ObjectIdentity) error {
	if id != nil {
		m.lastRecordedETag = id.ETag
	}
	return nil
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newTestUsageTracker creates a UsageTracker with no limits for testing.
func newTestUsageTracker() *counter.UsageTracker {
	return counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2"}), nil)
}

// newTestRecorder builds an accounting.Recorder wired to a fresh
// no-limits usage tracker and a no-op operation-metric callback. Use
// from mock setups: ops.EXPECT().Acct().Return(newTestRecorder()).
func newTestRecorder() *accounting.Recorder {
	return accounting.New(newTestUsageTracker(), func(string, string, time.Time, error) {})
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// GetUnderReplicatedObjects is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetUnderReplicatedObjects(_ context.Context, _, _ int) ([]core.ObjectLocation, error) {
	return m.underReplicated, m.underReplicatedErr
}

// GetUnderReplicatedObjectsExcluding is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetUnderReplicatedObjectsExcluding(_ context.Context, _, _ int, _ []string) ([]core.ObjectLocation, error) {
	return m.underReplicated, m.underReplicatedErr
}

// GetQuotaStats is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetQuotaStats(_ context.Context) (map[string]core.QuotaStat, error) {
	return m.quotaStats, m.quotaStatsErr
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
	return m.overReplicated, m.overReplicatedErr
}

// CountOverReplicatedObjects is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) CountOverReplicatedObjects(_ context.Context, _ int) (int64, error) {
	return m.overReplicatedCount, nil
}

// RemoveExcessCopy is a stub on mockMetadataStore; reports a successful
// removal so the cleaner counts it, a benign no-op (removed=false) when
// removeExcessNoOp is set, mimicking a race that already absorbed the excess,
// or the seeded removeExcessErr when the test drives a refusal.
func (m *mockMetadataStore) RemoveExcessCopy(_ context.Context, _, _ string, _ int) (bool, error) {
	if m.removeExcessErr != nil {
		return false, m.removeExcessErr
	}
	if m.removeExcessNoOp {
		return false, nil
	}
	m.removedCopies++
	return true, nil
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
	return m.allLocations, m.allLocationsErr
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
func (m *mockMetadataStore) DeleteObjectLocation(_ context.Context, key, backendName string) error {
	if m.deleteLocationErr != nil {
		return m.deleteLocationErr
	}
	m.staleDeleted++
	m.deletedLocations = append(m.deletedLocations, key+"@"+backendName)
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
