// -------------------------------------------------------------------------------
// Worker Gauges Tests
//
// Author: Alex Freidah
//
// Covers the two paths a worker's shared gauges take: through a wired
// publisher, and applied locally when none is wired.
// -------------------------------------------------------------------------------

package telemetry

import (
	"context"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
)

// recordingPublisher captures the last publish.
type recordingPublisher struct {
	source string
	g      WorkerGauges
}

// PublishWorkerGauges records the publish so the test can read back what the
// worker shared.
func (p *recordingPublisher) PublishWorkerGauges(_ context.Context, source string, g WorkerGauges) {
	p.source, p.g = source, g
}

// TestSharedGauges_ThroughPublisher verifies a wired publisher receives the
// gauges under the worker's source and the local gauge is left alone.
func TestSharedGauges_ThroughPublisher(t *testing.T) {
	CleanupQueueDepth.Set(0)
	p := &recordingPublisher{}
	var s SharedGauges
	s.SetGaugePublisher(p)

	n := int64(4)
	s.PublishGauges(context.Background(), GaugeSourceCleanup, WorkerGauges{CleanupQueueDepth: &n})

	if p.source != GaugeSourceCleanup || p.g.CleanupQueueDepth == nil || *p.g.CleanupQueueDepth != 4 {
		t.Errorf("published %q %+v, want cleanup depth 4", p.source, p.g)
	}
	if v := promtest.ToFloat64(CleanupQueueDepth); v != 0 {
		t.Errorf("local cleanup depth = %v, want 0 when a publisher is wired", v)
	}
}

// TestSharedGauges_Locally verifies the zero value, which has no publisher,
// sets the gauges locally.
func TestSharedGauges_Locally(t *testing.T) {
	var s SharedGauges
	n := int64(6)
	s.PublishGauges(context.Background(), GaugeSourcePendingReaper, WorkerGauges{PendingIntentsDepth: &n})
	if v := promtest.ToFloat64(PendingIntentsDepth); v != 6 {
		t.Errorf("pending intents = %v, want 6", v)
	}
}
