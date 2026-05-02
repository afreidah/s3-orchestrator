// -------------------------------------------------------------------------------
// Dashboard - BackendManager Wrappers
//
// Author: Alex Freidah
//
// Thin BackendManager methods that delegate to the dashboard.Aggregator
// and enrich the result with cluster-state data only the manager has
// (drain progress, breaker health). The Aggregator itself lives in the
// dashboard subpackage.
// -------------------------------------------------------------------------------

package proxy

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// GetDashboardData delegates to the dashboard.Aggregator and enriches the
// result with drain status and circuit-breaker health from the
// BackendManager's in-memory state.
func (m *BackendManager) GetDashboardData(ctx context.Context) (*dashboard.Data, error) {
	data, err := m.dashboard.GetData(ctx)
	if err != nil {
		return nil, err
	}

	data.DrainingBackends = make(map[string]drain.Progress)
	for _, name := range m.order {
		if !m.IsDraining(name) {
			continue
		}
		progress, err := m.DrainManager.GetDrainProgress(ctx, name)
		if err == nil {
			data.DrainingBackends[name] = *progress
		}
	}

	data.UnhealthyBackends = make(map[string]bool)
	for name, be := range m.backends {
		if cb, ok := be.(*backend.CircuitBreakerBackend); ok && !cb.IsHealthy() {
			data.UnhealthyBackends[name] = true
		}
	}

	return data, nil
}

// GetDirectoryChildren delegates to the dashboard.Aggregator.
func (m *BackendManager) GetDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.DirectoryListResult, error) {
	return m.dashboard.GetDirectoryChildren(ctx, prefix, startAfter, maxKeys)
}
