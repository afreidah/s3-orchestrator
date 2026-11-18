// -------------------------------------------------------------------------------
// Counter Test Doubles - Trackers in a Known State
//
// Author: Alex Freidah
//
// Builds counter trackers already in the state a test in another package is
// about, through the trackers' own API, so the test needs no knowledge of how
// they judge.
// -------------------------------------------------------------------------------

// Package countertest builds counter trackers in known states for tests.
package countertest

import (
	"context"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// PressureTracker returns a tracker over backends under the default policy,
// with each backend in pressured under slow pressure and the rest under none.
func PressureTracker(tb testing.TB, backends []string, pressured ...string) *counter.PressureTracker {
	tb.Helper()
	cfg := config.BackendPressureConfig{PressurePolicy: config.DefaultPressurePolicy()}
	tr := counter.NewPressureTracker(backends, syncutil.Fixed(&cfg))
	for _, name := range pressured {
		for range 30 {
			tr.Observe(name, 10*time.Millisecond, false)
		}
		for range 30 {
			tr.Observe(name, time.Second, false)
		}
	}
	tr.Evaluate(context.Background(), time.Now())
	for _, name := range pressured {
		if tr.Pressure(name) == 0 {
			tb.Fatalf("backend %s did not come under pressure", name)
		}
	}
	return tr
}
