package worker

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"go.uber.org/mock/gomock"
)

func TestOverReplicationCleaner_SetConfig_RoundTrip(t *testing.T) {
	ctrl := gomock.NewController(t)
	c := NewOverReplicationCleaner(NewMockOps(ctrl))
	if c.Config() != nil {
		t.Fatal("expected nil config before set")
	}
	cfg := &config.ReplicationConfig{Factor: 2}
	c.SetConfig(cfg)
	if got := c.Config(); got.Factor != 2 {
		t.Errorf("Config().Factor = %d, want 2", got.Factor)
	}
}

func TestCountPending(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{overReplicatedCount: 5}
	ops.EXPECT().Store().Return(ms)

	c := NewOverReplicationCleaner(ops)
	count, err := c.CountPending(context.Background(), 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 5 {
		t.Errorf("CountPending = %d, want 5", count)
	}
}

func TestScoreCopy_DrainingBackend(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ops.EXPECT().IsDraining("b1").Return(true)

	c := NewOverReplicationCleaner(ops)
	score := c.ScoreCopy(&store.ObjectLocation{BackendName: "b1"}, nil)
	if score != 0 {
		t.Errorf("draining backend should score 0, got %f", score)
	}
}

func TestScoreCopy_HealthyBackend(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	be := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().IsDraining("b1").Return(false)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": be})

	c := NewOverReplicationCleaner(ops)
	stats := map[string]store.QuotaStat{
		"b1": {BytesUsed: 200, BytesLimit: 1000}, // 20% utilized
	}
	score := c.ScoreCopy(&store.ObjectLocation{BackendName: "b1"}, stats)
	// Score should be 2 + (1 - 0.2) = 2.8
	if score < 2.7 || score > 2.9 {
		t.Errorf("healthy backend at 20%% should score ~2.8, got %f", score)
	}
}

func TestScoreCopy_UnknownBackend(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)

	ops.EXPECT().IsDraining("gone").Return(false)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{})

	c := NewOverReplicationCleaner(ops)
	score := c.ScoreCopy(&store.ObjectLocation{BackendName: "gone"}, nil)
	if score != 0 {
		t.Errorf("unknown backend should score 0, got %f", score)
	}
}

func TestCleanObject_RemovesLowestScored(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	be1 := backendtest.NewMockObjectBackend(ctrl)
	be2 := backendtest.NewMockObjectBackend(ctrl)
	ms := &mockMetadataStore{}

	ops.EXPECT().IsDraining(gomock.Any()).Return(false).AnyTimes()
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": be1, "b2": be2}).AnyTimes()
	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	// b1 is more utilized (lower score → removed first)
	ops.EXPECT().GetBackend("b1").Return(be1, nil)
	ops.EXPECT().DeleteOrEnqueue(gomock.Any(), be1, "b1", "key1", "over_replication", int64(100))

	c := NewOverReplicationCleaner(ops)
	copies := []store.ObjectLocation{
		{ObjectKey: "key1", BackendName: "b1", SizeBytes: 100},
		{ObjectKey: "key1", BackendName: "b2", SizeBytes: 100},
	}
	stats := map[string]store.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000}, // 90% → lower score
		"b2": {BytesUsed: 100, BytesLimit: 1000}, // 10% → higher score
	}

	removed := c.cleanObject(context.Background(), "key1", copies, 1, stats)
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if ms.removedCopies != 1 {
		t.Errorf("removedCopies = %d, want 1", ms.removedCopies)
	}
}

func TestClean_FactorOne_Noop(t *testing.T) {
	ctrl := gomock.NewController(t)
	c := NewOverReplicationCleaner(NewMockOps(ctrl))

	removed, err := c.Clean(context.Background(), config.ReplicationConfig{Factor: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}

func TestClean_NothingOverReplicated(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{}
	ops.EXPECT().Store().Return(ms).AnyTimes()

	c := NewOverReplicationCleaner(ops)
	removed, err := c.Clean(context.Background(), config.ReplicationConfig{Factor: 2, BatchSize: 10, Concurrency: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}
