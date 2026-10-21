// -------------------------------------------------------------------------------
// Flight Recorder Tests
//
// Author: Alex Freidah
//
// Covers the recorder's lifecycle and its nil-safety. A nil recorder is the
// normal state for a deployment that never enabled it, so every method has to
// tolerate one rather than the caller guarding each call.
// -------------------------------------------------------------------------------

package debug

import (
	"context"
	"testing"
	"time"
)

// TestFlightRecorder_NilSafe pins that Recorder() on a nil service
// returns nil instead of panicking — the admin handler relies on this.
func TestFlightRecorder_NilSafe(t *testing.T) {
	t.Parallel()
	var s *FlightRecorderService
	if r := s.Recorder(); r != nil {
		t.Errorf("Recorder() on nil service = %v, want nil", r)
	}
}

// TestFlightRecorder_RecorderExposesUnderlying verifies a constructed
// service hands back a non-nil *trace.FlightRecorder.
func TestFlightRecorder_RecorderExposesUnderlying(t *testing.T) {
	t.Parallel()
	s := NewFlightRecorderService(50 * time.Millisecond)
	if s.Recorder() == nil {
		t.Fatal("Recorder() on constructed service is nil")
	}
}

// TestFlightRecorder_RunStop asserts Run turns recording on before it waits for
// cancellation and leaves it on, and Stop turns it off. Run is driven with a
// cancelled context so it returns at once; the recorder is not safe to query
// while another goroutine starts it. Not parallel: at most one
// runtime/trace.FlightRecorder may be active per process.
func TestFlightRecorder_RunStop(t *testing.T) {
	s := NewFlightRecorderService(10 * time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run returned %v, want nil on ctx cancel", err)
	}
	if !s.Recorder().Enabled() {
		t.Fatal("Run returned without recording")
	}

	if err := s.Stop(t.Context()); err != nil {
		t.Errorf("Stop returned %v", err)
	}
	if s.Recorder().Enabled() {
		t.Error("recording still on after Stop")
	}
}
