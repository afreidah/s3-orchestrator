// -------------------------------------------------------------------------------
// Backend Runtime - Timed Call Tests
//
// Author: Alex Freidah
//
// A call is timed for its backend's pressure, and only a call the backend
// timeout ended counts as a timeout: one the caller ended, by its own deadline
// or by leaving, is not timed at all. Each test runs in a synctest bubble.
// -------------------------------------------------------------------------------

package infra

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/counter/countertest"
)

// TestTimed_CountsOnlyTheBackendTimeout verifies calls the caller ended leave
// no sample, and a Get or OpenGet the backend timeout ended is recorded as a
// timeout.
func TestTimed_CountsOnlyTheBackendTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pressure := countertest.PressureTracker(t, []string{"b1"})
		usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
		rt := New(&Config{BackendTimeout: openTimeout, Usage: usage, Pressure: pressure})
		stuck := ctxBackend{holdOpen: true}

		deadline, cancel := context.WithTimeout(t.Context(), openTimeout/2)
		defer cancel()
		left, leave := context.WithCancel(t.Context())
		leave()
		for _, ctx := range []context.Context{deadline, left} {
			if _, _, err := rt.Get(ctx, "b1", stuck, "k", ""); err == nil {
				t.Fatal("Get answered against a backend that never answers")
			}
		}
		pressure.Evaluate(t.Context(), time.Now())
		if calls := pressure.State("b1").Calls; calls != 0 {
			t.Errorf("%v calls the caller ended were timed, want none", calls)
		}

		if _, _, err := rt.Get(t.Context(), "b1", stuck, "k", ""); err == nil {
			t.Fatal("Get answered against a backend that never answers")
		}
		if _, _, _, err := rt.OpenGet(t.Context(), "b1", stuck, "k"); err == nil {
			t.Fatal("OpenGet answered against a backend that never answers")
		}
		pressure.Evaluate(t.Context(), time.Now())
		if st := pressure.State("b1"); st.Calls < 1.9 || st.Deciding().Timeouts != 1 {
			t.Errorf("after the backend timeouts: %v calls, timeout share %v; want both counted as timeouts",
				st.Calls, st.Deciding().Timeouts)
		}
	})
}
