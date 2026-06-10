// -------------------------------------------------------------------------------
// Progress - Observer Tests
//
// Author: Alex Freidah
//
// Covers Track's start/end bracketing, status propagation, and the nil-observer
// path that must still run the work.
// -------------------------------------------------------------------------------

package progress

import "testing"

func TestTrack_EmitsStartThenEnd(t *testing.T) {
	t.Parallel()
	var steps []Step
	ran := false
	Track(func(s Step) { steps = append(steps, s) }, "item-1", func() string {
		ran = true
		return StatusOK
	})

	if !ran {
		t.Fatal("work fn did not run")
	}
	if len(steps) != 2 {
		t.Fatalf("got %d steps, want 2 (start, end)", len(steps))
	}
	if steps[0].Phase != PhaseStart || steps[0].Label != "item-1" {
		t.Errorf("first step = %+v, want start/item-1", steps[0])
	}
	if steps[1].Phase != PhaseEnd || steps[1].Status != StatusOK || steps[1].Label != "item-1" {
		t.Errorf("second step = %+v, want end/ok/item-1", steps[1])
	}
	if steps[1].Duration < 0 {
		t.Errorf("duration = %v, want >= 0", steps[1].Duration)
	}
}

func TestTrack_PropagatesFailedStatus(t *testing.T) {
	t.Parallel()
	var end Step
	Track(func(s Step) {
		if s.Phase == PhaseEnd {
			end = s
		}
	}, "x", func() string { return StatusFailed })

	if end.Status != StatusFailed {
		t.Errorf("end status = %q, want failed", end.Status)
	}
}

func TestTrack_NilObserverStillRuns(t *testing.T) {
	t.Parallel()
	ran := false
	Track(nil, "x", func() string {
		ran = true
		return StatusOK
	})
	if !ran {
		t.Error("work fn did not run with a nil observer")
	}
}
