// -------------------------------------------------------------------------------
// Worker Gauges Tests
//
// Author: Alex Freidah
//
// Pins that applying a worker's figures sets the gauges it carries and leaves
// the ones it does not own alone.
// -------------------------------------------------------------------------------

package telemetry

import (
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
)

// TestWorkerGauges_ApplySetsOnlyWhatItCarries verifies a carried figure sets
// its gauge and a nil one leaves its gauge as it was.
func TestWorkerGauges_ApplySetsOnlyWhatItCarries(t *testing.T) {
	CleanupDLQDepth.Set(9)
	n := int64(6)
	g := WorkerGauges{PendingIntentsDepth: &n}
	g.Apply()
	if v := promtest.ToFloat64(PendingIntentsDepth); v != 6 {
		t.Errorf("pending intents = %v, want 6", v)
	}
	if v := promtest.ToFloat64(CleanupDLQDepth); v != 9 {
		t.Errorf("cleanup DLQ = %v, want 9 left alone", v)
	}
}
