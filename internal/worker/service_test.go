// -------------------------------------------------------------------------------
// Worker Service Tests
//
// Author: Alex Freidah
//
// Pins what the shared pass service builds from a spec: the service is named
// for the workload, its interval comes from the config when the config sets
// one, a tick runs the pass against the current config and refreshes the quota
// gauges only after a pass that changed something, and a startup pass runs
// only when asked for.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// heldLock is an advisory locker that always grants the lock and runs fn.
type heldLock struct{}

// refreshCounter counts quota gauge refreshes.
type refreshCounter struct{ n atomic.Int32 }

// passProbe is a spec under test with its config and the passes it saw.
type passProbe struct {
	cfg     *config.RebalanceConfig
	changed int
	passes  atomic.Int32
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// WithAdvisoryLock runs fn as the holder of the lock.
func (heldLock) WithAdvisoryLock(ctx context.Context, _ int64, fn func(ctx context.Context) error) (bool, error) {
	return true, fn(ctx)
}

// UpdateQuotaMetrics counts one refresh.
func (r *refreshCounter) UpdateQuotaMetrics(context.Context) error {
	r.n.Add(1)
	return nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// service builds the pass service over the probe's config and pass.
func (p *passProbe) service(t *testing.T, refresh *refreshCounter, atStartup bool) *tickrunner.Service {
	t.Helper()
	svc, ok := passService(&passSpec[config.RebalanceConfig]{
		Workload:   audit.WorkloadRebalance,
		Lock:       1,
		Config:     func() *config.RebalanceConfig { return p.cfg },
		Interval:   time.Hour,
		Configured: func(cfg *config.RebalanceConfig) time.Duration { return cfg.Interval },
		Enabled:    func(cfg *config.RebalanceConfig) bool { return cfg.Enabled },
		Pass: func(context.Context, *config.RebalanceConfig) (int, error) {
			p.passes.Add(1)
			return p.changed, nil
		},
		CountKey:     "objects_moved",
		RunAtStartup: atStartup,
	}, refresh, heldLock{}).(*tickrunner.Service)
	if !ok {
		t.Fatal("passService did not build a tick service")
	}
	return svc
}

// TestPassService_NamedForWorkloadWithConfiguredInterval verifies the service
// carries the workload's name and the config's interval, falling back to the
// default when the config sets none.
func TestPassService_NamedForWorkloadWithConfiguredInterval(t *testing.T) {
	t.Parallel()
	configured := (&passProbe{cfg: &config.RebalanceConfig{Interval: 2 * time.Hour}}).service(t, &refreshCounter{}, false)
	if configured.Name() != string(audit.WorkloadRebalance) || configured.Interval() != 2*time.Hour {
		t.Errorf("service = %q every %v, want rebalance every 2h", configured.Name(), configured.Interval())
	}
	if unset := (&passProbe{}).service(t, &refreshCounter{}, false); unset.Interval() != time.Hour {
		t.Errorf("interval without a config = %v, want the 1h default", unset.Interval())
	}
}

// TestPassService_TickRefreshesOnlyAfterChange verifies a tick runs the pass
// and refreshes the quota gauges only when the pass changed something.
func TestPassService_TickRefreshesOnlyAfterChange(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		changed     int
		wantRefresh int32
	}{
		{"pass changed objects", 3, 1},
		{"pass changed nothing", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe := &passProbe{cfg: &config.RebalanceConfig{Enabled: true}, changed: tc.changed}
			refresh := &refreshCounter{}
			probe.service(t, refresh, false).Tick(t.Context())
			if probe.passes.Load() != 1 || refresh.n.Load() != tc.wantRefresh {
				t.Errorf("passes=%d refreshes=%d, want 1 and %d", probe.passes.Load(), refresh.n.Load(), tc.wantRefresh)
			}
		})
	}
}

// TestPassService_NoConfigRunsNoPass verifies a tick before any config is set
// runs nothing.
func TestPassService_NoConfigRunsNoPass(t *testing.T) {
	t.Parallel()
	probe := &passProbe{}
	probe.service(t, &refreshCounter{}, false).Tick(t.Context())
	if probe.passes.Load() != 0 {
		t.Errorf("passes = %d, want none without a config", probe.passes.Load())
	}
}

// TestPassService_StartupPassOnlyWhenAsked verifies a startup pass runs as the
// service starts only when the spec asks for one.
func TestPassService_StartupPassOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	for _, atStartup := range []bool{true, false} {
		probe := &passProbe{cfg: &config.RebalanceConfig{Enabled: true}}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := probe.service(t, &refreshCounter{}, atStartup).Run(ctx); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if want := map[bool]int32{true: 1, false: 0}[atStartup]; probe.passes.Load() != want {
			t.Errorf("RunAtStartup=%v: passes = %d, want %d", atStartup, probe.passes.Load(), want)
		}
	}
}
