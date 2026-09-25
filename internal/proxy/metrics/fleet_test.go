// -------------------------------------------------------------------------------
// Fleet Snapshot Tests
//
// Author: Alex Freidah
//
// Covers publishing the fleet snapshot from the computing instance and loading
// it on the others: the round trip, a missing or undecodable snapshot, shared
// state errors, and a single instance with no shared state at all.
// -------------------------------------------------------------------------------

package metrics

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// memoryShared is a SharedState over one map, standing in for the Redis the
// instances share. putErr and getErr fail every call when set.
type memoryShared struct {
	mu     sync.Mutex
	data   map[string][]byte
	putErr error
	getErr error
}

func newMemoryShared() *memoryShared { return &memoryShared{data: map[string][]byte{}} }

func (m *memoryShared) PutShared(_ context.Context, name string, value []byte, _ time.Duration) error {
	if m.putErr != nil {
		return m.putErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[name] = value
	return nil
}

func (m *memoryShared) GetShared(_ context.Context, name string) ([]byte, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[name], nil
}

// panicDeps is a Deps whose every read fails the test: an instance that loads
// the fleet snapshot must not compute it.
type panicDeps struct{ Deps }

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestFleetSnapshot_LoaderServesComputedSnapshot publishes a snapshot from
// one collector and loads it into another that never reads the store.
func TestFleetSnapshot_LoaderServesComputedSnapshot(t *testing.T) {
	shared := newMemoryShared()
	under := []core.ObjectLocation{{ObjectKey: "a"}, {ObjectKey: "b"}}
	computing := &Collector{
		store:             fakeReplicationDeps{under: under, over: 1, plaintext: 4},
		replicationFactor: func() int { return 2 },
		shared:            shared,
		log:               slog.Default(),
	}
	loading := &Collector{store: panicDeps{}, shared: shared, log: slog.Default()}

	if err := computing.UpdateFleetMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateFleetMetrics: %v", err)
	}
	telemetry.EncryptionPlaintextCopies.Set(0)
	if err := loading.LoadFleetMetrics(context.Background()); err != nil {
		t.Fatalf("LoadFleetMetrics: %v", err)
	}

	want := computing.ReplicationSnapshot(context.Background())
	loading.shared = nil // read the loaded copy, not the shared one
	got := loading.ReplicationSnapshot(context.Background())
	if got.UnderReplicated != 2 || got.OverReplicated != 1 || !got.ComputedAt.Equal(want.ComputedAt) {
		t.Errorf("loaded snapshot = %+v, want %+v", got, want)
	}
	if v := promtest.ToFloat64(telemetry.EncryptionPlaintextCopies); v != 4 {
		t.Errorf("plaintext gauge = %v after load, want 4", v)
	}
}

// TestFleetSnapshot_LoadWithNothingPublished leaves the collector as it was
// when no instance has published yet.
func TestFleetSnapshot_LoadWithNothingPublished(t *testing.T) {
	t.Parallel()
	mc := &Collector{store: panicDeps{}, shared: newMemoryShared(), log: slog.Default()}
	if err := mc.LoadFleetMetrics(context.Background()); err != nil {
		t.Fatalf("LoadFleetMetrics: %v", err)
	}
	if mc.ReplicationSnapshot(context.Background()).Ready {
		t.Error("snapshot ready with nothing published")
	}
}

// TestFleetSnapshot_ReplicationReadsSharedOnEveryCall answers from the
// published snapshot even on a collector that has not loaded it yet, so
// instances whose flush ticks fall at different moments still agree. When the
// shared read fails, the collector answers from its own copy.
func TestFleetSnapshot_ReplicationReadsSharedOnEveryCall(t *testing.T) {
	t.Parallel()
	shared := newMemoryShared()
	computing := &Collector{
		store:             fakeReplicationDeps{under: []core.ObjectLocation{{ObjectKey: "a"}}},
		replicationFactor: func() int { return 2 },
		shared:            shared,
		log:               slog.Default(),
	}
	if err := computing.UpdateFleetMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateFleetMetrics: %v", err)
	}

	idle := &Collector{store: panicDeps{}, shared: shared, log: slog.Default()}
	if got := idle.ReplicationSnapshot(context.Background()); !got.Ready || got.UnderReplicated != 1 {
		t.Errorf("idle instance serves %+v, want the published snapshot", got)
	}

	shared.getErr = errors.New("redis down")
	if got := idle.ReplicationSnapshot(context.Background()); got.Ready {
		t.Errorf("with shared state failing, idle instance serves %+v, want its own empty copy", got)
	}
}

// TestFleetSnapshot_LoadErrors surfaces a shared-state failure and an
// undecodable snapshot rather than applying anything.
func TestFleetSnapshot_LoadErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("redis down")
	failing := newMemoryShared()
	failing.getErr = boom
	mc := &Collector{store: panicDeps{}, shared: failing, log: slog.Default()}
	if err := mc.LoadFleetMetrics(context.Background()); !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}

	garbled := newMemoryShared()
	garbled.data[fleetSnapshotKey] = []byte("{not json")
	mc = &Collector{store: panicDeps{}, shared: garbled, log: slog.Default()}
	if err := mc.LoadFleetMetrics(context.Background()); err == nil {
		t.Error("undecodable snapshot loaded without error")
	}
}

// TestFleetSnapshot_PublishFailureKeepsLocalResult still applies the snapshot
// on the computing instance when shared state refuses the write.
func TestFleetSnapshot_PublishFailureKeepsLocalResult(t *testing.T) {
	t.Parallel()
	shared := newMemoryShared()
	shared.putErr = errors.New("redis down")
	mc := &Collector{
		store:             fakeReplicationDeps{},
		replicationFactor: func() int { return 1 },
		shared:            shared,
		log:               slog.Default(),
	}
	if err := mc.UpdateFleetMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateFleetMetrics: %v", err)
	}
	if !mc.ReplicationSnapshot(context.Background()).Ready {
		t.Error("computing instance lost its own snapshot when the publish failed")
	}
}

// TestFleetSnapshot_SingleInstance runs with no shared state: the collector
// computes its own snapshot and loading is a no-op.
func TestFleetSnapshot_SingleInstance(t *testing.T) {
	t.Parallel()
	mc := &Collector{
		store:             fakeReplicationDeps{},
		replicationFactor: func() int { return 1 },
		log:               slog.Default(),
	}
	if err := mc.UpdateFleetMetrics(context.Background()); err != nil {
		t.Fatalf("UpdateFleetMetrics: %v", err)
	}
	if err := mc.LoadFleetMetrics(context.Background()); err != nil {
		t.Fatalf("LoadFleetMetrics: %v", err)
	}
	if !mc.ReplicationSnapshot(context.Background()).Ready {
		t.Error("single instance did not compute its own snapshot")
	}
}
