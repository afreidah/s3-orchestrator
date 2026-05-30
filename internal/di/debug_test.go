package di

import (
	"testing"
	"time"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// TestProvideFlightRecorderService verifies the provider resolves cfg
// and hands back a non-nil service when debug.flight_recorder is on.
func TestProvideFlightRecorderService(t *testing.T) {
	t.Parallel()
	inj := do.New()
	do.ProvideValue(inj, &config.Config{
		Debug: config.DebugConfig{
			FlightRecorder: config.FlightRecorderConfig{
				Enabled: true,
				MinAge:  50 * time.Millisecond,
			},
		},
	})
	fr, err := ProvideFlightRecorderService(inj)
	if err != nil {
		t.Fatalf("ProvideFlightRecorderService: %v", err)
	}
	if fr == nil {
		t.Fatal("ProvideFlightRecorderService returned nil")
	}
	if fr.Recorder() == nil {
		t.Fatal("service returned nil *trace.FlightRecorder")
	}
}
