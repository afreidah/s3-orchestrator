// -------------------------------------------------------------------------------
// Worker Dependency Contracts  -  Runtime Infrastructure Roles
//
// Author: Alex Freidah
//
// The non-store infrastructure each background worker needs from the
// proxy layer: backend fleet discovery, admission gating, data-movement
// primitives, and usage accounting. Composed types at the bottom of the
// file pin which subset each worker takes.
// -------------------------------------------------------------------------------

package worker

//go:generate mockgen -destination=mock_ops_test.go -package=worker github.com/afreidah/s3-orchestrator/internal/worker Ops,CleanupOps,ScrubberOps,Placement,BackendSyncer,FleetOps,UsageReconciler

import (
	"context"
	"io"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
)

// Single-operation admission sets shared by the background workers.
// Package-level so a pass that checks a backend per object does not allocate
// a one-element slice for every check.
var (
	getObjectOp = []s3op.Operation{s3op.GetObject}
	putObjectOp = []s3op.Operation{s3op.PutObject}
)

// BackendAccess provides backend fleet discovery and drain-awareness.
type BackendAccess interface {
	Backends() map[string]backend.ObjectBackend
	BackendOrder() []string
	IsDraining(name string) bool
	ExcludeDraining(eligible []string) []string
}

// AdmissionControl gates concurrent access to backends.
type AdmissionControl interface {
	AcquireAdmission(ctx context.Context) bool
	ReleaseAdmission()
}

// DataMover provides object data movement primitives.
type DataMover interface {
	GetBackend(name string) (backend.ObjectBackend, error)
	WithTimeout(ctx context.Context) (context.Context, context.CancelFunc)
	GetWithTimeout(ctx context.Context, be backend.ObjectBackend, key, rangeHeader string) (*backend.GetObjectResult, context.CancelFunc, error)
	HeadWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) (*backend.HeadObjectResult, error)
	StreamCopy(ctx context.Context, src, dst backend.CopyEndpoint, key string, sizeEstimate int64) (int64, error)
	DeleteWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) error
}

// Placement is the store-coupled write-path facet (target selection, move,
// delete-or-enqueue) workers get from *writepath.Coordinator, kept apart from
// the runtime roles because the runtime deliberately holds no store.
type Placement interface {
	RankReplicaTargets(size int64, exclusion map[string]bool) []string
	MoveObject(ctx context.Context, req *writepath.MoveRequest) (int64, error)
	DeleteOrEnqueue(ctx context.Context, be backend.ObjectBackend, backendName, key, reason string, sizeBytes int64)
}

// UsageAccessor provides usage tracking.
type UsageAccessor interface {
	Usage() *counter.UsageTracker
}

// QuotaAccessor provides the byte-reservation tracker. Background workers that
// add or remove copies charge it directly: the bytes they move are the same
// bytes a client write is admitted against.
type QuotaAccessor interface {
	Quota() *counter.QuotaTracker
}

// RecorderProvider provides the shared per-backend accounting recorder.
// Prefer this over UsageAccessor for per-attempt API call / byte
// counters: APICall / Egress / Ingress / Operation document intent at
// the call site and prevent argument-order bugs.
type RecorderProvider interface {
	Acct() *accounting.Recorder
}

// -------------------------------------------------------------------------
// COMPOSED DEPENDENCY CONTRACTS
// -------------------------------------------------------------------------

// Ops combines fleet access, admission control, data movement, and usage
// tracking. Used by Rebalancer, Replicator, and OverReplicationCleaner.
type Ops interface {
	BackendAccess
	AdmissionControl
	DataMover
	UsageAccessor
	QuotaAccessor
	RecorderProvider
}

// CleanupOps is the dependency contract for CleanupWorker. It omits
// BackendAccess because cleanup operates on specific named backends.
type CleanupOps interface {
	AdmissionControl
	DataMover
	UsageAccessor
	QuotaAccessor
	RecorderProvider
}

// ScrubberOps is the dependency contract for Scrubber. It omits
// AdmissionControl because integrity checks are best-effort background work.
//
// BackendAccess is required for the fleet roster: the scrubber has to know
// which backends exist before it can ask the usage tracker which of them it can
// still afford to read from.
type ScrubberOps interface {
	BackendAccess
	DataMover
	UsageAccessor
	QuotaAccessor
	RecorderProvider
}

// StreamDecompressor decodes a stored object front to back, which is all the
// scrubber needs: it reads whole objects and never seeks within one. Declared
// here rather than taking *compression.Codec so a test can present bytes that
// will not decode without hand-building a corrupt object.
type StreamDecompressor interface {
	DecompressStream(r io.Reader) (io.ReadCloser, error)
}
