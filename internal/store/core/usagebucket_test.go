// -------------------------------------------------------------------------------
// Usage Bucket Tests
//
// Author: Alex Freidah
//
// Covers the hourly usage buckets: where a time falls, how a batch folds
// charges into buckets and walks them back, and how a store read's rows fold
// into usage per backend and workload.
// -------------------------------------------------------------------------------

package core

import (
	"errors"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestUsageBucket_TruncatesToTheUTCHour asserts a time in any zone lands in
// the UTC hour that contains it.
func TestUsageBucket_TruncatesToTheUTCHour(t *testing.T) {
	t.Parallel()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	in := time.Date(2026, 3, 1, 19, 45, 12, 5, ny)

	got := UsageBucket(in)

	if want := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC); !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("UsageBucket(%v) = %v, want %v in UTC", in, got, want)
	}
}

// TestBucketUsage_AddFoldsPerBucketAndWorkload asserts charges to the same
// bucket and workload add up while other buckets and workloads stay apart.
func TestBucketUsage_AddFoldsPerBucketAndWorkload(t *testing.T) {
	t.Parallel()
	h1 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	h2 := h1.Add(UsageBucketSize)
	b := BucketUsage{}

	b.Add(h1, "client", Usage{APIRequests: 2, Pools: PoolUsage{"reads": 2}})
	b.Add(h1, "client", Usage{APIRequests: 3, Pools: PoolUsage{"reads": 3}})
	b.Add(h1, "scrub", Usage{EgressBytes: 9})
	b.Add(h2, "client", Usage{APIRequests: 1})

	if c := b[h1]["client"]; c.APIRequests != 5 || c.Pools["reads"] != 5 {
		t.Errorf("h1 client = %+v, want 5 requests and reads=5", c)
	}
	if s := b[h1]["scrub"]; s.EgressBytes != 9 {
		t.Errorf("h1 scrub = %+v, want 9 egress", s)
	}
	if c := b[h2]["client"]; c.APIRequests != 1 {
		t.Errorf("h2 client = %+v, want 1 request", c)
	}
}

// TestUsageBatch_EachVisitsEveryEntryAndStopsOnError asserts Each reaches
// every bucket and workload, and returns the first error fn gives.
func TestUsageBatch_EachVisitsEveryEntryAndStopsOnError(t *testing.T) {
	t.Parallel()
	h1 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	b := &UsageBatch{Buckets: BucketUsage{}}
	b.Buckets.Add(h1, "client", Usage{APIRequests: 2})
	b.Buckets.Add(h1, "scrub", Usage{APIRequests: 3})
	b.Buckets.Add(h1.Add(UsageBucketSize), "client", Usage{APIRequests: 4})

	var total int64
	if err := b.Each(func(_ time.Time, _ string, u *Usage) error {
		total += u.APIRequests
		return nil
	}); err != nil || total != 9 {
		t.Errorf("Each = %v with %d requests, want nil and 9", err, total)
	}

	stop := errors.New("stop")
	calls := 0
	err := b.Each(func(time.Time, string, *Usage) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Errorf("Each = %v after %d calls, want %v after 1", err, calls, stop)
	}
}

// TestAddUsageRow_FoldsTotalsOnceAndPoolsPerRow asserts a workload read back
// as one row per pool keeps its totals once and gains every pool, and a row
// with no pool sets only the totals.
func TestAddUsageRow_FoldsTotalsOnceAndPoolsPerRow(t *testing.T) {
	t.Parallel()
	usage := map[string]WorkloadUsage{}
	stat := UsageStat{APIRequests: 7, EgressBytes: 70}
	reads, writes := "reads", "writes"
	var four, three int64 = 4, 3

	AddUsageRow(usage, "b1", "client", stat, &reads, &four)
	AddUsageRow(usage, "b1", "client", stat, &writes, &three)
	AddUsageRow(usage, "b2", "scrub", UsageStat{IngressBytes: 5}, nil, nil)

	c := usage["b1"]["client"]
	if c.UsageStat != stat || c.Pools[reads] != 4 || c.Pools[writes] != 3 {
		t.Errorf("b1 client = %+v, want totals %+v once and reads=4 writes=3", c, stat)
	}
	if s := usage["b2"]["scrub"]; s.IngressBytes != 5 || len(s.Pools) != 0 {
		t.Errorf("b2 scrub = %+v, want 5 ingress and no pools", s)
	}
}
