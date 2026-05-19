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

// TestAdmissionSemFor_PartialSplit_FallsToMerged pins the fallthrough
// branch: when only one of reads/writes is set but the other is zero,
// the split-mode case does not match. The function falls through to
// MaxConcurrentRequests, or to nil if that is also unset. This is the
// non-obvious shape future readers most likely to misinterpret.
func TestAdmissionSemFor_PartialSplit_FallsToMerged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  config.ServerConfig
		want int // -1 means expect nil
	}{
		{
			name: "reads_only_with_requests",
			cfg:  config.ServerConfig{MaxConcurrentReads: 100, MaxConcurrentRequests: 300},
			want: 300,
		},
		{
			name: "writes_only_with_requests",
			cfg:  config.ServerConfig{MaxConcurrentWrites: 100, MaxConcurrentRequests: 300},
			want: 300,
		},
		{
			name: "reads_only_no_requests",
			cfg:  config.ServerConfig{MaxConcurrentReads: 100},
			want: -1,
		},
		{
			name: "writes_only_no_requests",
			cfg:  config.ServerConfig{MaxConcurrentWrites: 100},
			want: -1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := admissionSemFor(&tc.cfg)
			if tc.want == -1 {
				if got != nil {
					t.Errorf("expected nil, got cap=%d", cap(got))
				}
				return
			}
			if got == nil {
				t.Fatalf("expected non-nil sem with cap=%d", tc.want)
			}
			if cap(got) != tc.want {
				t.Errorf("cap(sem) = %d, want %d", cap(got), tc.want)
			}
		})
	}
}
