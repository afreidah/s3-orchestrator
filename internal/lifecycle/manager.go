// -------------------------------------------------------------------------------
// Service Lifecycle Manager
//
// Author: Alex Freidah
//
// Manages background service goroutines with panic recovery, automatic restart,
// and ordered shutdown. Services implement the Runner interface (blocking Run
// method); optional Stopper interface adds explicit cleanup on shutdown.
// -------------------------------------------------------------------------------

// Package lifecycle provides a service manager for registering and running
// background goroutines with coordinated graceful shutdown.
package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
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

// Run starts all registered services and blocks until ctx is cancelled. Each
// service runs in its own goroutine with panic recovery and automatic restart.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup

	for _, e := range m.services {
		wg.Add(1)
		go func(e entry) {
			defer wg.Done()
			m.supervise(ctx, e)
		}(e)
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

	for i := len(m.services) - 1; i >= 0; i-- {
		s, ok := m.services[i].runner.(Stopper)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), perService)
		if err := s.Stop(ctx); err != nil {
			m.log.ErrorContext(ctx, "service stop error",
				"service", m.services[i].name,
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
