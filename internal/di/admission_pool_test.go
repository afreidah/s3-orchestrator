// -------------------------------------------------------------------------------
// Admission Pool Sizing Tests
//
// Author: Alex Freidah
//
// Pins the admissionPoolsFor wiring contract documented in backend.go: split
// mode builds a read pool and a write pool, the write pool sized to
// MaxConcurrentWrites and shared with background work; merged mode builds one
// pool that serves as both; no limits builds neither. The asymmetry is
// intentional, and a regression guard keeps a refactor from quietly flipping
// it.
// -------------------------------------------------------------------------------

package di

import (
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/admission"
	"github.com/afreidah/s3-orchestrator/internal/config"
)

// slots counts the requests pool admits at once before one waits past a
// nanosecond and is refused.
func slots(t *testing.T, pool *admission.Pool) int {
	t.Helper()
	n := 0
	for {
		if _, err := pool.Acquire(t.Context(), "", time.Nanosecond); err != nil {
			return n
		}
		n++
	}
}

// checkPool fails the test unless pool is named name and admits want at once.
func checkPool(t *testing.T, pool *admission.Pool, name string, want int) {
	t.Helper()
	if pool == nil || pool.Name() != name {
		t.Fatalf("pool = %v, want the %s pool", pool, name)
	}
	if n := slots(t, pool); n != want {
		t.Errorf("%s pool slots = %d, want %d", name, n, want)
	}
}

// TestAdmissionPoolsFor_SplitMode pins the split-pool sizing: with reads and
// writes both set, each gets a pool of its own, sized to its limit.
func TestAdmissionPoolsFor_SplitMode(t *testing.T) {
	t.Parallel()
	pools := admissionPoolsFor(&config.ServerConfig{MaxConcurrentReads: 100, MaxConcurrentWrites: 50})
	checkPool(t, pools.Reads, "read", 100)
	checkPool(t, pools.Writes, "write", 50)
}

// TestAdmissionPoolsFor_MergedMode pins the merged-pool sizing: with only
// MaxConcurrentRequests set, one pool sized to it serves as both.
func TestAdmissionPoolsFor_MergedMode(t *testing.T) {
	t.Parallel()
	pools := admissionPoolsFor(&config.ServerConfig{MaxConcurrentRequests: 200})
	if pools.Reads != pools.Writes {
		t.Fatal("merged mode built two pools, want one serving as both")
	}
	checkPool(t, pools.Writes, "shared", 200)
}

// TestAdmissionPoolsFor_NoLimits pins the "no admission cap" shape: with no
// fields set neither pool is built, and no admission middleware is installed.
func TestAdmissionPoolsFor_NoLimits(t *testing.T) {
	t.Parallel()
	if pools := admissionPoolsFor(&config.ServerConfig{}); pools.Reads != nil || pools.Writes != nil {
		t.Errorf("pools = %+v with no limits, want none", pools)
	}
}

// TestAdmissionPoolsFor_LoneSplitLimit pins that setting only one split limit
// still splits the pools: the write pool takes the write limit when set and
// the shared limit, or the default, otherwise.
func TestAdmissionPoolsFor_LoneSplitLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  config.ServerConfig
		want int
	}{
		{"reads_only_with_requests", config.ServerConfig{MaxConcurrentReads: 100, MaxConcurrentRequests: 300}, 300},
		{"writes_only_with_requests", config.ServerConfig{MaxConcurrentWrites: 100, MaxConcurrentRequests: 300}, 100},
		{"reads_only_no_requests", config.ServerConfig{MaxConcurrentReads: 100}, config.DefaultMaxConcurrentRequests},
		{"writes_only_no_requests", config.ServerConfig{MaxConcurrentWrites: 100}, 100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkPool(t, admissionPoolsFor(&tc.cfg).Writes, "write", tc.want)
		})
	}
}
