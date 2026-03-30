// -------------------------------------------------------------------------------
// Service Lifecycle Manager
//
// Author: Alex Freidah
//
// Manages background service goroutines with panic recovery, automatic restart,
// and ordered shutdown. Services implement the Service interface (blocking Run
// method); optional Stoppable interface adds explicit cleanup on shutdown.
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
)

// Service represents a long-running background task. Run blocks until ctx is
// cancelled or a fatal error occurs.
type Service interface {
	Run(ctx context.Context) error
}

// Stoppable is an optional interface for services that need explicit cleanup
// beyond context cancellation.
type Stoppable interface {
	Stop(ctx context.Context) error
}

type entry struct {
	name    string
	service Service
}

// Manager registers and supervises background services.
type Manager struct {
	services []entry
}

// NewManager creates an empty service manager.
func NewManager() *Manager {
	return &Manager{}
}

// Register adds a named service. Services start in registration order and stop
// in reverse order.
func (m *Manager) Register(name string, svc Service) {
	m.services = append(m.services, entry{name: name, service: svc})
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

// Stop calls Stop on services that implement Stoppable, in reverse
// registration order. The timeout is divided equally among stoppable
// services so a slow service cannot starve the rest of their shutdown
// budget.
func (m *Manager) Stop(timeout time.Duration) {
	var stoppable int
	for _, e := range m.services {
		if _, ok := e.service.(Stoppable); ok {
			stoppable++
		}
	}
	if stoppable == 0 {
		return
	}
	perService := timeout / time.Duration(stoppable)

	for i := len(m.services) - 1; i >= 0; i-- {
		s, ok := m.services[i].service.(Stoppable)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), perService)
		if err := s.Stop(ctx); err != nil {
			slog.ErrorContext(ctx, "Service stop error",
				"service", m.services[i].name,
				"error", err,
			)
		}
		cancel()
	}
}

// restartBackoff parameters.
const (
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
	backoffReset   = 60 * time.Second // healthy run duration that resets backoff
)

func (m *Manager) supervise(ctx context.Context, e entry) {
	backoff := initialBackoff

	for {
		start := time.Now()

		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.ErrorContext(ctx, "Service panicked, restarting",
						"service", e.name,
						"panic", fmt.Sprint(r),
						"stack", string(debug.Stack()),
					)
				}
			}()

			if err := e.service.Run(ctx); err != nil && ctx.Err() == nil {
				slog.ErrorContext(ctx, "Service exited unexpectedly, restarting",
					"service", e.name,
					"error", err,
				)
			}
		}()

		if ctx.Err() != nil {
			return
		}

		// Reset backoff if the service ran long enough to be considered healthy.
		if time.Since(start) >= backoffReset {
			backoff = initialBackoff
		}

		slog.WarnContext(ctx, "Restarting service after backoff",
			"service", e.name,
			"backoff", backoff,
		)

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}

		// Exponential backoff capped at maxBackoff.
		backoff = min(backoff*2, maxBackoff)
	}
}
