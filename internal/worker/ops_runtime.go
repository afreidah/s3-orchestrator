// -------------------------------------------------------------------------------
// Worker Dependency Contracts  -  Runtime Infrastructure Roles
//
// Author: Alex Freidah
//
// The non-store infrastructure the background workers need from the proxy
// layer: backend fleet discovery, admission gating, data-movement primitives,
// and usage accounting, plus the store-coupled placement facet and the metrics
// a worker reports through. Fleet bundles the three every data-moving worker
// takes.
// -------------------------------------------------------------------------------

package worker

//go:generate mockgen -destination=mock_ops_test.go -package=worker github.com/afreidah/s3-orchestrator/internal/worker Ops,Placement,Metrics,BackendSyncer,FleetOps,UsageReconciler

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/cycle"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// Ops is the subset of *infra.BackendRuntime every background worker takes.
type Ops interface {
	Backends() map[string]backend.ObjectBackend
	BackendOrder() []string
	BackendFlags(name string) core.BackendFlags
	ServedCalls(name string) core.BackendCalls
	RefusedFor(name string, required core.BackendCalls) time.Duration
	WriteTargets() []string
	AcquireAdmission(ctx context.Context) bool
	ReleaseAdmission()
	GetBackend(name string) (backend.ObjectBackend, error)
	WithTimeout(ctx context.Context) (context.Context, context.CancelFunc)
	Get(ctx context.Context, name string, be backend.ObjectBackend, key, rangeHeader string) (*backend.GetObjectResult, context.CancelFunc, error)
	Head(ctx context.Context, name string, be backend.ObjectBackend, key string) (*backend.HeadObjectResult, error)
	StreamCopy(ctx context.Context, req *backend.CopyRequest) (string, error)
	DeleteMany(ctx context.Context, name string, be backend.ObjectBackend, storageKeys []string) map[string]error
	Usage() *counter.UsageTracker
	Quota() *counter.QuotaTracker
	Acct() *accounting.Recorder
}

// Metrics is what a worker reports through: the quota gauges a pass that moved
// data refreshes, and the fleet-wide gauges a lock holder publishes.
// *metrics.Collector satisfies it.
type Metrics interface {
	cycle.Refresher
	telemetry.GaugePublisher
}

// Fleet is what every data-moving worker draws on: the backend runtime, the
// write coordinator, and the metrics it reports through. A worker's Deps embed
// it, and its constructor checks it with mustBeWired.
type Fleet struct {
	Ops       Ops
	Placement Placement
	Metrics   Metrics
}

// mustBeWired panics unless every part of the fleet is set, since a missing
// one is a wiring bug that would otherwise surface on the first pass.
func (f *Fleet) mustBeWired() {
	must.NotNil("d.Ops", f.Ops)
	must.NotNil("d.Placement", f.Placement)
	must.NotNil("d.Metrics", f.Metrics)
}

// moveResult classifies a finished move for the tally: a placement the source's
// change or the target declined is a skip, anything else that went wrong a
// failure.
func moveResult(err error) batch.ItemResult {
	switch {
	case err == nil:
		return batch.Succeeded()
	case core.PlacementDeclined(err):
		return batch.Skipped(progress.StatusSkipped)
	default:
		return batch.Failed()
	}
}

// Placement is the store-coupled write-path facet (target selection, claim,
// move, placement, discard, and settling queued cleanup) workers get from
// *writepath.Coordinator, kept apart from Ops because the runtime deliberately
// holds no store.
type Placement interface {
	RankReplicaTargets(size int64, exclusion map[string]bool) []string
	ClaimCopyTarget(ctx context.Context, p *core.PendingObject, accepts core.BackendFlags) error
	MoveObject(ctx context.Context, req *writepath.MoveRequest) (int64, error)
	Place(ctx context.Context, p *core.Placement) (int64, error)
	Discard(ctx context.Context, p *core.PendingObject, reason string)
	DropCopy(ctx context.Context, loc *core.ObjectLocation, keepAtLeast int, reason string) error
	Settle(ctx context.Context, items []core.CleanupItem)
}
