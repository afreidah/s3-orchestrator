// -------------------------------------------------------------------------------
// Backend Runtime - Admission Tests
//
// Author: Alex Freidah
//
// Background work is admitted from the pool HTTP writes share, as a request
// flow of its own. Who a freed slot goes to is the pool's to test; these check
// the runtime admits through it, gives the slot back, and admits without limit
// when no pool is wired.
// -------------------------------------------------------------------------------

package infra

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/afreidah/s3-orchestrator/internal/admission"
)

// TestAdmission_NoPoolAdmits verifies a runtime with no pool admits background
// work at once and its release does nothing.
func TestAdmission_NoPoolAdmits(t *testing.T) {
	t.Parallel()
	release, ok := (&BackendRuntime{}).AcquireAdmission(t.Context())
	if !ok {
		t.Fatal("AcquireAdmission refused with no pool wired")
	}
	release()
}

// TestAdmission_TakesAndGivesBackASlot verifies background work holds a slot
// of the wired pool until it releases it, and is refused once its context
// ends while it waits for one.
func TestAdmission_TakesAndGivesBackASlot(t *testing.T) {
	t.Parallel()
	rt := New(&Config{Admission: admission.New("write", 2, admission.Metrics{
		Queued: prometheus.NewGauge(prometheus.GaugeOpts{Name: "queued"}),
		Waited: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "waited"}),
	})})
	release, ok := rt.AcquireAdmission(t.Context())
	if !ok {
		t.Fatal("AcquireAdmission refused on an idle pool")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, ok := rt.AcquireAdmission(ctx); ok {
		t.Error("AcquireAdmission admitted past background work's half of the pool")
	}
	release()
	if _, ok := rt.AcquireAdmission(ctx); !ok {
		t.Error("AcquireAdmission refused the slot given back")
	}
}
