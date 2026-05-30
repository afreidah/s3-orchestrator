// -------------------------------------------------------------------------------
// FlightRecorder Lifecycle Service
//
// Author: Alex Freidah
//
// Thin lifecycle-manager adapter around runtime/trace.FlightRecorder (Go 1.25).
// The recorder runs always-on in a bounded ring buffer; the admin
// trace-snapshot endpoint streams the last MinAge of trace bytes on demand.
// -------------------------------------------------------------------------------

// Package debug houses opt-in diagnostic services wired by DI when the
// matching config block is enabled.
package debug

import (
	"context"
	"log/slog"
	"runtime/trace"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
)

// FlightRecorderService adapts *trace.FlightRecorder to lifecycle.Runner and
// lifecycle.Stopper so the supervisor owns its start/stop and the admin
// handler can hold the recorder for snapshot reads.
type FlightRecorderService struct {
	fr     *trace.FlightRecorder
	minAge time.Duration
	log    *slog.Logger
}

// NewFlightRecorderService constructs a service around a FlightRecorder
// configured with the given minimum window age. The recorder is not started
// here; Run starts it so a registration-time failure (another recorder
// already active) surfaces through the supervisor.
func NewFlightRecorderService(minAge time.Duration) *FlightRecorderService {
	return &FlightRecorderService{
		fr:     trace.NewFlightRecorder(trace.FlightRecorderConfig{MinAge: minAge}),
		minAge: minAge,
		log:    slog.Default().With(logfmt.Component("flight-recorder")),
	}
}

// Recorder returns the underlying *trace.FlightRecorder for the admin
// snapshot handler to call WriteTo on. Nil-safe so the admin handler can
// receive a nil service when the feature is disabled and check before use.
func (s *FlightRecorderService) Recorder() *trace.FlightRecorder {
	if s == nil {
		return nil
	}
	return s.fr
}

// Run starts the recorder and blocks until ctx is cancelled. A Start failure
// (only happens if another recorder is already active in-process) is returned
// so the supervisor logs it instead of silently no-op'ing.
func (s *FlightRecorderService) Run(ctx context.Context) error {
	if err := s.fr.Start(); err != nil {
		return err
	}
	s.log.InfoContext(ctx, "flight recorder started", "min_age", s.minAge)
	<-ctx.Done()
	return nil
}

// Stop ends recording so the ring buffer can be GC'd. Idempotent: calling
// Stop on a recorder that never started or already stopped is safe.
func (s *FlightRecorderService) Stop(_ context.Context) error {
	s.fr.Stop()
	return nil
}
