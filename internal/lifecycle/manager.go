// -------------------------------------------------------------------------------
// Service Lifecycle Manager
//
// Author: Alex Freidah
//
// Manages background service goroutines with panic recovery, automatic restart,
// and ordered shutdown. Services implement the Service interface (blocking Run
// method); optional Stoppable interface adds explicit cleanup on shutdown.
// -------------------------------------------------------------------------------

package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
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
// registration order, bounded by the given timeout.
func (m *Manager) Stop(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for i := len(m.services) - 1; i >= 0; i-- {
		if s, ok := m.services[i].service.(Stoppable); ok {
			if err := s.Stop(ctx); err != nil {
				slog.Error("Service stop error",
					"service", m.services[i].name,
					"error", err,
				)
			}
		}
	}
}

func (m *Manager) supervise(ctx context.Context, e entry) {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("Service panicked, restarting",
						"service", e.name,
						"panic", fmt.Sprint(r),
					)
				}
			}()

			if err := e.service.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Error("Service exited unexpectedly, restarting",
					"service", e.name,
					"error", err,
				)
			}
		}()

		// If context is done, exit the supervision loop
		if ctx.Err() != nil {
			return
		}

		// Brief pause before restart to avoid tight loops
		select {
		case <-time.After(1 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}
