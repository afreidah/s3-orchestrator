// -------------------------------------------------------------------------------
// OverReplicationCleaner Tests
//
// Author: Alex Freidah
//
// Verifies the over-replication scoring and selection logic: the cleaner
// removes excess copies preferentially from draining backends and
// circuit-broken backends, and degrades gracefully when the backend
// health table contains an unknown name. Also covers config round-trip
// and the pending-count surface used by the dashboard gauge.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestOverReplicationCleaner_SetConfig_RoundTrip verifies the over replication cleaner set config round trip contract.
// Asserts that Config().Factor = , want 2.
func TestOverReplicationCleaner_SetConfig_RoundTrip(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	c := NewOverReplicationCleaner(newMockOps(ctrl), NewMockPlacement(ctrl), &mockMetadataStore{})
	if c.Config() != nil {
		t.Fatal("expected nil config before set")
	}
	cfg := &config.ReplicationConfig{Factor: 2}
	c.SetConfig(cfg)
	if got := c.Config(); got.Factor != 2 {
		t.Errorf("Config().Factor = %d, want 2", got.Factor)
	}
}

// TestCountPending verifies the count pending contract.
// Asserts that unexpected error:.
func TestCountPending(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{overReplicatedCount: 5}

	c := NewOverReplicationCleaner(ops, pl, ms)
	count, err := c.CountPending(context.Background(), 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 5 {
		t.Errorf("CountPending = %d, want 5", count)
	}
}

// TestScoreCopy_DrainingBackend verifies the score copy draining backend contract.
// Asserts that draining backend should score 0, got.
func TestScoreCopy_DrainingBackend(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	ops.EXPECT().IsDraining("b1").Return(true)

	c := NewOverReplicationCleaner(ops, pl, ms)
	score := c.ScoreCopy(&core.ObjectLocation{BackendName: "b1"}, nil)
	if score != 0 {
		t.Errorf("draining backend should score 0, got %f", score)
	}
}

// TestScoreCopy_HealthyBackend verifies the score copy healthy backend contract.
// Asserts that healthy backend at 20 should score ~2.8, got.
func TestScoreCopy_HealthyBackend(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	be := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().IsDraining("b1").Return(false)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": be})

	c := NewOverReplicationCleaner(ops, pl, ms)
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 200, BytesLimit: 1000}, // 20% utilized
	}
	score := c.ScoreCopy(&core.ObjectLocation{BackendName: "b1"}, stats)
	// Score should be 2 + (1 - 0.2) = 2.8
	if score < 2.7 || score > 2.9 {
		t.Errorf("healthy backend at 20%% should score ~2.8, got %f", score)
	}
}

// TestScoreCopy_UnknownBackend verifies the score copy unknown backend contract.
// Asserts that unknown backend should score 0, got.
func TestScoreCopy_UnknownBackend(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	ops.EXPECT().IsDraining("gone").Return(false)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{})

	c := NewOverReplicationCleaner(ops, pl, ms)
	score := c.ScoreCopy(&core.ObjectLocation{BackendName: "gone"}, nil)
	if score != 0 {
		t.Errorf("unknown backend should score 0, got %f", score)
	}
}

// TestCleanObject_RemovesLowestScored verifies the clean object removes lowest scored contract.
// Asserts that removed = , want 1.
func TestCleanObject_RemovesLowestScored(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	be1 := backendtest.NewMockObjectBackend(ctrl)
	be2 := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().IsDraining(gomock.Any()).Return(false).AnyTimes()
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": be1, "b2": be2}).AnyTimes()
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	// b1 is more utilized (lower score -> removed first)
	ops.EXPECT().GetBackend("b1").Return(be1, nil)
	pl.EXPECT().DeleteOrEnqueue(gomock.Any(), be1, "b1", "key1", "over_replication", int64(100))

	c := NewOverReplicationCleaner(ops, pl, ms)
	copies := []core.ObjectLocation{
		{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
		{ObjectKey: "key1", BackendName: "b2", SizeBytes: 100},
	}
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000}, // 90% -> lower score
		"b2": {BytesUsed: 100, BytesLimit: 1000}, // 10% -> higher score
	}

	removed, _ := c.cleanObject(context.Background(), "key1", copies, 1, 1, stats)
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if ms.removedCopies != 1 {
		t.Errorf("removedCopies = %d, want 1", ms.removedCopies)
	}
}

// TestCleanObject_DoesNotDoubleCountAPICalls pins issue #881: cleanObject
// must let DeleteOrEnqueue own the backend DELETE API-call accounting
// and not record its own. Setting ops.EXPECT().Acct().Times(0) makes the
// previous duplicate Acct().APICall(victim.BackendName) line fail loudly
// if it is ever reintroduced.
func TestCleanObject_DoesNotDoubleCountAPICalls(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	be1 := backendtest.NewMockObjectBackend(ctrl)
	be2 := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().IsDraining(gomock.Any()).Return(false).AnyTimes()
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": be1, "b2": be2}).AnyTimes()
	ops.EXPECT().Acct().Times(0)
	ops.EXPECT().GetBackend("b1").Return(be1, nil)
	pl.EXPECT().DeleteOrEnqueue(gomock.Any(), be1, "b1", "key1", "over_replication", int64(100))

	c := NewOverReplicationCleaner(ops, pl, ms)
	copies := []core.ObjectLocation{
		{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
		{ObjectKey: "key1", BackendName: "b2", SizeBytes: 100},
	}
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}

	if removed, _ := c.cleanObject(context.Background(), "key1", copies, 1, 1, stats); removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
}

// TestCleanObject_SkipsBackendDeleteOnRaceNoOp verifies that when
// RemoveExcessCopy reports removed=false (a concurrent path already
// absorbed the excess), the cleaner counts nothing and never touches the
// backend -- no GetBackend, no DeleteOrEnqueue. The unset gomock
// expectations assert the latter.
func TestCleanObject_SkipsBackendDeleteOnRaceNoOp(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{removeExcessNoOp: true}

	be1 := backendtest.NewMockObjectBackend(ctrl)
	be2 := backendtest.NewMockObjectBackend(ctrl)
	ops.EXPECT().IsDraining(gomock.Any()).Return(false).AnyTimes()
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": be1, "b2": be2}).AnyTimes()

	c := NewOverReplicationCleaner(ops, pl, ms)
	copies := []core.ObjectLocation{
		{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
		{ObjectKey: "key1", BackendName: "b2", SizeBytes: 100},
	}
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}

	if removed, _ := c.cleanObject(context.Background(), "key1", copies, 1, 1, stats); removed != 0 {
		t.Errorf("removed = %d, want 0 on benign no-op", removed)
	}
}

// TestClean_FactorOne_Noop verifies the clean factor one noop contract.
// Asserts that unexpected error:.
func TestClean_FactorOne_Noop(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	c := NewOverReplicationCleaner(newMockOps(ctrl), NewMockPlacement(ctrl), &mockMetadataStore{})

	sum, err := c.Clean(context.Background(), config.ReplicationConfig{Factor: 1}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sum.CopiesRemoved != 0 {
		t.Errorf("removed = %d, want 0", sum.CopiesRemoved)
	}
}

// TestClean_NothingOverReplicated verifies the clean nothing over replicated contract.
// Asserts that unexpected error:.
// Deliberately not parallel: asserts a process-wide gauge that other
// over-replication tests overwrite, so a concurrent run reads whichever
// cycle finished last.
func TestClean_NothingOverReplicated(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	c := NewOverReplicationCleaner(ops, pl, ms)
	sum, err := c.Clean(context.Background(), config.ReplicationConfig{Factor: 2, BatchSize: 10, Concurrency: 1}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sum.CopiesRemoved != 0 {
		t.Errorf("removed = %d, want 0", sum.CopiesRemoved)
	}
	if p := promtest.ToFloat64(telemetry.OverReplicationPending); p != 0 {
		t.Errorf("OverReplicationPending = %v, want 0", p)
	}
}

// TestCleanObject_SkipsVictimHoldingOnlyKey verifies a refusal from the store
// is treated as a skip rather than a failure: the cleaner must not go on to
// delete the backend object whose metadata row it was denied permission to
// drop, or the only readable copy would be destroyed anyway.
func TestCleanObject_SkipsVictimHoldingOnlyKey(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := newMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{removeExcessErr: core.ErrCopyHoldsOnlyDEK}

	be1 := backendtest.NewMockObjectBackend(ctrl)
	be2 := backendtest.NewMockObjectBackend(ctrl)
	ops.EXPECT().IsDraining(gomock.Any()).Return(false).AnyTimes()
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": be1, "b2": be2}).AnyTimes()

	c := NewOverReplicationCleaner(ops, pl, ms)
	copies := []core.ObjectLocation{
		{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
		{ObjectKey: "key1", BackendName: "b2", SizeBytes: 100},
	}
	stats := map[string]core.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}

	if removed, _ := c.cleanObject(context.Background(), "key1", copies, 1, 1, stats); removed != 0 {
		t.Errorf("removed = %d, want 0 when the victim holds the only key", removed)
	}
}
