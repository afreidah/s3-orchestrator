// -------------------------------------------------------------------------------
// DI - Ops Layer Wiring Tests
//
// Author: Alex Freidah
//
// The ops services are unit-tested against stubbed collaborators, so a
// dependency ProvideOps never populates leaves every one of those tests
// passing while the endpoint reports the feature as unavailable. These tests
// resolve the real ops layer out of the injector and exercise it far enough to
// prove the collaborator arrived.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/ops"
)

// TestProvideOps_LifecycleExpirerIsWired pins that the on-demand expiration
// sweep reaches an expiry manager. Run declines with ErrLifecycleUnavailable
// when its collaborator is nil, which is indistinguishable at the transport
// from a deployment that legitimately has no lifecycle manager.
func TestProvideOps_LifecycleExpirerIsWired(t *testing.T) {
	t.Parallel()
	cfg := happyPathConfig(t.TempDir())
	cfg.Lifecycle = config.LifecycleConfig{
		Rules: []config.LifecycleRule{{Prefix: "test-bucket/logs/", ExpirationDays: 7}},
	}
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("config validation: %v", err)
	}
	inj := NewInjector(InjectorDeps{Config: cfg, Mode: "all", LogLevel: new(slog.LevelVar), LogBuffer: telemetry.NewLogBuffer()})
	t.Cleanup(func() { _ = inj.Shutdown() })

	svc, err := do.Invoke[*ops.Services](inj)
	if err != nil {
		t.Fatalf("resolve ops.Services: %v", err)
	}

	// The rule matches nothing in an empty ledger, so a wired manager sweeps
	// and reports zero. Only an unwired one refuses to sweep at all.
	if _, err := svc.Lifecycle.Run(context.Background(), nil); errors.Is(err, ops.ErrLifecycleUnavailable) {
		t.Fatal("ops.Lifecycle has no expiry manager; ProvideOps must pass one into ops.Deps")
	} else if err != nil {
		t.Fatalf("Lifecycle.Run: %v", err)
	}
}
