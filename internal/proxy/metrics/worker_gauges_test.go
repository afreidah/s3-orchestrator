// -------------------------------------------------------------------------------
// Worker Gauges Tests
//
// Author: Alex Freidah
//
// Covers a lock-holding instance publishing a worker's gauges and another
// instance serving them: the round trip, a published value carrying no figure,
// shared-state and decode errors, and a single instance with no shared state.
// The gauges are process-wide, so these tests do not run in parallel.
// -------------------------------------------------------------------------------

package metrics

import (
	"context"
	"errors"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// TestWorkerGauges_LoaderServesWhatTheHolderPublished publishes a rebalance
// plan size from one collector and loads it into another whose own value is
// stale.
func TestWorkerGauges_LoaderServesWhatTheHolderPublished(t *testing.T) {
	shared, _ := newShared()
	holder := testCollector(CollectorDeps{Shared: shared})
	other := testCollector(CollectorDeps{Shared: shared})

	n := int64(7)
	holder.PublishWorkerGauges(context.Background(), telemetry.GaugeSourceRebalancer,
		telemetry.WorkerGauges{RebalancePending: &n})
	if v := promtest.ToFloat64(telemetry.RebalancePending); v != 7 {
		t.Errorf("holder's gauge = %v, want 7", v)
	}

	telemetry.RebalancePending.Set(36999) // the other instance's stale value
	if err := other.LoadWorkerGauges(context.Background()); err != nil {
		t.Fatalf("LoadWorkerGauges: %v", err)
	}
	if v := promtest.ToFloat64(telemetry.RebalancePending); v != 7 {
		t.Errorf("rebalance pending after load = %v, want 7", v)
	}
}

// TestWorkerGauges_UnownedGaugesUntouched verifies a load leaves a gauge alone
// when the published value does not carry it.
func TestWorkerGauges_UnownedGaugesUntouched(t *testing.T) {
	shared, _ := newShared()
	mc := testCollector(CollectorDeps{Shared: shared})
	mc.PublishWorkerGauges(context.Background(), telemetry.GaugeSourceRebalancer, telemetry.WorkerGauges{})

	telemetry.RebalancePending.Set(11)
	if err := mc.LoadWorkerGauges(context.Background()); err != nil {
		t.Fatalf("LoadWorkerGauges: %v", err)
	}
	if v := promtest.ToFloat64(telemetry.RebalancePending); v != 11 {
		t.Errorf("rebalance pending = %v, want it untouched at 11", v)
	}
}

// TestWorkerGauges_WithoutSharedState applies locally and loads nothing.
func TestWorkerGauges_WithoutSharedState(t *testing.T) {
	mc := testCollector(CollectorDeps{})
	n := int64(5)
	mc.PublishWorkerGauges(context.Background(), telemetry.GaugeSourceRebalancer,
		telemetry.WorkerGauges{RebalancePending: &n})
	if v := promtest.ToFloat64(telemetry.RebalancePending); v != 5 {
		t.Errorf("rebalance pending = %v, want 5", v)
	}
	if err := mc.LoadWorkerGauges(context.Background()); err != nil {
		t.Errorf("LoadWorkerGauges without shared state: %v", err)
	}
}

// TestWorkerGauges_Errors reports shared-state and decode failures from the
// load while still applying the sources that read cleanly, and logs a failed
// publish without losing the local value.
func TestWorkerGauges_Errors(t *testing.T) {
	shared, mem := newShared()
	if err := mem.PutShared(context.Background(), "worker_gauges:"+telemetry.GaugeSourceRebalancer, []byte("not json"), 0); err != nil {
		t.Fatal(err)
	}
	mc := testCollector(CollectorDeps{Shared: shared})
	if err := mc.LoadWorkerGauges(context.Background()); err == nil {
		t.Error("LoadWorkerGauges accepted an undecodable value")
	}

	mem.Fail(errors.New("redis down"))
	if err := mc.LoadWorkerGauges(context.Background()); err == nil {
		t.Error("LoadWorkerGauges hid a shared-state error")
	}

	n := int64(9)
	mc.PublishWorkerGauges(context.Background(), telemetry.GaugeSourceRebalancer,
		telemetry.WorkerGauges{RebalancePending: &n})
	if v := promtest.ToFloat64(telemetry.RebalancePending); v != 9 {
		t.Errorf("rebalance pending after failed publish = %v, want 9 locally", v)
	}
}
