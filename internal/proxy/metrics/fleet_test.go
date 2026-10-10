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
	computing := &Collector{
		store:             fakeReplicationDeps{under: 2, over: 1, plaintext: 4},
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
		store:             fakeReplicationDeps{under: 1},
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

// TestRefreshFleetIfStale_RecomputesAStaleSnapshot replaces a published
// snapshot older than half the interval with a fresh computation.
func TestRefreshFleetIfStale_RecomputesAStaleSnapshot(t *testing.T) {
	t.Parallel()
	shared := newMemoryShared()
	old := time.Now().Add(-time.Minute)
	shared.data[fleetSnapshotKey] = []byte(`{"computed_at":"` + old.Format(time.RFC3339Nano) + `"}`)
	mc := &Collector{
		store:             fakeReplicationDeps{under: 3},
		replicationFactor: func() int { return 2 },
		shared:            shared,
		fleetInterval:     time.Minute,
		log:               slog.Default(),
	}
	if err := mc.RefreshFleetIfStale(context.Background()); err != nil {
		t.Fatalf("RefreshFleetIfStale: %v", err)
	}
	if got := mc.ReplicationSnapshot(context.Background()); got.UnderReplicated != 3 || !got.ComputedAt.After(old) {
		t.Errorf("snapshot = %+v, want a fresh computation", got)
	}
}

// TestRefreshFleetIfStale_LoadFailureRecomputes computes the snapshot when the
// published one cannot be read, rather than leaving the gauges unrefreshed.
func TestRefreshFleetIfStale_LoadFailureRecomputes(t *testing.T) {
	t.Parallel()
	shared := newMemoryShared()
	shared.getErr = errors.New("redis down")
	mc := &Collector{
		store:             fakeReplicationDeps{under: 2},
		replicationFactor: func() int { return 2 },
		shared:            shared,
		fleetInterval:     time.Minute,
		log:               slog.Default(),
	}
	if err := mc.RefreshFleetIfStale(context.Background()); err != nil {
		t.Fatalf("RefreshFleetIfStale: %v", err)
	}
	shared.getErr = nil
	if got := mc.ReplicationSnapshot(context.Background()); got.UnderReplicated != 2 {
		t.Errorf("snapshot = %+v, want the recomputed one", got)
	}
}

// TestRefreshFleetIfStale_SingleInstanceAlwaysComputes has nothing published
// to reuse without shared state, so every tick computes.
func TestRefreshFleetIfStale_SingleInstanceAlwaysComputes(t *testing.T) {
	t.Parallel()
	mc := New(&CollectorDeps{
		Store:             fakeReplicationDeps{over: 4},
		ReplicationFactor: func() int { return 2 },
	})
	if err := mc.RefreshFleetIfStale(context.Background()); err != nil {
		t.Fatalf("RefreshFleetIfStale: %v", err)
	}
	if got := mc.ReplicationSnapshot(context.Background()); got.OverReplicated != 4 {
		t.Errorf("snapshot = %+v, want a computed one", got)
	}
}

// TestFleetSnapshot_LookupOrder serves the published snapshot first, then the
// last one applied locally, and computes one only when neither exists.
func TestFleetSnapshot_LookupOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	shared := newMemoryShared()
	publisher := &Collector{
		store:             fakeReplicationDeps{under: 1},
		replicationFactor: func() int { return 2 },
		shared:            shared,
		log:               slog.Default(),
	}
	if err := publisher.UpdateFleetMetrics(ctx); err != nil {
		t.Fatalf("UpdateFleetMetrics: %v", err)
	}

	reader := &Collector{store: panicDeps{}, shared: shared, log: slog.Default()}
	snap, err := reader.FleetSnapshot(ctx)
	if err != nil || snap.Replication.UnderReplicated != 1 {
		t.Fatalf("published lookup = %+v, %v; want the published snapshot", snap, err)
	}

	// With shared state down the reader serves the copy it last loaded, and
	// the panicking store proves it does not compute one.
	if err := reader.LoadFleetMetrics(ctx); err != nil {
		t.Fatalf("LoadFleetMetrics: %v", err)
	}
	shared.getErr = errors.New("redis down")
	snap, err = reader.FleetSnapshot(ctx)
	if err != nil || snap.Replication.UnderReplicated != 1 {
		t.Errorf("local lookup = %+v, %v; want the loaded copy", snap, err)
	}
}

// TestFleetSnapshot_ComputesWhenNoneExists computes and keeps a snapshot on
// first use, then serves the kept one.
func TestFleetSnapshot_ComputesWhenNoneExists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mc := New(&CollectorDeps{
		Store:             fakeReplicationDeps{over: 2},
		ReplicationFactor: func() int { return 2 },
	})
	first, err := mc.FleetSnapshot(ctx)
	if err != nil || first.Replication.OverReplicated != 2 {
		t.Fatalf("first lookup = %+v, %v; want a computed snapshot", first, err)
	}
	second, err := mc.FleetSnapshot(ctx)
	if err != nil || second != first {
		t.Errorf("second lookup = %p, %v; want the kept snapshot %p", second, err, first)
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
