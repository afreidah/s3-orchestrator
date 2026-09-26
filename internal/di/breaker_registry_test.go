// -------------------------------------------------------------------------------
// Breaker Registry Wiring Tests
//
// Author: Alex Freidah
//
// ProvideBreakerRegistry is where every breaker the watchdog probes is
// enrolled: the database breaker, and a recovery prober per backend breaker,
// charging its health checks through the shared usage tracker.
// -------------------------------------------------------------------------------

package di

import (
	"errors"
	"testing"
	"time"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
)

// TestProvideBreakerRegistry_EnrollsEveryBreaker verifies the registry holds
// the database breaker plus one prober per backend breaker.
func TestProvideBreakerRegistry_EnrollsEveryBreaker(t *testing.T) {
	t.Parallel()
	b1 := backend.NewCircuitBreakerBackend(backendtest.NewInMemory(), backend.CircuitBreakerConfig{Name: "b1", Threshold: 1, Timeout: time.Minute})
	b2 := backend.NewCircuitBreakerBackend(backendtest.NewInMemory(), backend.CircuitBreakerConfig{Name: "b2", Threshold: 1, Timeout: time.Minute})
	dbCB := breaker.NewCircuitBreaker(breaker.Config{
		Name: "database", Threshold: 1, Timeout: time.Minute,
		IsError: func(err error) bool { return err != nil }, Sentinel: errors.New("db down"),
	})

	inj := do.New()
	do.ProvideValue(inj, &config.Config{})
	do.ProvideValue(inj, dbCB)
	do.ProvideValue(inj, &BackendsResult{
		Backends: map[string]backend.ObjectBackend{"b1": b1, "b2": b2},
		Order:    []string{"b1", "b2"},
		Breakers: []*backend.CircuitBreakerBackend{b1, b2},
	})
	do.Provide(inj, ProvideUsageTracker)

	reg, err := ProvideBreakerRegistry(inj)
	if err != nil {
		t.Fatalf("ProvideBreakerRegistry: %v", err)
	}
	if reg.Len() != 3 {
		t.Errorf("registry holds %d breakers, want 3 (database + two backends)", reg.Len())
	}
	reg.ProbeAll(t.Context())
}
