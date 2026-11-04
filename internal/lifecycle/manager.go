// -------------------------------------------------------------------------------
// Service Lifecycle Manager
//
// Author: Alex Freidah
//
// Manages background service goroutines with panic recovery, automatic restart,
// and ordered shutdown. Services implement the Runner interface (blocking Run
// method); optional Stopper interface adds explicit cleanup on shutdown.
// -------------------------------------------------------------------------------

package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/util/backoff"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Runner represents a long-running background task. Run blocks until ctx is
// cancelled or a fatal error occurs, and Name is the snake_case name the
// supervisor logs it under.
type Runner interface {
	Run(ctx context.Context) error
	Name() string
}

// Stopper is an optional interface for services that need explicit cleanup
// beyond context cancellation.
type Stopper interface {
	Stop(ctx context.Context) error
}

// restartBackoff spaces the restarts of a service that exits or panics, and a
// service that ran for healthyRun before it failed starts again from the
// first step.
var restartBackoff = backoff.Exponential{Base: time.Second, Ceiling: 30 * time.Second}

// healthyRun is how long a service must run before its failure counts as a
// fresh fault rather than another immediate one.
const healthyRun = 60 * time.Second

// Manager registers and supervises background services.
type Manager struct {
	services []Runner
	log      *slog.Logger
}

// -------------------------------------------------------------------------
// MANAGER LIFECYCLE
// -------------------------------------------------------------------------

// NewManager creates an empty service manager that restarts a failed service
// with the supervisor backoff.
func NewManager() *Manager {
	return &Manager{log: logfmt.Logger("lifecycle_manager")}
}

// Register adds a service. Services start in registration order and stop in
// reverse order.
func (m *Manager) Register(r Runner) {
	m.services = append(m.services, r)
}

// Names returns the registered service names in registration order.
// Intended for tests that assert which services are wired in a given
// run mode; the supervisor loop itself does not consume this.
func (m *Manager) Names() []string {
	names := make([]string, len(m.services))
	for i, r := range m.services {
		names[i] = r.Name()
	}
	return names
}

// WorkerHealth snapshots a registered service's last tick outcomes
// plus its registration name. ConsecutiveFailures resets to 0 on the
// next success; LastSuccess/LastFailure are zero until the
// corresponding event happens. Surfaced through Manager.Health() and
// the admin /api/workers endpoint.
type WorkerHealth struct {
	Name                string    `json:"name"`
	LastSuccess         time.Time `json:"last_success"`
	LastFailure         time.Time `json:"last_failure"`
	LastError           string    `json:"last_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
}

// HealthReporter is the optional interface registered services may
// implement to expose per-tick health state. Services that satisfy it
// appear in Manager.Health(); services that do not are silently
// omitted, so adding the interface is purely additive.
type HealthReporter interface {
	Health() WorkerHealth
}

// Health returns a snapshot of every registered service that
// implements HealthReporter. Order matches registration order so
// operators reading the JSON dump see the same service ordering as
// startup logs.
func (m *Manager) Health() []WorkerHealth {
	out := make([]WorkerHealth, 0, len(m.services))
	for _, r := range m.services {
		hr, ok := r.(HealthReporter)
		if !ok {
			continue
		}
		h := hr.Health()
		h.Name = r.Name()
		out = append(out, h)
	}
	return out
}

// Run starts all registered services and blocks until ctx is cancelled. Each
// service runs in its own goroutine with panic recovery and automatic restart.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup

	for _, r := range m.services {
		wg.Go(func() { m.supervise(ctx, r) })
	}

	wg.Wait()
}

// Stop calls Stop on services that implement Stopper, in reverse
// registration order. The timeout is divided equally among stoppable
// services so a slow service cannot starve the rest of their shutdown
// budget.
func (m *Manager) Stop(timeout time.Duration) {
	var stoppable int
	for _, r := range m.services {
		if _, ok := r.(Stopper); ok {
			stoppable++
		}
	}
	if stoppable == 0 {
		return
	}
	perService := timeout / time.Duration(stoppable)

	for _, r := range slices.Backward(m.services) {
		s, ok := r.(Stopper)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), perService)
		if err := s.Stop(ctx); err != nil {
			m.log.ErrorContext(ctx, "service stop error",
				"service", r.Name(),
				"error", err,
			)
		}
		cancel()
	}
}

// -------------------------------------------------------------------------
// SUPERVISOR LOOP
// -------------------------------------------------------------------------

// supervise runs a single service in a loop and restarts it on restartBackoff
// if Run returns or panics while ctx is still alive. A service that ran for
// healthyRun starts again from the first step, so a long-stable service does
// not carry stale backoff into a fresh fault.
func (m *Manager) supervise(ctx context.Context, svc Runner) {
	var restarts int32
	for {
		start := time.Now()

		func() {
			defer func() {
				if r := recover(); r != nil {
					m.log.ErrorContext(ctx, "service panicked, restarting",
						"service", svc.Name(),
						"panic", fmt.Sprint(r),
						"stack", string(debug.Stack()),
					)
				}
			}()

			if err := svc.Run(ctx); err != nil && ctx.Err() == nil {
				m.log.ErrorContext(ctx, "service exited unexpectedly, restarting",
					"service", svc.Name(),
					"error", err,
				)
			}
		}()

		if ctx.Err() != nil {
			return
		}
		if time.Since(start) >= healthyRun {
			restarts = 0
		}
		wait := restartBackoff.For(restarts)
		m.log.WarnContext(ctx, "restarting service after backoff",
			"service", svc.Name(),
			"backoff", wait,
		)
		if !syncutil.Sleep(ctx, wait) {
			return
		}
		restarts++
	}
}
