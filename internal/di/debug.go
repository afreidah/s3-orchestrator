// -------------------------------------------------------------------------------
// DI - Debug Service Providers
//
// Author: Alex Freidah
//
// Optional providers for diagnostic services gated by config.Debug.
// Today this is just the FlightRecorder lifecycle wrapper; future debug
// toggles land here so the main injector body stays a flat list.
// -------------------------------------------------------------------------------

package di

import (
	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/debug"
)

// ProvideFlightRecorderService constructs the always-on runtime/trace
// FlightRecorder wrapper that the admin trace-snapshot endpoint streams
// from. Only registered when cfg.Debug.FlightRecorder.Enabled is true, so
// resolution failure means the operator did not opt in.
func ProvideFlightRecorderService(i do.Injector) (*debug.FlightRecorderService, error) {
	cfg, err := do.Invoke[*config.Config](i)
	if err != nil {
		return nil, err
	}
	return debug.NewFlightRecorderService(cfg.Debug.FlightRecorder.MinAge), nil
}
