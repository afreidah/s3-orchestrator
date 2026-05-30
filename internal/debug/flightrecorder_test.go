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

// TestFlightRecorder_RunStop drives the Start/block-on-ctx/Stop loop.
// Not parallel: at most one runtime/trace.FlightRecorder may be active
// per process.
func TestFlightRecorder_RunStop(t *testing.T) {
	s := NewFlightRecorderService(10 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Give Run a chance to reach Start before we cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil on ctx cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after ctx cancel")
	}

	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("Stop returned %v", err)
	}
}
