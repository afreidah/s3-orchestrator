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
	RebalancePending.Set(9)
	(&WorkerGauges{}).Apply()
	if v := promtest.ToFloat64(RebalancePending); v != 9 {
		t.Errorf("rebalance pending = %v, want 9 left alone", v)
	}
	n := int64(6)
	(&WorkerGauges{RebalancePending: &n}).Apply()
	if v := promtest.ToFloat64(RebalancePending); v != 6 {
		t.Errorf("rebalance pending = %v, want 6", v)
	}
}
