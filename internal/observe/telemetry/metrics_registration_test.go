// -------------------------------------------------------------------------------
// Metrics Registration Smoke Test
//
// Author: Alex Freidah
//
// Verifies that promauto's auto-registration on package init still wires
// every s3o_* family into prometheus.DefaultGatherer after the metrics
// definitions were split across metrics_<domain>.go files. Catches the
// failure mode where a domain file's var block is removed or its imports
// are stripped during a rename.
// -------------------------------------------------------------------------------

package telemetry

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestMetricsRegistration_KeyFamiliesPresent touches one representative
// metric from each domain file and confirms it appears in the default
// gatherer. promauto only emits a CounterVec/GaugeVec family in Gather()
// once a label set has been observed; this both proves the var-block
// initialised cleanly and that the underlying CollectAndCount surface is
// wired into the default registry.
func TestMetricsRegistration_KeyFamiliesPresent(t *testing.T) {
	// Touch one metric per domain file so a series materialises and the
	// family appears in Gather(). Picking a representative metric from each
	// file means a misplaced var block (or a domain file silently dropped)
	// would surface here as a missing family below.
	type touch struct {
		name string
		fn   func()
	}
	touches := []touch{
		{"s3o_requests_total", func() { RequestsTotal.WithLabelValues("GET", "200").Inc() }},
		{"s3o_backend_requests_total", func() { BackendRequestsTotal.WithLabelValues("b", "GET", "ok").Inc() }},
		{"s3o_quota_bytes_used", func() { QuotaBytesUsed.WithLabelValues("b").Set(0) }},
		{"s3o_rebalance_runs_total", func() { RebalanceRunsTotal.WithLabelValues("pack", "ok").Inc() }},
		{"s3o_replication_runs_total", func() { ReplicationRunsTotal.WithLabelValues("ok").Inc() }},
		{"s3o_circuit_breaker_state", func() { CircuitBreakerState.WithLabelValues("test-reg").Set(0) }},
		{"s3o_cleanup_queue_depth", func() { CleanupQueueDepth.Set(0) }},
		{"s3o_pending_intents_depth", func() { PendingIntentsDepth.Set(0) }},
		{"s3o_pending_intents_enqueued_total", func() { PendingIntentsEnqueuedTotal.Inc() }},
		{"s3o_pending_intents_resolved_total", func() { PendingIntentsResolvedTotal.WithLabelValues("test").Inc() }},
		{"s3o_audit_events_total", func() { AuditEventsTotal.WithLabelValues("test").Inc() }},
		{"s3o_encryption_operations_total", func() { EncryptionOpsTotal.WithLabelValues("encrypt").Inc() }},
		{"s3o_cache_hits_total", func() { CacheHitsTotal.Inc() }},
		{"s3o_build_info", func() { BuildInfo.WithLabelValues("test", "go").Set(1) }},
	}
	for _, t := range touches {
		t.fn()
	}

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	got := make(map[string]struct{}, len(families))
	for _, mf := range families {
		got[mf.GetName()] = struct{}{}
	}

	for _, tc := range touches {
		if _, ok := got[tc.name]; !ok {
			s3o := []string{}
			for n := range got {
				if strings.HasPrefix(n, "s3o_") {
					s3o = append(s3o, n)
				}
			}
			t.Errorf("metric %q not registered (saw %d s3o_* families)", tc.name, len(s3o))
		}
	}
}
