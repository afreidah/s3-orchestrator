// -------------------------------------------------------------------------------
// Worker Dependency Contracts  -  Runtime Infrastructure Roles
//
// Author: Alex Freidah
//
// The non-store infrastructure the background workers need from the proxy
// layer: backend fleet discovery, admission gating, data-movement primitives,
// and usage accounting, plus the store-coupled placement facet.
// -------------------------------------------------------------------------------

package worker

//go:generate mockgen -destination=mock_ops_test.go -package=worker github.com/afreidah/s3-orchestrator/internal/worker Ops,Placement,BackendSyncer,FleetOps,UsageReconciler

import (
	"context"
	"io"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// Single-operation admission sets shared by the background workers.
// Package-level so a pass that checks a backend per object does not allocate
// a one-element slice for every check.
var (
	getObjectOp = []s3op.Operation{s3op.GetObject}
	putObjectOp = []s3op.Operation{s3op.PutObject}
)

// Ops is the subset of *infra.BackendRuntime every background worker takes.
type Ops interface {
	Backends() map[string]backend.ObjectBackend
	BackendOrder() []string
	IsDraining(name string) bool
	ExcludeDraining(eligible []string) []string
	AcquireAdmission(ctx context.Context) bool
	ReleaseAdmission()
	GetBackend(name string) (backend.ObjectBackend, error)
	WithTimeout(ctx context.Context) (context.Context, context.CancelFunc)
	GetWithTimeout(ctx context.Context, be backend.ObjectBackend, key, rangeHeader string) (*backend.GetObjectResult, context.CancelFunc, error)
	HeadWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) (*backend.HeadObjectResult, error)
	StreamCopy(ctx context.Context, src, dst backend.CopyEndpoint, srcKey, dstKey string, sizeEstimate int64) (int64, error)
	DeleteMany(ctx context.Context, name string, be backend.ObjectBackend, storageKeys []string) map[string]error
	Usage() *counter.UsageTracker
	Quota() *counter.QuotaTracker
	Acct() *accounting.Recorder
}

// Placement is the store-coupled write-path facet (target selection, move,
// delete-or-enqueue) workers get from *writepath.Coordinator, kept apart from
// Ops because the runtime deliberately holds no store.
type Placement interface {
	RankReplicaTargets(size int64, exclusion map[string]bool) []string
	MoveObject(ctx context.Context, req *writepath.MoveRequest) (int64, error)
	DeleteOrEnqueue(ctx context.Context, be backend.ObjectBackend, c *core.CleanupRequest)
	DeleteDisplaced(ctx context.Context, key string, displaced []core.DeletedCopy)
}

// StreamDecompressor decodes a stored object front to back, which is all the
// scrubber needs: it reads whole objects and never seeks within one. Declared
// here rather than taking *compression.Codec so a test can present bytes that
// will not decode without hand-building a corrupt object.
type StreamDecompressor interface {
	DecompressStream(r io.Reader) (io.ReadCloser, error)
}
