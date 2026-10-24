// -------------------------------------------------------------------------------
// Integration Tests - Usage Cycles
//
// Author: Alex Freidah
//
// Usage is recorded in hourly buckets and each backend's cycle is read as the
// sum of its buckets from the cycle's start. These tests run one backend on a
// cycle that starts mid-month in a zone off UTC, against the real store, and
// check that admission and the reported usage cover only the current cycle.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestUsageCycle_ReadsFromTheZonedCycleStart seeds flushed usage on both sides
// of a Tokyo-midnight cycle start, then checks only the side inside the cycle
// counts, and that a real write is flushed into the cycle and read back.
func TestUsageCycle_ReadsFromTheZonedCycleStart(t *testing.T) {
	const name = "cycle-1"
	h := newHarness(t, harnessSpec{Backends: []harnessBackend{{Name: name, Quota: 64 << 20}}})
	ctx := context.Background()
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	lim, err := core.NewUsageLimits(1000, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	lim.Period = core.UsagePeriod{ResetDay: 10, Location: tokyo}
	tracker := h.stack.Runtime.Usage()
	tracker.UpdateLimits(map[string]core.UsageLimits{name: lim})

	start := lim.Period.Cycle(time.Now()).Start
	for bucket, egress := range map[time.Time]int64{start.Add(-time.Hour): 5000, start: 300} {
		if _, err := h.db.ExecContext(ctx,
			`INSERT INTO backend_usage (backend_name, bucket_start, workload, egress_bytes) VALUES ($1, $2, 'client', $3)`,
			name, bucket, egress); err != nil {
			t.Fatalf("seed bucket %v: %v", bucket, err)
		}
	}

	if _, err := tracker.RefreshUsage(ctx, h.store); err != nil {
		t.Fatalf("RefreshUsage: %v", err)
	}
	if got := tracker.CycleUsage()[name].Usage.EgressBytes; got != 300 {
		t.Errorf("cycle egress = %d, want only the 300 from the cycle start", got)
	}
	if !tracker.WithinLimits(name, s3op.GetObject, 700, 0) || tracker.WithinLimits(name, s3op.GetObject, 701, 0) {
		t.Error("admission should leave exactly 700 bytes of egress this cycle")
	}

	h.put("cycle/object", make([]byte, 128))
	if err := h.stack.Usage.FlushUsage(ctx); err != nil {
		t.Fatalf("FlushUsage: %v", err)
	}
	if _, err := tracker.RefreshUsage(ctx, h.store); err != nil {
		t.Fatalf("RefreshUsage after the write: %v", err)
	}
	if got := tracker.CycleUsage()[name].Usage; got.IngressBytes != 128 || got.EgressBytes != 300 {
		t.Errorf("cycle usage after the write = %+v, want 128 ingress and 300 egress", got)
	}
}
