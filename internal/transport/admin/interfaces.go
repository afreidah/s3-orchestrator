// -------------------------------------------------------------------------------
// Admin Handler - Consumer-defined Contracts
//
// Author: Alex Freidah
//
// What the admin transport still reaches for directly, once the operations
// themselves live in internal/ops: the usage counters behind the flush and
// usage-reconcile endpoints, and the dashboard snapshot behind the status
// endpoint. Each is a narrow consumer-side interface satisfied implicitly by
// the production type named in its comment.
// -------------------------------------------------------------------------------

package admin

//go:generate mockgen -destination=mock_deps_test.go -package=admin github.com/afreidah/s3-orchestrator/internal/transport/admin BackendOps,DashboardReader

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/proxy/usage"
)

// BackendOps is the usage-accounting surface behind the flush and reconcile
// endpoints. *usage.Service satisfies it.
type BackendOps interface {
	FlushUsage(ctx context.Context) error
	ReconcileUsage(ctx context.Context) (map[string]int64, error)
}

// DashboardReader is the dashboard surface the admin handler reads for its
// status endpoint. *dashboard.Aggregator satisfies it.
type DashboardReader interface {
	GetStatus(ctx context.Context) (*dashboard.Data, error)
}

// Compile-time assertions.
var (
	_ BackendOps      = (*usage.Service)(nil)
	_ DashboardReader = (*dashboard.Aggregator)(nil)
)
