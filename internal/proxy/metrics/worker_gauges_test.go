// -------------------------------------------------------------------------------
// Worker Gauges Tests
//
// Author: Alex Freidah
//
// Covers a lock-holding instance publishing a worker's gauges and another
// instance serving them: the round trip, a worker that published only some
// gauges, nothing published, shared-state and decode errors, and a single
// instance with no shared state. The gauges are process-wide, so these tests
// do not run in parallel.
// -------------------------------------------------------------------------------

package metrics

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// TestWorkerGauges_LoaderServesWhatTheHolderPublished publishes a cleanup
// depth from one collector and loads it into another whose own value is stale.
func TestWorkerGauges_LoaderServesWhatTheHolderPublished(t *testing.T) {
	shared := newMemoryShared()
	holder := &Collector{shared: shared, log: slog.Default()}
	other := &Collector{shared: shared, log: slog.Default()}

	depth, dlq := int64(7), int64(2)
	holder.PublishWorkerGauges(context.Background(), telemetry.GaugeSourceCleanup,
		telemetry.WorkerGauges{CleanupQueueDepth: &depth, CleanupDLQDepth: &dlq})
	if v := promtest.ToFloat64(telemetry.CleanupQueueDepth); v != 7 {
		t.Errorf("holder's gauge = %v, want 7", v)
	}

	telemetry.CleanupQueueDepth.Set(36999) // the other instance's stale value
	telemetry.CleanupDLQDepth.Set(0)
	if err := other.LoadWorkerGauges(context.Background()); err != nil {
		t.Fatalf("LoadWorkerGauges: %v", err)
	}
	if v := promtest.ToFloat64(telemetry.CleanupQueueDepth); v != 7 {
		t.Errorf("cleanup depth after load = %v, want 7", v)
	}
	if v := promtest.ToFloat64(telemetry.CleanupDLQDepth); v != 2 {
		t.Errorf("DLQ depth after load = %v, want 2", v)
	}
}

// TestWorkerGauges_UnownedGaugesUntouched verifies a load leaves a gauge alone
// when the published value does not carry it.
func TestWorkerGauges_UnownedGaugesUntouched(t *testing.T) {
	shared := newMemoryShared()
	mc := &Collector{shared: shared, log: slog.Default()}
	pending := int64(3)
	mc.PublishWorkerGauges(context.Background(), telemetry.GaugeSourcePendingReaper,
		telemetry.WorkerGauges{PendingIntentsDepth: &pending})

	telemetry.RebalancePending.Set(11)
	if err := mc.LoadWorkerGauges(context.Background()); err != nil {
		t.Fatalf("LoadWorkerGauges: %v", err)
	}
	if v := promtest.ToFloat64(telemetry.RebalancePending); v != 11 {
		t.Errorf("rebalance pending = %v, want it untouched at 11", v)
	}
	if v := promtest.ToFloat64(telemetry.PendingIntentsDepth); v != 3 {
		t.Errorf("pending intents = %v, want 3", v)
	}
}

// TestWorkerGauges_WithoutSharedState applies locally and loads nothing.
func TestWorkerGauges_WithoutSharedState(t *testing.T) {
	mc := &Collector{log: slog.Default()}
	n := int64(5)
	mc.PublishWorkerGauges(context.Background(), telemetry.GaugeSourceNotifier,
		telemetry.WorkerGauges{NotificationQueueDepth: &n})
	if v := promtest.ToFloat64(telemetry.NotificationQueueDepth); v != 5 {
		t.Errorf("notification depth = %v, want 5", v)
	}
	if err := mc.LoadWorkerGauges(context.Background()); err != nil {
		t.Errorf("LoadWorkerGauges without shared state: %v", err)
	}
}

// TestWorkerGauges_Errors reports shared-state and decode failures from the
// load while still applying the sources that read cleanly, and logs a failed
// publish without losing the local value.
func TestWorkerGauges_Errors(t *testing.T) {
	shared := newMemoryShared()
	shared.data[workerGaugesKey(telemetry.GaugeSourceScrubber)] = []byte("not json")
	mc := &Collector{shared: shared, log: slog.Default()}
	if err := mc.LoadWorkerGauges(context.Background()); err == nil {
		t.Error("LoadWorkerGauges accepted an undecodable value")
	}

	shared.getErr = errors.New("redis down")
	if err := mc.LoadWorkerGauges(context.Background()); err == nil {
		t.Error("LoadWorkerGauges hid a shared-state error")
	}

	shared.putErr = errors.New("redis down")
	n := int64(9)
	mc.PublishWorkerGauges(context.Background(), telemetry.GaugeSourceRebalancer,
		telemetry.WorkerGauges{RebalancePending: &n})
	if v := promtest.ToFloat64(telemetry.RebalancePending); v != 9 {
		t.Errorf("rebalance pending after failed publish = %v, want 9 locally", v)
	}
}
