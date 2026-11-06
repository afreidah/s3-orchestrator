// -------------------------------------------------------------------------------
// Request Metric Label Helpers
//
// Author: Alex Freidah
//
// Reads the request metrics through a registry of their own, so a test can ask
// what series exist without creating one by naming it.
// -------------------------------------------------------------------------------

package s3api

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// methodSeries counts the request metric series labeled with method.
func methodSeries(t *testing.T, method string) int {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(telemetry.InflightRequests, telemetry.RequestsTotal, telemetry.RequestDuration)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	found := 0
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "method" && label.GetValue() == method {
					found++
				}
			}
		}
	}
	return found
}
