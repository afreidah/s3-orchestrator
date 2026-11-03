// -------------------------------------------------------------------------------
// Operation Metrics Tests
//
// Author: Alex Freidah
//
// Pins that a recorded operation is counted under the status its error gives
// it and timed under its operation and backend.
// -------------------------------------------------------------------------------

package telemetry

import (
	"errors"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
)

// TestOperationMetrics_RecordLabelsStatus records one success and one failure
// and checks each lands on its own status label.
func TestOperationMetrics_RecordLabelsStatus(t *testing.T) {
	t.Parallel()
	const op, backend = "op-record-test", "backend-record-test"

	ManagerOps.Record(op, backend, time.Now(), nil)
	ManagerOps.Record(op, backend, time.Now(), errors.New("boom"))
	ManagerOps.Record(op, backend, time.Now(), errors.New("boom"))

	if got := promtest.ToFloat64(ManagerOps.requests.WithLabelValues(op, backend, "success")); got != 1 {
		t.Errorf("success count = %v, want 1", got)
	}
	if got := promtest.ToFloat64(ManagerOps.requests.WithLabelValues(op, backend, "error")); got != 2 {
		t.Errorf("error count = %v, want 2", got)
	}
	if promtest.CollectAndCount(ManagerOps.duration) == 0 {
		t.Error("no duration observed")
	}
}
