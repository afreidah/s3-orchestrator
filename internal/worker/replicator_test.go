package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"go.uber.org/mock/gomock"
)

func TestReplicator_SetConfig_RoundTrip(t *testing.T) {
	ctrl := gomock.NewController(t)
	r := NewReplicator(NewMockOps(ctrl))
	if r.Config() != nil {
		t.Fatal("expected nil config before set")
	}
	cfg := &config.ReplicationConfig{Factor: 3}
	r.SetConfig(cfg)
	if got := r.Config(); got.Factor != 3 {
		t.Errorf("Config().Factor = %d, want 3", got.Factor)
	}
}

func TestFindReplicaTarget_SelectsBackendWithSpace(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	be1 := backendtest.NewMockObjectBackend(ctrl)
	be2 := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"})
	ops.EXPECT().ExcludeDraining([]string{"b1", "b2"}).Return([]string{"b1", "b2"})
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": be1, "b2": be2}).AnyTimes()

	r := NewReplicator(ops)
	stats := map[string]store.QuotaStat{
		"b1": {BytesUsed: 900, BytesLimit: 1000}, // 100 free
		"b2": {BytesUsed: 100, BytesLimit: 1000}, // 900 free
	}
	exclusion := map[string]bool{"b1": true} // b1 already has copy

	target := r.FindReplicaTarget(context.Background(), stats, "key1", 50, exclusion)
	if target != "b2" {
		t.Errorf("FindReplicaTarget = %q, want b2", target)
	}
}

func TestFindReplicaTarget_NoSpace(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)

	ops.EXPECT().BackendOrder().Return([]string{"b1"})
	ops.EXPECT().ExcludeDraining([]string{"b1"}).Return([]string{"b1"})
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{
		"b1": backendtest.NewMockObjectBackend(ctrl),
	}).AnyTimes()

	r := NewReplicator(ops)
	stats := map[string]store.QuotaStat{
		"b1": {BytesUsed: 990, BytesLimit: 1000},
	}

	target := r.FindReplicaTarget(context.Background(), stats, "key1", 50, nil)
	if target != "" {
		t.Errorf("FindReplicaTarget = %q, want empty (no space)", target)
	}
}

func TestCopyToReplica_Success(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(nil)

	r := NewReplicator(ops)
	copies := []store.ObjectLocation{{BackendName: "b1"}}

	src, err := r.CopyToReplica(context.Background(), "key1", copies, "b2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src != "b1" {
		t.Errorf("source = %q, want b1", src)
	}
}

func TestCopyToReplica_AllSourcesFail(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(fmt.Errorf("read: timeout"))

	r := NewReplicator(ops)
	copies := []store.ObjectLocation{{BackendName: "b1"}}

	_, err := r.CopyToReplica(context.Background(), "key1", copies, "b2")
	if err == nil {
		t.Fatal("expected error when all sources fail")
	}
}

func TestCleanupOrphan_DelegatesToDeleteOrEnqueue(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	be := backendtest.NewMockObjectBackend(ctrl)

	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": be})
	ops.EXPECT().DeleteOrEnqueue(gomock.Any(), be, "b1", "key1", "replication_orphan", int64(100))
	ops.EXPECT().Usage().Return(newTestUsageTracker())

	r := NewReplicator(ops)
	r.CleanupOrphan(context.Background(), "b1", "key1", 100)
}

func TestReplicate_FactorOne_Noop(t *testing.T) {
	ctrl := gomock.NewController(t)
	r := NewReplicator(NewMockOps(ctrl))

	created, err := r.Replicate(context.Background(), config.ReplicationConfig{Factor: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created != 0 {
		t.Errorf("created = %d, want 0", created)
	}
}

func TestReplicate_NothingUnderReplicated(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{}

	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{}).AnyTimes()

	r := NewReplicator(ops)
	cfg := config.ReplicationConfig{Factor: 2, BatchSize: 10, Concurrency: 1, UnhealthyThreshold: time.Hour}
	created, err := r.Replicate(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created != 0 {
		t.Errorf("created = %d, want 0", created)
	}
}

func TestReplicateObject_Success(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	srcBe := backendtest.NewMockObjectBackend(ctrl)
	dstBe := backendtest.NewMockObjectBackend(ctrl)

	ms := &mockMetadataStore{recordReplicaOK: true}

	ops.EXPECT().Store().Return(ms).AnyTimes()
	ops.EXPECT().BackendOrder().Return([]string{"b1", "b2"}).AnyTimes()
	ops.EXPECT().ExcludeDraining(gomock.Any()).Return([]string{"b1", "b2"}).AnyTimes()
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe}).AnyTimes()
	ops.EXPECT().GetBackend("b2").Return(dstBe, nil)
	ops.EXPECT().StreamCopy(gomock.Any(), srcBe, dstBe, "key1").Return(nil)
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()

	r := NewReplicator(ops)
	copies := []store.ObjectLocation{{BackendName: "b1", SizeBytes: 50}}
	stats := map[string]store.QuotaStat{
		"b1": {BytesUsed: 100, BytesLimit: 1000},
		"b2": {BytesUsed: 100, BytesLimit: 1000},
	}

	created, err := r.ReplicateObject(context.Background(), stats, "key1", copies, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created != 1 {
		t.Errorf("created = %d, want 1", created)
	}
	if ms.replicaRecorded != 1 {
		t.Errorf("replicaRecorded = %d, want 1", ms.replicaRecorded)
	}
}
