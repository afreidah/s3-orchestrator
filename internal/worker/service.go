// -------------------------------------------------------------------------------
// Worker Services - Shared Tick Service Construction
//
// Author: Alex Freidah
//
// The replicator, the over-replication cleaner and the rebalancer each run one
// pass per tick under their own reloadable config: the config sets the tick
// interval and whether the worker runs at all, and the pass reports how much it
// changed. passService builds that service once, named for the worker's
// workload like everything else the worker reports.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
)

// passSpec describes a worker that runs one pass per tick under its config C.
// Interval is the tick when the config sets none, and Configured reads the one
// it sets. Enabled decides from the config whether a tick runs; an unset config
// never runs. Pass runs one cycle and returns how much it changed, which is
// logged under CountKey. RunAtStartup also runs a pass as the service starts.
type passSpec[C any] struct {
	Workload     audit.Workload
	Lock         int64
	Config       func() *C
	Interval     time.Duration
	Configured   func(*C) time.Duration
	Enabled      func(*C) bool
	Pass         func(ctx context.Context, cfg *C) (int, error)
	CountKey     string
	RunAtStartup bool
}

// passService builds the advisory-locked tick service for spec. After a pass
// that changed something it logs the count and refreshes manager's quota
// gauges.
func passService[C any](spec *passSpec[C], manager tickrunner.QuotaMetricsRefresher, locker tickrunner.AdvisoryLocker) lifecycle.Runner {
	log := workerLogger(spec.Workload)
	work := func(ctx context.Context) error {
		cfg := spec.Config()
		if cfg == nil {
			return nil
		}
		n, err := spec.Pass(ctx, cfg)
		return tickrunner.HandlePassResult(ctx, log, manager, n, err, spec.CountKey)
	}
	interval := spec.Interval
	if cfg := spec.Config(); cfg != nil && spec.Configured(cfg) > 0 {
		interval = spec.Configured(cfg)
	}
	svc := tickrunner.Config{
		Locker:   locker,
		Interval: interval,
		LockID:   spec.Lock,
		Name:     string(spec.Workload),
		Log:      log,
		ShouldRun: func() bool {
			cfg := spec.Config()
			return cfg != nil && spec.Enabled(cfg)
		},
		Work: work,
	}
	if spec.RunAtStartup {
		svc.Startup = work
	}
	return tickrunner.New(svc)
}
