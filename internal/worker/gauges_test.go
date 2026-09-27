// -------------------------------------------------------------------------------
// Worker Gauges Tests
//
// Author: Alex Freidah
//
// Covers the two paths a worker's gauges take: through a wired publisher, and
// applied locally when none is wired.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// recordingPublisher captures the last publish.
type recordingPublisher struct {
	source string
	g      telemetry.WorkerGauges
}

// PublishWorkerGauges records the publish.
func (p *recordingPublisher) PublishWorkerGauges(_ context.Context, source string, g telemetry.WorkerGauges) {
	p.source, p.g = source, g
}

// TestPublishGauges_ThroughPublisher verifies a wired publisher receives the
// gauges under the worker's source.
func TestPublishGauges_ThroughPublisher(t *testing.T) {
	t.Parallel()
	p := &recordingPublisher{}
	n := int64(4)
	publishGauges(context.Background(), p, telemetry.GaugeSourceCleanup, telemetry.WorkerGauges{CleanupQueueDepth: &n})
	if p.source != telemetry.GaugeSourceCleanup || p.g.CleanupQueueDepth == nil || *p.g.CleanupQueueDepth != 4 {
		t.Errorf("published %q %+v, want cleanup depth 4", p.source, p.g)
	}
}

// TestPublishGauges_Locally verifies the gauges are set locally when no
// publisher is wired.
func TestPublishGauges_Locally(t *testing.T) {
	n := int64(6)
	publishGauges(context.Background(), nil, telemetry.GaugeSourcePendingReaper, telemetry.WorkerGauges{PendingIntentsDepth: &n})
	if v := promtest.ToFloat64(telemetry.PendingIntentsDepth); v != 6 {
		t.Errorf("pending intents = %v, want 6", v)
	}
}
