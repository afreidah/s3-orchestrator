// -------------------------------------------------------------------------------
// Admission Semaphore Sizing Tests
//
// Author: Alex Freidah
//
// Pins the admissionSemFor wiring contract documented in backend.go: split
// mode (both MaxConcurrentReads and MaxConcurrentWrites set) returns a
// channel sized to MaxConcurrentWrites that doubles as the writes-and-
// workers pool; merged mode (only MaxConcurrentRequests set) returns the
// global pool; any other shape returns nil. The asymmetry is intentional
// and worth a regression guard so a future "look this looks wrong"
// refactor doesn't quietly flip the semantics (#835).
// -------------------------------------------------------------------------------

package di

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// TestAdmissionSemFor_SplitMode_ReturnsWritesSizedChannel pins the
// split-pool sizing: when both reads and writes are set, the returned
// sem is sized to MaxConcurrentWrites (the writes-and-workers pool).
// The reads sem is created elsewhere (transport/httpserver/routes.go);
// this layer only owns the writes side.
func TestAdmissionSemFor_SplitMode_ReturnsWritesSizedChannel(t *testing.T) {
	t.Parallel()
	got := admissionSemFor(&config.ServerConfig{
		MaxConcurrentReads:  100,
		MaxConcurrentWrites: 50,
	})
	if got == nil {
		t.Fatal("expected non-nil sem in split mode")
	}
	if cap(got) != 50 {
		t.Errorf("cap(sem) = %d, want 50 (MaxConcurrentWrites)", cap(got))
	}
}

// TestAdmissionSemFor_MergedMode_ReturnsRequestsSizedChannel pins the
// merged-pool sizing: when only MaxConcurrentRequests is set, the
// returned sem is the global pool sized to that value.
func TestAdmissionSemFor_MergedMode_ReturnsRequestsSizedChannel(t *testing.T) {
	t.Parallel()
	got := admissionSemFor(&config.ServerConfig{
		MaxConcurrentRequests: 200,
	})
	if got == nil {
		t.Fatal("expected non-nil sem in merged mode")
	}
	if cap(got) != 200 {
		t.Errorf("cap(sem) = %d, want 200 (MaxConcurrentRequests)", cap(got))
	}
}

// TestAdmissionSemFor_NoLimits_ReturnsNil pins the "no admission cap"
// shape: no fields set returns nil. The routes.go switch reads this as
// "do not install the admission middleware."
func TestAdmissionSemFor_NoLimits_ReturnsNil(t *testing.T) {
	t.Parallel()
	got := admissionSemFor(&config.ServerConfig{})
	if got != nil {
		t.Errorf("expected nil sem with no limits, got cap=%d", cap(got))
	}
}

// TestAdmissionSemFor_LoneSplitLimit pins that setting only one split limit
// still splits the pools: the shared semaphore is the write pool, which takes
// the write limit when set and the shared limit, or the default, otherwise.
// Setting one limit used to install no admission at all.
func TestAdmissionSemFor_LoneSplitLimit(t *testing.T) {
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
			if got := admissionSemFor(&tc.cfg); got == nil || cap(got) != tc.want {
				t.Errorf("sem = %v (cap %d), want cap %d", got != nil, cap(got), tc.want)
			}
		})
	}
}
