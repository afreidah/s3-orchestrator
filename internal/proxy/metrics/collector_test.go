// -------------------------------------------------------------------------------
// Metrics Collector - Replication Snapshot Tests
//
// Author: Alex Freidah
//
// Covers the replication snapshot the collector retains for the admin endpoint:
// under-replicated (distinct keys) and over-replicated counts when replication
// is enabled, and a ready, zeroed snapshot when it is disabled.
// -------------------------------------------------------------------------------

package metrics

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// fakeReplicationDeps is a metrics.Deps returning canned replication data; the
// non-replication methods are unused by updateReplicationPending.
type fakeReplicationDeps struct {
	under     []core.ObjectLocation
	over      int64
	plaintext int64
	countErr  error
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

func (fakeReplicationDeps) GetQuotaStats(context.Context) (map[string]core.QuotaStat, error) {
	return nil, nil
}
func (f fakeReplicationDeps) CountUnencryptedLocations(context.Context) (int64, error) {
	return f.plaintext, f.countErr
}
func (fakeReplicationDeps) GetObjectCounts(context.Context) (map[string]int64, error) {
	return nil, nil
}
func (fakeReplicationDeps) GetActiveMultipartCounts(context.Context) (map[string]int64, error) {
	return nil, nil
}
func (fakeReplicationDeps) GetUsageForPeriod(context.Context, string) (map[string]core.UsageStat, error) {
	return nil, nil
}
func (fakeReplicationDeps) GetPoolUsageForPeriod(context.Context, string) (map[string]core.PoolUsage, error) {
	return nil, nil
}
func (f fakeReplicationDeps) GetUnderReplicatedObjects(context.Context, int, int) ([]core.ObjectLocation, error) {
	return f.under, nil
}
func (f fakeReplicationDeps) CountOverReplicatedObjects(context.Context, int) (int64, error) {
	return f.over, nil
}

func TestCollector_ReplicationSnapshot(t *testing.T) {
	t.Parallel()
	// under: 3 locations across 2 distinct keys -> 2 under-replicated objects.
	under := []core.ObjectLocation{{ObjectKey: "a"}, {ObjectKey: "a"}, {ObjectKey: "b"}}
	mc := &Collector{
		store:             fakeReplicationDeps{under: under, over: 5},
		replicationFactor: func() int { return 2 },
		log:               slog.Default(),
	}

	if mc.ReplicationSnapshot().Ready {
		t.Error("snapshot should not be ready before the first compute")
	}

	mc.updateReplicationPending(context.Background())
	snap := mc.ReplicationSnapshot()
	if !snap.Ready || snap.Factor != 2 || snap.UnderReplicated != 2 || snap.OverReplicated != 5 {
		t.Errorf("snapshot = %+v", snap)
	}
	if snap.ComputedAt.IsZero() {
		t.Error("ComputedAt should be set")
	}
}

func TestCollector_ReplicationSnapshot_Disabled(t *testing.T) {
	t.Parallel()
	mc := &Collector{
		store:             fakeReplicationDeps{},
		replicationFactor: func() int { return 1 }, // disabled
		log:               slog.Default(),
	}
	mc.updateReplicationPending(context.Background())
	snap := mc.ReplicationSnapshot()
	if !snap.Ready || snap.Factor != 1 || snap.UnderReplicated != 0 || snap.OverReplicated != 0 {
		t.Errorf("disabled snapshot = %+v", snap)
	}
}

// TestCollector_PublishesPlaintextCopies keeps the figure refreshing on the
// periodic path. Computing it only when the web UI asks would leave a
// Prometheus-only deployment reading a gauge that never moves.
func TestCollector_PublishesPlaintextCopies(t *testing.T) {
	mc := &Collector{store: fakeReplicationDeps{plaintext: 17}, log: slog.Default()}

	mc.updatePlaintextCopies(context.Background())

	if got := promtest.ToFloat64(telemetry.EncryptionPlaintextCopies); got != 17 {
		t.Errorf("plaintext gauge = %v, want 17", got)
	}
}

// TestCollector_PlaintextCountFailureLeavesGauge guards against a failed count
// publishing zero. Reporting "no plaintext copies" because the query failed
// would read as a fully encrypted fleet.
func TestCollector_PlaintextCountFailureLeavesGauge(t *testing.T) {
	telemetry.EncryptionPlaintextCopies.Set(9)
	mc := &Collector{
		store: fakeReplicationDeps{plaintext: 0, countErr: errors.New("ledger unavailable")},
		log:   slog.Default(),
	}

	mc.updatePlaintextCopies(context.Background())

	if got := promtest.ToFloat64(telemetry.EncryptionPlaintextCopies); got != 9 {
		t.Errorf("gauge = %v after a failed count, want the previous value 9", got)
	}
}
