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
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Runner represents a long-running background task. Run blocks until ctx is
// cancelled or a fatal error occurs.
type Runner interface {
	Run(ctx context.Context) error
}

// Stopper is an optional interface for services that need explicit cleanup
// beyond context cancellation.
type Stopper interface {
	Stop(ctx context.Context) error
}

// entry pairs a registered Runner with the human-readable name used
// in supervisor logs and metric labels. The Manager walks []entry to
// start, supervise, and stop each service.
type entry struct {
	name   string
	runner Runner
}

// Default supervisor backoff: a service that exits or panics is restarted
// after initialBackoff, doubled each subsequent immediate failure up to
// maxBackoff, reset to initial once the service has run healthily for at
// least backoffReset. Tests override via SetBackoff for sub-second runs.
const (
	defaultInitialBackoff = 1 * time.Second
	defaultMaxBackoff     = 30 * time.Second
	defaultBackoffReset   = 60 * time.Second
)

// Manager registers and supervises background services.
type Manager struct {
	services []entry
	log      *slog.Logger

	initialBackoff time.Duration
	maxBackoff     time.Duration
	backoffReset   time.Duration
}

// -------------------------------------------------------------------------
// MANAGER LIFECYCLE
// -------------------------------------------------------------------------

// NewManager creates an empty service manager with production backoff
// defaults.
func NewManager() *Manager {
	return &Manager{
		log:            slog.Default().With(logfmt.Component("lifecycle_manager")),
		initialBackoff: defaultInitialBackoff,
		maxBackoff:     defaultMaxBackoff,
		backoffReset:   defaultBackoffReset,
	}
}

// SetBackoff overrides the supervisor's restart backoff parameters. Intended
// for tests that exercise the restart path without paying real wall-clock
// time. Must be called before Run; values take effect on the next supervise
// iteration.
func (m *Manager) SetBackoff(initial, maximum, reset time.Duration) {
	m.initialBackoff = initial
	m.maxBackoff = maximum
	m.backoffReset = reset
}

// Register adds a named service. Services start in registration order and stop
// in reverse order.
func (m *Manager) Register(name string, r Runner) {
	m.services = append(m.services, entry{name: name, runner: r})
}

// Names returns the registered service names in registration order.
// Intended for tests that assert which services are wired in a given
// run mode; the supervisor loop itself does not consume this.
func (m *Manager) Names() []string {
	names := make([]string, len(m.services))
	for i, e := range m.services {
		names[i] = e.name
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
	for _, e := range m.services {
		hr, ok := e.runner.(HealthReporter)
		if !ok {
			continue
		}
		h := hr.Health()
		if h.Name == "" {
			h.Name = e.name
		}
		out = append(out, h)
	}
	return out
}

// Run starts all registered services and blocks until ctx is cancelled. Each
// service runs in its own goroutine with panic recovery and automatic restart.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup

	for _, e := range m.services {
		wg.Go(func() { m.supervise(ctx, e) })
	}

	wg.Wait()
}

// Stop calls Stop on services that implement Stopper, in reverse
// registration order. The timeout is divided equally among stoppable
// services so a slow service cannot starve the rest of their shutdown
// budget.
func (m *Manager) Stop(timeout time.Duration) {
	var stoppable int
	for _, e := range m.services {
		if _, ok := e.runner.(Stopper); ok {
			stoppable++
		}
	}
	if stoppable == 0 {
		return
	}
	perService := timeout / time.Duration(stoppable)

	for _, v := range slices.Backward(m.services) {
		s, ok := v.runner.(Stopper)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), perService)
		if err := s.Stop(ctx); err != nil {
			m.log.ErrorContext(ctx, "service stop error",
				"service", v.name,
				"error", err,
			)
		}
		cancel()
	}
}

// -------------------------------------------------------------------------
// SUPERVISOR LOOP
// -------------------------------------------------------------------------

// supervise runs a single service in a loop and restarts it with
// exponential backoff if Run returns or panics while ctx is still
// alive. The backoff resets to initialBackoff after the service runs
// healthily for at least healthyResetWindow so a long-stable service
// doesn't carry stale backoff state into a fresh fault.
func (m *Manager) supervise(ctx context.Context, e entry) {
	backoff := m.initialBackoff

	for {
		start := time.Now()

		func() {
			defer func() {
				if r := recover(); r != nil {
					m.log.ErrorContext(ctx, "service panicked, restarting",
						"service", e.name,
						"panic", fmt.Sprint(r),
						"stack", string(debug.Stack()),
					)
				}
			}()

			if err := e.runner.Run(ctx); err != nil && ctx.Err() == nil {
				m.log.ErrorContext(ctx, "service exited unexpectedly, restarting",
					"service", e.name,
					"error", err,
				)
			}
		}()

		if ctx.Err() != nil {
			return
		}

		// Reset backoff if the service ran long enough to be considered healthy.
		if time.Since(start) >= m.backoffReset {
			backoff = m.initialBackoff
		}

		m.log.WarnContext(ctx, "restarting service after backoff",
			"service", e.name,
			"backoff", backoff,
		)

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}

		// Exponential backoff capped at maxBackoff.
		backoff = min(backoff*2, m.maxBackoff)
	}
}
