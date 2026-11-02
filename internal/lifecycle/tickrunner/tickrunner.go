// -------------------------------------------------------------------------------
// Tick Runner - The Periodic Service Primitive
//
// Author: Alex Freidah
//
// Shared lifecycle.Runner that drives a function on an interval, either under
// a PostgreSQL advisory lock so one instance runs each tick, or with no lock
// so every instance does. Owns the request ID per tick, startup jitter for
// locked services, per-service health snapshots, and the worker-tick
// telemetry counters. A package of its own so worker subsystems can construct
// their services without going through DI.
// -------------------------------------------------------------------------------

package tickrunner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
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

// Config bundles the inputs needed to construct a Service. Name and Work are
// required. Name is the snake_case slug used for the metrics label and the
// logger's component, and Log defaults to ComponentLogger(Name).
//
// Locker, when set, runs each tick under LockID so one instance runs it; nil
// runs every tick on every instance. Interval is the wait between ticks, and
// Next, when set, replaces it for a service whose period changes. ShouldRun
// skips a tick without recording a failure, RunAtStartup also ticks as the
// service starts, and OnError replaces the default log for a lock failure.
type Config struct {
	Locker       AdvisoryLocker
	LockID       int64
	Interval     time.Duration
	Next         func() time.Duration
	Name         string
	Log          *slog.Logger
	ShouldRun    func() bool
	RunAtStartup bool
	Work         func(ctx context.Context) error
	OnError      func(err error)
}

// Service runs a function on an interval, under an advisory lock when it has
// one. Health state is recorded after each tick for the admin endpoint and
// Prometheus staleness alerts.
type Service struct {
	cfg      Config
	locked   bool
	healthMu sync.Mutex
	health   lifecycle.WorkerHealth
}

// everyInstance is the locker of a service with no advisory lock: every tick
// runs, on every instance.
type everyInstance struct{}

// WithAdvisoryLock runs fn without taking a lock.
func (everyInstance) WithAdvisoryLock(ctx context.Context, _ int64, fn func(ctx context.Context) error) (bool, error) {
	return true, fn(ctx)
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// ComponentLogger returns the logger scoped to a component slug, normally the
// same slug as Service.Name.
func ComponentLogger(slug string) *slog.Logger {
	return slog.Default().With(logfmt.Component(slug))
}

// New constructs a Service from cfg, filling the logger and the locker of a
// service that runs on every instance.
//
//nolint:gocritic // hugeParam: Config is a value struct by design; copying it once at construction is fine and the call sites read better than a pointer.
func New(cfg Config) *Service {
	s := &Service{cfg: cfg, locked: cfg.Locker != nil}
	if s.cfg.Locker == nil {
		s.cfg.Locker = everyInstance{}
	}
	if s.cfg.Log == nil {
		s.cfg.Log = ComponentLogger(cfg.Name)
	}
	return s
}

// Run implements lifecycle.Runner. A locked service waits a random part of its
// first interval, so instances started together do not all contend for the
// lock at once.
func (s *Service) Run(ctx context.Context) error {
	syncutil.Schedule{
		Interval:    s.cfg.Interval,
		Next:        s.cfg.Next,
		Jitter:      s.locked,
		Immediately: s.cfg.RunAtStartup,
	}.Run(ctx, s.Tick)
	return nil
}

// Tick runs one tick as Run does: skipped when ShouldRun declines it,
// otherwise under the lock, request ID and health recording.
func (s *Service) Tick(ctx context.Context) {
	if s.cfg.ShouldRun != nil && !s.cfg.ShouldRun() {
		s.recordSkip()
		return
	}
	ctx = audit.WithRequestID(ctx, audit.NewID())
	var workErr error
	acquired, lockErr := s.cfg.Locker.WithAdvisoryLock(ctx, s.cfg.LockID, func(lockCtx context.Context) error {
		workErr = s.cfg.Work(lockCtx)
		return nil
	})
	s.record(ctx, acquired, lockErr, workErr)
}

// Name returns the service's snake_case component slug.
func (s *Service) Name() string { return s.cfg.Name }

// LockID returns the advisory-lock identifier this service holds per
// tick. Exposed so DI invariant tests can pin "no two services share
// a lock ID."
func (s *Service) LockID() int64 { return s.cfg.LockID }

// Interval returns the configured tick period. Exposed so DI
// invariant tests can pin "no interval is zero or pathologically
// large."
func (s *Service) Interval() time.Duration { return s.cfg.Interval }

// Health implements lifecycle.HealthReporter. Returns a snapshot of
// the service's last tick outcomes plus its registered name so the
// admin endpoint can render a per-service status table.
func (s *Service) Health() lifecycle.WorkerHealth {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	h := s.health
	h.Name = s.cfg.Name
	return h
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// record accounts for how a tick ended. A tick the database was down for, or
// whose lock another instance held, is skipped: it did no work and is not the
// service's failure, and its last success stays where it was.
func (s *Service) record(ctx context.Context, acquired bool, lockErr, workErr error) {
	switch {
	case errors.Is(lockErr, core.ErrDBUnavailable) || errors.Is(workErr, core.ErrDBUnavailable):
		s.cfg.Log.DebugContext(ctx, "tick skipped, database unavailable")
		s.recordSkip()
	case lockErr != nil:
		if s.cfg.OnError != nil {
			s.cfg.OnError(lockErr)
		} else {
			s.cfg.Log.ErrorContext(ctx, "tick failed", "error", lockErr)
		}
		s.recordHealth(false, fmt.Errorf("advisory lock: %w", lockErr))
	case !acquired:
		s.cfg.Log.DebugContext(ctx, "tick skipped, another instance holds the lock")
		s.recordSkip()
	case workErr != nil:
		s.cfg.Log.ErrorContext(ctx, "pass failed", "error", workErr)
		s.recordHealth(false, workErr)
	default:
		s.recordHealth(true, nil)
	}
}

// recordSkip counts a tick that did no work and is not a failure. Health is
// left as it was.
func (s *Service) recordSkip() {
	telemetry.WorkerTicksTotal.WithLabelValues(s.cfg.Name, "skipped").Inc()
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
		telemetry.WorkerTicksTotal.WithLabelValues(s.cfg.Name, "success").Inc()
		telemetry.WorkerLastSuccessTimestampSeconds.WithLabelValues(s.cfg.Name).Set(float64(now.Unix()))
		telemetry.WorkerConsecutiveFailures.WithLabelValues(s.cfg.Name).Set(0)
	} else {
		telemetry.WorkerTicksTotal.WithLabelValues(s.cfg.Name, "error").Inc()
		telemetry.WorkerConsecutiveFailures.WithLabelValues(s.cfg.Name).Set(float64(failures))
	}
}
