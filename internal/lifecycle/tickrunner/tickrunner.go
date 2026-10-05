// -------------------------------------------------------------------------------
// Tick Runner - Advisory-Locked Periodic Service Primitive
//
// Author: Alex Freidah
//
// Shared lifecycle.Runner implementation that drives a worker function on a
// fixed interval under a PostgreSQL advisory lock. Owns audit-context
// creation per tick, lock-busy / startup-jitter handling, per-service
// health snapshotting, and the worker-tick telemetry counters. Moved out
// of internal/di to its own package so worker subsystems can construct
// their own services without going through DI, leaving DI focused on
// wiring.
// -------------------------------------------------------------------------------

package tickrunner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// MsgPassFailed / MsgPassCompleted / MsgQuotaMetricsRefreshFailed are
// the canonical terminal-event log messages shared by the periodic
// "pass" workers (rebalance, replication, over-replication). Held as
// constants so the three nearly-identical work closures cannot drift.
const (
	MsgPassFailed                = "pass failed"
	MsgPassCompleted             = "pass completed"
	MsgQuotaMetricsRefreshFailed = "quota metrics refresh failed after pass"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// AdvisoryLocker is the consumer-defined slice of the metadata store a
// Service needs: one TryAdvisoryLock call per tick. The concrete
// metadata store satisfies this implicitly.
type AdvisoryLocker interface {
	WithAdvisoryLock(ctx context.Context, lockID int64, fn func(ctx context.Context) error) (bool, error)
}

// QuotaMetricsRefresher is the single-method subset of
// *infra.BackendRuntime that HandlePassResult calls to push fresh quota
// gauges after a successful worker pass. Lives here so the worker
// packages can take it as a typed dep without importing DI.
type QuotaMetricsRefresher interface {
	UpdateQuotaMetrics(ctx context.Context) error
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// ComponentLogger returns the canonical scoped logger every Service
// uses, derived from the snake_case slug so the component attr is the
// single source of truth for log filtering. Callers typically pass the
// same slug as Service.Name.
func ComponentLogger(slug string) *slog.Logger {
	return slog.Default().With(logfmt.Component(slug))
}

// QueueWork adapts a queue-draining pass into a Config.Work function. Both
// queue workers report the same way: run one pass, and say what it did only if
// it did something. A quiet queue is the normal state, so logging every tick
// would write a line a minute per instance forever and bury the ticks that
// matter.
func QueueWork(log *slog.Logger, msg string, pass func(ctx context.Context) (succeeded, failed int)) func(context.Context) error {
	return func(ctx context.Context) error {
		succeeded, failed := pass(ctx)
		if succeeded > 0 || failed > 0 {
			log.InfoContext(ctx, msg, "processed", succeeded, "failed", failed)
		}
		return nil
	}
}

// HandlePassResult is the shared post-call handling for the three
// nearly-identical "pass" workers (rebalance, over-replication,
// replication). Each one returns (count, err) from a worker call and
// then either: surfaces non-DB errors as tick failures (so health
// reporting sees them), or - when work was done - logs a completion
// message with a work-specific count key and refreshes quota metrics
// so the dashboard reflects the move. Centralised so the three
// closures cannot drift, and so coverage of the error and count>0
// branches lands in one place.
func HandlePassResult(ctx context.Context, log *slog.Logger, manager QuotaMetricsRefresher, count int, err error, countKey string) error {
	if err != nil {
		if errors.Is(err, core.ErrDBUnavailable) {
			return nil
		}
		return err
	}
	if count > 0 {
		log.InfoContext(ctx, MsgPassCompleted, countKey, count)
		if qerr := manager.UpdateQuotaMetrics(ctx); qerr != nil {
			log.WarnContext(ctx, MsgQuotaMetricsRefreshFailed, "error", qerr)
		}
	}
	return nil
}

// Config bundles the inputs needed to construct a Service. Held as a
// struct so callers can pin a few fields and leave the rest at their
// zero values rather than threading a long argument list. ShouldRun,
// Startup, and OnError are optional; Work and Name are required.
//
// Name is the canonical snake_case component slug, used for both the metrics
// label and the logger's component attribute, so the two cannot disagree.
// ShouldRun skips a tick without recording a failure, which is how a service
// its config disabled at runtime stays quiet rather than reporting errors.
// OnError replaces the default failure log for services that also bump their
// own metric.
type Config struct {
	Locker   AdvisoryLocker
	Interval time.Duration // between ticks, post-jitter
	LockID   int64         // core.LockRebalancer and friends
	Name     string
	Log      *slog.Logger // ComponentLogger(Name) is the default contract

	ShouldRun func() bool
	Startup   func(ctx context.Context) error // runs once before the first tick
	Work      func(ctx context.Context) error // required
	OnError   func(err error)
}

// New constructs a Service from cfg. Required fields (Locker, Interval,
// LockID, Name, Log, Work) must be non-nil/positive; the constructor
// trusts the caller to supply them.
//
//nolint:gocritic // hugeParam: Config is a value struct by design; copying it once at construction is fine and the call sites read better than a pointer.
func New(cfg Config) *Service {
	return &Service{
		locker:    cfg.Locker,
		interval:  cfg.Interval,
		lockID:    cfg.LockID,
		name:      cfg.Name,
		log:       cfg.Log,
		shouldRun: cfg.ShouldRun,
		startup:   cfg.Startup,
		work:      cfg.Work,
		onError:   cfg.OnError,
	}
}

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Service runs a function on a fixed interval under an advisory lock.
// Handles audit context creation, lock acquisition, skip/error logging,
// and context cancellation. The component identity lives on the scoped
// logger (component attr) rather than in message text, so logs from
// every service share the same shape and operators filter by attribute.
//
// Per-service health state (lastSuccess, lastFailure, lastError,
// consecutiveFailures) is recorded after each tick so operators can
// query worker liveness through the admin endpoint and alert on
// staleness through Prometheus.
type Service struct {
	locker   AdvisoryLocker
	interval time.Duration
	lockID   int64
	name     string
	log      *slog.Logger

	shouldRun func() bool
	startup   func(ctx context.Context) error
	work      func(ctx context.Context) error
	onError   func(err error)

	healthMu sync.Mutex
	health   lifecycle.WorkerHealth
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Tick drives a single iteration of the work function under the same
// lock + audit + health-recording path Run uses. Exposed for tests
// that want to verify per-tick behaviour without spinning the full
// ticker loop, and for any future admin endpoint that wants to force
// a tick.
func (s *Service) Tick(ctx context.Context) {
	s.runOnce(ctx, s.work)
}

// Run implements lifecycle.Runner with a jittered first tick to
// prevent thundering herd on the advisory lock at startup.
func (s *Service) Run(ctx context.Context) error {
	if s.startup != nil {
		s.runOnce(ctx, s.startup)
	}

	jitter := rand.N(s.interval / 2) //nolint:gosec // G404: startup jitter does not require crypto-strength randomness
	select {
	case <-time.After(jitter):
	case <-ctx.Done():
		return nil
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if s.shouldRun != nil && !s.shouldRun() {
				continue
			}
			s.runOnce(ctx, s.work)
		case <-ctx.Done():
			return nil
		}
	}
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// runOnce creates an audit context, acquires the advisory lock, runs
// fn, and records the resulting health state. Lock-busy and
// shouldRun-gated ticks are accounted as "skipped" so health metrics
// can distinguish them from outright failures.
func (s *Service) runOnce(ctx context.Context, fn func(ctx context.Context) error) {
	tickCtx := audit.WithRequestID(ctx, audit.NewID())
	var workErr error
	acquired, lockErr := s.locker.WithAdvisoryLock(tickCtx, s.lockID,
		func(lockCtx context.Context) error {
			workErr = fn(lockCtx)
			return nil
		})

	switch {
	case lockErr != nil && !errors.Is(lockErr, core.ErrDBUnavailable):
		if s.onError != nil {
			s.onError(lockErr)
		} else {
			s.log.ErrorContext(ctx, "tick failed", "error", lockErr)
		}
		s.recordHealth(false, fmt.Errorf("advisory lock: %w", lockErr))
	case !acquired:
		s.log.DebugContext(ctx, "tick skipped, another instance holds the lock")
		telemetry.WorkerTicksTotal.WithLabelValues(s.name, "skipped").Inc()
	case workErr != nil:
		s.log.ErrorContext(ctx, MsgPassFailed, "error", workErr)
		s.recordHealth(false, workErr)
	default:
		s.recordHealth(true, nil)
	}
}

// recordHealth updates the per-service health state plus the worker
// metrics. Centralised so the tick path has one place to maintain the
// invariants (consecutiveFailures resets on success, last_success
// timestamp only moves forward on success).
func (s *Service) recordHealth(success bool, err error) {
	now := time.Now()
	s.healthMu.Lock()
	if success {
		s.health.LastSuccess = now
		s.health.LastError = ""
		s.health.ConsecutiveFailures = 0
	} else {
		s.health.LastFailure = now
		if err != nil {
			s.health.LastError = err.Error()
		}
		s.health.ConsecutiveFailures++
	}
	failures := s.health.ConsecutiveFailures
	s.healthMu.Unlock()

	if success {
		telemetry.WorkerTicksTotal.WithLabelValues(s.name, "success").Inc()
		telemetry.WorkerLastSuccessTimestampSeconds.WithLabelValues(s.name).Set(float64(now.Unix()))
		telemetry.WorkerConsecutiveFailures.WithLabelValues(s.name).Set(0)
	} else {
		telemetry.WorkerTicksTotal.WithLabelValues(s.name, "error").Inc()
		telemetry.WorkerConsecutiveFailures.WithLabelValues(s.name).Set(float64(failures))
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Name returns the snake_case component slug the service was
// constructed with. Exposed for tests that validate cross-service
// invariants (unique lock IDs, sane intervals) and for any future
// admin tooling that wants per-service identification.
func (s *Service) Name() string { return s.name }

// LockID returns the advisory-lock identifier this service holds per
// tick. Exposed so DI invariant tests can pin "no two services share
// a lock ID."
func (s *Service) LockID() int64 { return s.lockID }

// Interval returns the configured tick period. Exposed so DI
// invariant tests can pin "no interval is zero or pathologically
// large."
func (s *Service) Interval() time.Duration { return s.interval }

// Health implements lifecycle.HealthReporter. Returns a snapshot of
// the service's last tick outcomes plus its registered name so the
// admin endpoint can render a per-service status table.
func (s *Service) Health() lifecycle.WorkerHealth {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	h := s.health
	h.Name = s.name
	return h
}
