// -------------------------------------------------------------------------------
// Admin Handler - Consumer-defined Contracts
//
// Author: Alex Freidah
//
// What the admin transport still reaches for directly, once the operations
// themselves live in internal/ops: the usage counters behind the flush and
// reconcile endpoints, the dashboard snapshot behind the status endpoint, and
// the reconciler. Each is a narrow consumer-side interface satisfied
// implicitly by the production type named in its comment.
// -------------------------------------------------------------------------------

package admin

//go:generate mockgen -destination=mock_deps_test.go -package=admin github.com/afreidah/s3-orchestrator/internal/transport/admin BackendOps,DashboardReader,Reconciler

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// BackendOps is the usage-accounting surface behind the flush and reconcile
// endpoints. *proxy.BackendManager satisfies it.
type BackendOps interface {
	FlushUsage(ctx context.Context) error
	ReconcileUsage(ctx context.Context) (map[string]int64, error)
}

// DashboardReader is the dashboard surface the admin handler reads for its
// status endpoint. *dashboard.Aggregator satisfies it.
type DashboardReader interface {
	GetData(ctx context.Context) (*dashboard.Data, error)
}

// Reconciler is the slice of *worker.Reconciler the admin handler uses for the
// on-demand reconciliation endpoint.
type Reconciler interface {
	Reconcile(ctx context.Context, backendName string) (*worker.ReconcileResult, error)
	ReconcileStreaming(ctx context.Context, backendName string, observer progress.Observer) (*worker.ReconcileResult, error)
}

// Compile-time assertions.
var (
	_ BackendOps      = (*proxy.BackendManager)(nil)
	_ DashboardReader = (*dashboard.Aggregator)(nil)
	_ Reconciler      = (*worker.Reconciler)(nil)
)
