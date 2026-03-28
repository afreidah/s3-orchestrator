package counter

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store"
)

// BenchmarkUsageTracker_WithinLimits measures the per-backend usage limit
// check called on every write request via eligibleForWrite.
func BenchmarkUsageTracker_WithinLimits(b *testing.B) {
	cb := NewLocalCounterBackend([]string{"oci", "r2"})
	tracker := NewUsageTracker(cb, map[string]store.UsageLimits{
		"oci": {APIRequestLimit: 50000, EgressByteLimit: 10 << 30},
		"r2":  {APIRequestLimit: 1000000},
	})
	tracker.SetBaseline("oci", store.UsageStat{APIRequests: 1000})

	for b.Loop() {
		tracker.WithinLimits("oci", 1, 0, 1024)
	}
}

// BenchmarkUsageTracker_WithinLimits_Parallel measures RWMutex contention
// on the limit check under concurrent request load.
func BenchmarkUsageTracker_WithinLimits_Parallel(b *testing.B) {
	cb := NewLocalCounterBackend([]string{"oci", "r2"})
	tracker := NewUsageTracker(cb, map[string]store.UsageLimits{
		"oci": {APIRequestLimit: 50000, EgressByteLimit: 10 << 30},
		"r2":  {APIRequestLimit: 1000000},
	})
	tracker.SetBaseline("oci", store.UsageStat{APIRequests: 1000})

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tracker.WithinLimits("oci", 1, 0, 1024)
		}
	})
}

// BenchmarkUsageTracker_Record measures the per-request usage counter
// increment called on every S3 operation.
func BenchmarkUsageTracker_Record(b *testing.B) {
	cb := NewLocalCounterBackend([]string{"oci"})
	tracker := NewUsageTracker(cb, nil)

	for b.Loop() {
		tracker.Record("oci", 1, 1024, 0)
	}
}
