// -------------------------------------------------------------------------------
// UsageTracker Benchmarks
//
// Author: Alex Freidah
//
// Measures the hot-path cost of UsageTracker.WithinLimits and Record. These
// are called on every S3 request before the body is touched, so any
// regression in their per-call cost translates directly to higher latency
// at the proxy. The parallel variant exercises lock contention under load,
// pinning the contention behavior of the per-backend counter shards.
//
// Benchmarked against two pools rather than one: an operation charges every
// pool that contains it, so the pool count is what the check scales with.
// -------------------------------------------------------------------------------

package counter

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// benchLimits builds the two-backend fixture the benchmarks share: a metered
// backend with a class split, and one with a single aggregate budget.
func benchLimits(b *testing.B) map[string]core.UsageLimits {
	b.Helper()
	oci, err := core.NewUsageLimits(10<<30, 0, []core.PoolSpec{
		{Name: "writes", Operations: []string{string(s3op.PutObject), string(s3op.UploadPart)}, Limit: 50000},
		{Name: "reads", Operations: []string{string(s3op.GetObject), string(s3op.HeadObject)}, Limit: 500000},
	}, []s3op.Operation{s3op.DeleteObject})
	if err != nil {
		b.Fatalf("build oci limits: %v", err)
	}
	r2, err := core.NewUsageLimits(0, 0, core.SingleRequestPool(1000000), nil)
	if err != nil {
		b.Fatalf("build r2 limits: %v", err)
	}
	return map[string]core.UsageLimits{"oci": oci, "r2": r2}
}

// BenchmarkUsageTracker_WithinLimits measures the per-backend usage limit
// check called on every write request via eligibleForWrite.
func BenchmarkUsageTracker_WithinLimits(b *testing.B) {
	cb := NewLocalCounterBackend([]string{"oci", "r2"})
	tracker := NewUsageTracker(cb, benchLimits(b))
	tracker.SetBaseline("oci", core.UsageStat{APIRequests: 1000}, core.PoolUsage{"writes": 1000})

	ops := []s3op.Operation{s3op.PutObject}
	for b.Loop() {
		tracker.WithinLimits("oci", ops, 0, 1024)
	}
}

// BenchmarkUsageTracker_WithinLimits_Parallel measures counter contention
// on the limit check under concurrent request load.
func BenchmarkUsageTracker_WithinLimits_Parallel(b *testing.B) {
	cb := NewLocalCounterBackend([]string{"oci", "r2"})
	tracker := NewUsageTracker(cb, benchLimits(b))
	tracker.SetBaseline("oci", core.UsageStat{APIRequests: 1000}, core.PoolUsage{"writes": 1000})

	ops := []s3op.Operation{s3op.PutObject}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tracker.WithinLimits("oci", ops, 0, 1024)
		}
	})
}

// BenchmarkUsageTracker_Record measures the per-request usage counter
// increment called on every S3 operation.
func BenchmarkUsageTracker_Record(b *testing.B) {
	cb := NewLocalCounterBackend([]string{"oci"})
	tracker := NewUsageTracker(cb, benchLimits(b))

	for b.Loop() {
		tracker.Record("oci", s3op.PutObject, 1024, 0)
	}
}
