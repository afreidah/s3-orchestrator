// -------------------------------------------------------------------------------
// Watchdog Tests
//
// Author: Alex Freidah
//
// Drives the watchdog on a fake clock at its production interval: it probes
// every registered breaker once per tick, charges the probes to the probe
// workload, and returns when cancelled.
// -------------------------------------------------------------------------------

package breaker

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
)

// TestWatchdog_ProbesEachTickUntilCancelled asserts one probe per breaker per
// interval, under the probe workload, and a clean return on cancel.
func TestWatchdog_ProbesEachTickUntilCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := &fakeProber{}, &fakeProber{}
		w := NewWatchdog(NewRegistry(a, b))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() { done <- w.Run(ctx) }()

		synctest.Sleep(3 * DefaultWatchdogInterval)
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run returned %v on cancel, want nil", err)
		}

		for _, p := range []*fakeProber{a, b} {
			if got := p.count.Load(); got != 3 {
				t.Errorf("probes after three intervals = %d, want 3", got)
			}
			if got := p.workload.Load(); got != audit.WorkloadProbe {
				t.Errorf("probe workload = %v, want %q", got, audit.WorkloadProbe)
			}
		}
	})
}
