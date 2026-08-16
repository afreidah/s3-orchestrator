// -------------------------------------------------------------------------------
// Ops - Consumer-defined Contracts
//
// Author: Alex Freidah
//
// Every collaborator the operations layer calls is declared here as a narrow
// consumer-side interface, so ops depends on behaviour rather than on the
// concrete worker, manager, and store types. Each contract is satisfied
// implicitly; the compile-time assertions below name the production
// implementation for each one.
// -------------------------------------------------------------------------------

package ops

import (
	"context"
	"io"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// ObjectAPI is the object-byte surface the operations layer uses: the calls
// that move data through a backend and record usage on the way.
// *object.Manager satisfies it.
type ObjectAPI interface {
	GetObject(ctx context.Context, key, rangeHeader string) (*s3be.GetObjectResult, error)
	PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string, metadata map[string]string) (string, error)
	DeleteObject(ctx context.Context, key string) error
	DeleteObjects(ctx context.Context, keys []string) []object.DeleteObjectResult
	ListObjects(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*object.ListObjectsV2Result, error)
}

// ObjectStore is the metadata half of the object operations: the namespace
// listing that answers what exists without reading any bytes.
type ObjectStore interface {
	core.ObjectStore
}

// EncryptionStore is the admin surface the bulk rewrite passes read and write
// as they move objects between plaintext and ciphertext.
type EncryptionStore interface {
	core.EncryptionAdmin
}

// BackendOps is the store-coupled backend surface the operations layer uses
// for usage accounting and the integrity settings that gate a scrub.
// *proxy.BackendManager satisfies it.
type BackendOps interface {
	RecordUsage(backendName string, requests, ingressBytes, egressBytes int64)
	IntegrityConfig() *config.IntegrityConfig
}

// RuntimeOps is the backend-runtime surface: backend lookup for the bulk
// rewrite passes, and the post-mutation quota-metric refresh.
// *infra.BackendRuntime satisfies it.
type RuntimeOps interface {
	GetBackend(name string) (s3be.ObjectBackend, error)
	UpdateQuotaMetrics(ctx context.Context) error
}

// ReplicatorOps is the slice of *worker.Replicator the replication operations
// use. Config returns nil when the worker is unconfigured; Replicate runs one
// cycle and returns the count.
type ReplicatorOps interface {
	Config() *config.ReplicationConfig
	Replicate(ctx context.Context, cfg config.ReplicationConfig, observer progress.Observer) (int, error)
}

// RebalancerOps is the slice of *worker.Rebalancer the rebalance operation
// uses. Config returns nil when the worker is unconfigured; Rebalance runs one
// cycle, reporting each move through the observer.
type RebalancerOps interface {
	Config() *config.RebalanceConfig
	Rebalance(ctx context.Context, cfg config.RebalanceConfig, observer progress.Observer) (worker.RebalanceSummary, error)
}

// OverReplicationOps is the slice of *worker.OverReplicationCleaner the
// surplus-copy operations use.
type OverReplicationOps interface {
	Config() *config.ReplicationConfig
	CountPending(ctx context.Context, factor int) (int64, error)
	Clean(ctx context.Context, cfg config.ReplicationConfig, observer progress.Observer) (int, error)
}

// ScrubberOps is the slice of *worker.Scrubber the integrity operations use.
type ScrubberOps interface {
	Scrub(ctx context.Context, batchSize int, observer progress.Observer) worker.WorkSummary
	ScrubKey(ctx context.Context, key string) ([]worker.CopyVerification, error)
	Backfill(ctx context.Context, batchSize, offset int, observer progress.Observer) (worker.WorkSummary, int)
}

// Compile-time assertions.
var (
	_ ObjectAPI          = (*object.Manager)(nil)
	_ BackendOps         = (*proxy.BackendManager)(nil)
	_ RuntimeOps         = (*infra.BackendRuntime)(nil)
	_ ReplicatorOps      = (*worker.Replicator)(nil)
	_ RebalancerOps      = (*worker.Rebalancer)(nil)
	_ OverReplicationOps = (*worker.OverReplicationCleaner)(nil)
	_ ScrubberOps        = (*worker.Scrubber)(nil)
)
