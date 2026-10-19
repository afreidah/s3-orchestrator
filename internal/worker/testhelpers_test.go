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
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// FIXTURE NORMALISATION
// -------------------------------------------------------------------------

// A fixture that names no storage key describes bytes at the object's own key,
// as a backfilled or imported row has them. Filling the path in on the way out
// of the stub keeps each fixture about its own subject instead of restating the
// path.

// withPaths fills in the storage key of every row that left it unset.
func withPaths(rows []core.ObjectLocation) []core.ObjectLocation {
	out := make([]core.ObjectLocation, len(rows))
	for i := range rows {
		out[i] = rows[i]
		out[i].StorageKey = core.StoragePath(rows[i].ObjectKey, rows[i].StorageKey)
	}
	return out
}

// withIntentPaths does the same for pending intents.
func withIntentPaths(rows []core.PendingObject) []core.PendingObject {
	out := make([]core.PendingObject, len(rows))
	for i := range rows {
		out[i] = rows[i]
		out[i].StorageKey = core.StoragePath(rows[i].ObjectKey, rows[i].StorageKey)
	}
	return out
}

// withCleanupPaths does the same for queued cleanups.
func withCleanupPaths(rows []core.CleanupItem) []core.CleanupItem {
	out := make([]core.CleanupItem, len(rows))
	for i, r := range rows {
		r.StorageKey = core.StoragePath(r.ObjectKey, r.StorageKey)
		out[i] = r
	}
	return out
}

// -------------------------------------------------------------------------
// MATCHERS
// -------------------------------------------------------------------------

// cleanupOf matches a settled batch of exactly one queued cleanup by backend,
// object key, reason, and size. The storage path is not matched, since
// workers mint it randomly.
func cleanupOf(backendName, objectKey, reason string, size int64) gomock.Matcher {
	return gomock.Cond(func(items []core.CleanupItem) bool {
		return len(items) == 1 && items[0].BackendName == backendName &&
			items[0].ObjectKey == objectKey && items[0].Reason == reason &&
			items[0].SizeBytes == size
	})
}

// cleanupOn matches a settled batch of exactly one queued cleanup by backend
// and object alone, for tests whose subject is that a cleanup happened at all.
func cleanupOn(backendName, objectKey string) gomock.Matcher {
	return gomock.Cond(func(items []core.CleanupItem) bool {
		return len(items) == 1 && items[0].BackendName == backendName &&
			items[0].ObjectKey == objectKey
	})
}

// intentOn matches a copy intent by its target backend and object key.
func intentOn(backendName, objectKey string) gomock.Matcher {
	return gomock.Cond(func(p *core.PendingObject) bool {
		return p.BackendName == backendName && p.ObjectKey == objectKey
	})
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newMockOps builds an Ops mock that already answers Quota() with a real
// tracker. Every fixture needs it, because an unanswered call inside a worker
// pool deadlocks the dispatcher instead of failing the test.
func newMockOps(ctrl *gomock.Controller) *MockOps {
	ops := NewMockOps(ctrl)
	ops.EXPECT().Quota().Return(counter.NewQuotaTracker()).AnyTimes()
	return ops
}

// expectBackendOrder has ops list order as its backends and name targets as
// the ones that may take new bytes. A test of a healthy fleet passes order for
// both. WriteTargets returns a fresh slice each call, as the runtime does.
func expectBackendOrder(ops *MockOps, order, targets []string) {
	ops.EXPECT().BackendOrder().Return(order).AnyTimes()
	ops.EXPECT().WriteTargets().DoAndReturn(func() []string { return slices.Clone(targets) }).AnyTimes()
}

// expectFleet has ops serve fleet as its backends, and answer which calls each
// one serves from the backend itself, so a tripped breaker in a fixture reads
// as refusing. A name missing from fleet serves nothing.
func expectFleet(ops *MockOps, fleet map[string]backend.ObjectBackend) {
	ops.EXPECT().Backends().Return(fleet).AnyTimes()
	ops.EXPECT().ServedCalls(gomock.Any()).DoAndReturn(func(name string) core.BackendCalls {
		b, ok := fleet[name]
		if !ok {
			return 0
		}
		return backend.ServedCalls(b)
	}).AnyTimes()
	ops.EXPECT().RefusedFor(gomock.Any(), gomock.Any()).DoAndReturn(func(name string, required core.BackendCalls) time.Duration {
		b, ok := fleet[name]
		if !ok {
			return 0
		}
		return backend.RefusedFor(b, required)
	}).AnyTimes()
}

// newTestReplicator builds a Replicator with no stored-form decoders and no
// integrity config, which is the shape most replication tests want: copies move
// verbatim and nothing is hash-checked. Tests that exercise verify_on_replicate
// call SetIntegrityConfig and pass their own decoders through ReplicatorDeps.
func newTestReplicator(ops Ops, pl Placement, store ReplicatorStore) *Replicator {
	return NewReplicator(ReplicatorDeps{Ops: ops, Placement: pl, Store: store})
}

// admitClaims has pl accept every copy-target claim, recording each intent
// claimed. The returned slice pointer collects every claim.
func admitClaims(pl *MockPlacement) *[]*core.PendingObject {
	claims := &[]*core.PendingObject{}
	pl.EXPECT().ClaimCopyTarget(gomock.Any(), gomock.Any(), core.AcceptForWrite).
		DoAndReturn(func(_ context.Context, p *core.PendingObject, _ core.BackendFlags) error {
			*claims = append(*claims, p)
			return nil
		}).AnyTimes()
	return claims
}

// placeThroughStore has pl accept every claim and place through ms, the way the
// coordinator places through the store, answering with ms's configured size or
// error. Every placement is recorded on ms.placements.
func placeThroughStore(pl *MockPlacement, ms *mockMetadataStore) {
	admitClaims(pl)
	pl.EXPECT().Place(gomock.Any(), gomock.Any()).
		DoAndReturn(func(ctx context.Context, p *core.Placement) (int64, error) {
			placed, _, err := ms.PlaceCopy(ctx, p)
			return placed, err
		}).AnyTimes()
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
	core.BackendStateStore
	core.DashboardStore
	core.UsageFlusher
	core.AdvisoryLocker
	core.LifecycleAdmin
	core.NotificationOutbox
	pendingCleanups     []core.CleanupItem
	completedItems      []core.CleanupItem
	dlqMoves            []dlqMoveCall
	moveDLQErr          error
	dlqDepthVal         int64
	dlqDepthErr         error
	randomHashedObjects []core.ObjectLocation
	unreadable          []core.ObjectLocation
	allLocations        []core.ObjectLocation
	allLocationsErr     error
	scrubbed            []string

	scrubSelectedBackends []string
	scrubDeclinedBackends []string
	scrubBatchCutoff      time.Time
	scrubDeferredCutoff   time.Time
	deferredCandidates    int64
	deferredCandidatesErr error

	markScrubbedErr     error
	hashPatchErr        error
	deletedLocations    []string
	deleteReasons       []string
	deleteLocationErr   error
	ledgerErr           error
	ledger              core.LedgerStats
	lastUpdatedHash     string
	lastRecordedETag    string
	underReplicated     []core.ObjectLocation
	underReplicatedErr  error
	overReplicated      []core.ObjectLocation
	overReplicatedErr   error
	overReplicatedCount int64
	quotaStats          map[string]core.QuotaStat
	quotaStatsErr       error
	placeCopySize       int64
	placeCopyErr        error
	placements          []core.Placement
	removedCopies       int
	removedCopySize     int64
	removeExcessNoOp    bool
	removeExcessErr     error
	removeExcessFailKey string
	objectsByBackend    map[string][]core.ObjectLocation
	staleDeleted        int

	getBackendsForKeysCalls int
	getBackendsForKeysResp  map[string][]string

	stalePending      []core.PendingObject
	deletedPendingIDs []string
	promotedPending   []core.PendingObject
	promoteResult     core.PendingPromoteResult
	promoteCleanup    []core.CleanupItem
	promoteDeltas     core.QuotaDeltas
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
	return withCleanupPaths(m.pendingCleanups), nil
}

// ClaimPendingCleanups is a stub on mockMetadataStore; mirrors
// GetPendingCleanups, stamping each row with the caller's claim the way the
// store does.
func (m *mockMetadataStore) ClaimPendingCleanups(_ context.Context, _ int, instanceID string, _ time.Time) ([]core.CleanupItem, error) {
	items := withCleanupPaths(m.pendingCleanups)
	for i := range items {
		if items[i].Claim == nil {
			items[i].Claim = &core.CleanupClaim{At: time.Now(), By: instanceID}
		}
	}
	return items, nil
}

// CompleteCleanupItems is a stub on mockMetadataStore; records the items and
// reports every one retired.
func (m *mockMetadataStore) CompleteCleanupItems(_ context.Context, items []core.CleanupItem) ([]int64, error) {
	m.completedItems = append(m.completedItems, items...)
	return cleanupIDs(items), nil
}

// RetryCleanupItem is a stub on mockMetadataStore; reports the claim held so
// the retry lands.
func (m *mockMetadataStore) RetryCleanupItem(_ context.Context, _ *core.CleanupItem, _ time.Duration, _ string) (bool, error) {
	return true, nil
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
func (m *mockMetadataStore) MoveCleanupToDLQ(_ context.Context, item *core.CleanupItem, lastError string) (bool, error) {
	m.dlqMoves = append(m.dlqMoves, dlqMoveCall{id: item.ID, lastError: lastError})
	if m.moveDLQErr != nil {
		return false, m.moveDLQErr
	}
	return true, nil
}

// cleanupIDs returns the id of each item in order.
func cleanupIDs(items []core.CleanupItem) []int64 {
	ids := make([]int64, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	return ids
}

// settlesCleanups has store retire, retry, and dead-letter every row it is
// handed, as it does for rows still held under their claim.
func settlesCleanups(store *storetest.MockMetadataStore) {
	store.EXPECT().CompleteCleanupItems(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, items []core.CleanupItem) ([]int64, error) {
			return cleanupIDs(items), nil
		}).AnyTimes()
	store.EXPECT().RetryCleanupItem(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(true, nil).AnyTimes()
	store.EXPECT().MoveCleanupToDLQ(gomock.Any(), gomock.Any(), gomock.Any()).Return(true, nil).AnyTimes()
}

// CleanupDLQDepth is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) CleanupDLQDepth(_ context.Context) (int64, error) {
	return m.dlqDepthVal, m.dlqDepthErr
}

// GetLeastRecentlyScrubbedObjects is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value. Records the backend filter and the
// re-verification cutoff so tests can assert the scrubber only asked for
// backends it can afford to read, and only for copies that are due.
func (m *mockMetadataStore) GetLeastRecentlyScrubbedObjects(_ context.Context, _ int, backends []string, scrubbedBefore time.Time) ([]core.ObjectLocation, error) {
	m.scrubSelectedBackends = backends
	m.scrubBatchCutoff = scrubbedBefore
	return withPaths(m.randomHashedObjects), nil
}

// CountScrubCandidatesOnBackends is a stub on mockMetadataStore; records the
// backends a cycle declined and the cutoff it counted against, and returns the
// test-set fixture count.
func (m *mockMetadataStore) CountScrubCandidatesOnBackends(_ context.Context, backends []string, scrubbedBefore time.Time) (int64, error) {
	m.scrubDeclinedBackends = backends
	m.scrubDeferredCutoff = scrubbedBefore
	return m.deferredCandidates, m.deferredCandidatesErr
}

// UpdateCopyMetadata is a stub on mockMetadataStore. A hash patch records the
// hash and returns hashPatchErr; a bare stamp records the copy a scrub cycle
// stamped, so tests can assert the sweep advanced past it, and returns
// markScrubbedErr.
func (m *mockMetadataStore) UpdateCopyMetadata(_ context.Context, loc *core.ObjectLocation, p *core.CopyPatch) error {
	if p.ContentHash != "" {
		if m.hashPatchErr != nil {
			return m.hashPatchErr
		}
		m.lastUpdatedHash = p.ContentHash
		return nil
	}
	m.scrubbed = append(m.scrubbed, loc.ObjectKey+"@"+loc.BackendName)
	return m.markScrubbedErr
}

// LedgerStats returns the test-set ledger fixture.
func (m *mockMetadataStore) LedgerStats(context.Context) (core.LedgerStats, error) {
	return m.ledger, m.ledgerErr
}

// isUnreadableQuery reports whether a form query asks for the copies encrypted
// with no key, the only population the fixture holds.
func isUnreadableQuery(with, without core.StoredFlags) bool {
	return with == core.StoredEncrypted && without == core.StoredKeyed
}

// ListCopiesByForm returns up to limit rows of the unreadable fixture that sort
// after the cursor, in (object_key, backend_name) order, and nothing for any
// other filter.
func (m *mockMetadataStore) ListCopiesByForm(_ context.Context, f *core.FormFilter, limit int, after core.Cursor) ([]core.ObjectLocation, error) {
	if !isUnreadableQuery(f.With, f.Without) {
		return nil, nil
	}
	rows := slices.Clone(m.unreadable)
	slices.SortFunc(rows, func(a, b core.ObjectLocation) int {
		return cmp.Or(cmp.Compare(a.ObjectKey, b.ObjectKey), cmp.Compare(a.BackendName, b.BackendName))
	})
	var out []core.ObjectLocation
	for i := range rows {
		r := &rows[i]
		if len(out) == limit {
			break
		}
		if r.ObjectKey > after.ObjectKey || (r.ObjectKey == after.ObjectKey && r.BackendName > after.BackendName) {
			out = append(out, *r)
		}
	}
	return out, nil
}

// CountCopiesByForm reports the size of the unreadable fixture for the
// unreadable query, and zero for any other.
func (m *mockMetadataStore) CountCopiesByForm(_ context.Context, with, without core.StoredFlags) (int64, error) {
	if !isUnreadableQuery(with, without) {
		return 0, nil
	}
	return int64(len(m.unreadable)), nil
}

// RecordObjectIdentity is a stub on mockMetadataStore; records the ETag the
// backfill computed so a test can assert the object learned one.
func (m *mockMetadataStore) RecordObjectIdentity(_ context.Context, _ *core.ObjectLocation, id *core.ObjectIdentity) error {
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
	return withPaths(m.underReplicated), m.underReplicatedErr
}

// GetUnderReplicatedObjectsExcluding is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetUnderReplicatedObjectsExcluding(_ context.Context, _, _ int, _ []string) ([]core.ObjectLocation, error) {
	return withPaths(m.underReplicated), m.underReplicatedErr
}

// GetQuotaStats is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetQuotaStats(_ context.Context) (map[string]core.QuotaStat, error) {
	return m.quotaStats, m.quotaStatsErr
}

// PlaceCopy records the placement on mockMetadataStore and returns the
// test-set size, or the test-set error in its place.
func (m *mockMetadataStore) PlaceCopy(_ context.Context, p *core.Placement) (int64, []core.CleanupItem, error) {
	m.placements = append(m.placements, *p)
	if m.placeCopyErr != nil {
		return 0, nil, m.placeCopyErr
	}
	return m.placeCopySize, nil, nil
}

// GetOverReplicatedObjects is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetOverReplicatedObjects(_ context.Context, _, _ int) ([]core.ObjectLocation, error) {
	return withPaths(m.overReplicated), m.overReplicatedErr
}

// CountReplicationBacklog is a stub on mockMetadataStore; reports the test-set
// over-replicated count.
func (m *mockMetadataStore) CountReplicationBacklog(_ context.Context, _ int) (core.ReplicationBacklog, error) {
	return core.ReplicationBacklog{Over: m.overReplicatedCount}, nil
}

// ListObjectsByBackend returns up to limit of the backend's fixture objects
// after the cursor, in (size_bytes, object_key) order as the stores do.
func (m *mockMetadataStore) ListObjectsByBackend(_ context.Context, name string, limit int, after core.SizeCursor) ([]core.ObjectLocation, error) {
	rows := slices.Clone(m.objectsByBackend[name])
	slices.SortFunc(rows, func(a, b core.ObjectLocation) int {
		return cmp.Or(cmp.Compare(a.SizeBytes, b.SizeBytes), cmp.Compare(a.ObjectKey, b.ObjectKey))
	})
	var out []core.ObjectLocation
	for i := range rows {
		r := &rows[i]
		if len(out) == limit {
			break
		}
		if r.SizeBytes > after.SizeBytes || (r.SizeBytes == after.SizeBytes && r.ObjectKey > after.ObjectKey) {
			out = append(out, *r)
		}
	}
	return withPaths(out), nil
}

// GetAllObjectLocations is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetAllObjectLocations(_ context.Context, _ string) ([]core.ObjectLocation, error) {
	return withPaths(m.allLocations), m.allLocationsErr
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

// DeleteObjectLocation is a stub on mockMetadataStore. A surplus-copy removal
// (keepAtLeast > 0) reports success so the cleaner counts it,
// ErrCopyNotExcess when removeExcessNoOp is set, mimicking a race that already
// absorbed the excess, or the seeded removeExcessErr when the test drives a
// refusal. Any other removal records the copy and reports it removed at its
// own size, as the store does when the copy is unchanged, or returns
// deleteLocationErr. A removal queues one cleanup row under reason, and none
// when reason is empty. Every reason passed is recorded on deleteReasons.
func (m *mockMetadataStore) DeleteObjectLocation(_ context.Context, loc *core.ObjectLocation, keepAtLeast int, reason string) ([]core.CleanupItem, error) {
	m.deleteReasons = append(m.deleteReasons, reason)
	if keepAtLeast > 0 {
		switch {
		case loc.ObjectKey == m.removeExcessFailKey:
			return nil, errRemoveExcessFailed
		case m.removeExcessErr != nil:
			return nil, m.removeExcessErr
		case m.removeExcessNoOp:
			return nil, core.ErrCopyNotExcess
		}
		m.removedCopies++
		return queuedCleanup(loc, reason, m.removedCopySize), nil
	}
	if m.deleteLocationErr != nil {
		return nil, m.deleteLocationErr
	}
	m.staleDeleted++
	m.deletedLocations = append(m.deletedLocations, loc.ObjectKey+"@"+loc.BackendName)
	return queuedCleanup(loc, reason, loc.SizeBytes), nil
}

// errRemoveExcessFailed is the store error a surplus-copy removal of
// removeExcessFailKey fails with.
var errRemoveExcessFailed = errors.New("remove excess copy failed")

// queuedCleanup is the cleanup row a removal of loc queues under reason, or
// nothing when reason is empty, as the store does.
func queuedCleanup(loc *core.ObjectLocation, reason string, size int64) []core.CleanupItem {
	if reason == "" {
		return nil
	}
	return []core.CleanupItem{{
		BackendName: loc.BackendName,
		ObjectKey:   loc.ObjectKey,
		StorageKey:  loc.StorageKey,
		Reason:      reason,
		SizeBytes:   size,
	}}
}

// --- PendingStore stubs ---

// GetStalePending returns the configured fixture rows; the reaper batch
// limit is ignored to keep tests focused on resolution outcomes rather
// than is a stub on mockMetadataStore; returns either the test-set
// fixture field or the zero value.
func (m *mockMetadataStore) GetStalePending(_ context.Context, _ time.Time, _ int) ([]core.PendingObject, error) {
	return withIntentPaths(m.stalePending), nil
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
func (m *mockMetadataStore) PromotePending(_ context.Context, p *core.PendingObject) (core.PendingPromoteResult, []core.CleanupItem, core.QuotaDeltas, error) {
	if p != nil {
		m.promotedPending = append(m.promotedPending, *p)
	}
	return m.promoteResult, m.promoteCleanup, m.promoteDeltas, m.promoteErr
}

// PendingDepth returns the configured depth value (defaults to 0).
func (m *mockMetadataStore) PendingDepth(_ context.Context) (int64, error) {
	return m.pendingDepthVal, m.pendingDepthErr
}
