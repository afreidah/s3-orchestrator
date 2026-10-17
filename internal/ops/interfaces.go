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

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// ObjectAPI is the object-byte surface the operations layer uses: the calls
// that move data through a backend and record usage on the way.
// *object.Manager satisfies it.
type ObjectAPI interface {
	GetObject(ctx context.Context, key, rangeHeader string) (*object.GetResult, error)
	PutObject(ctx context.Context, req *object.PutObjectRequest) (string, error)
	DeleteObject(ctx context.Context, key string) error
	DeleteObjects(ctx context.Context, keys []string) []object.DeleteObjectResult
	ListObjects(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*object.ListObjectsV2Result, error)
	GetObjectTags(ctx context.Context, key string) ([]core.Tag, error)
	PutObjectTags(ctx context.Context, key string, tags []core.Tag) error
	DeleteObjectTags(ctx context.Context, key string) error
}

// -------------------------------------------------------------------------
// STORES AND CODECS
// -------------------------------------------------------------------------

// ObjectStore is the metadata half of the object operations: the namespace
// listing that answers what exists without reading any bytes.
type ObjectStore interface {
	core.ObjectStore
}

// UnhashedLister lists the copies a checksum backfill still has to hash, which
// the backfill selects by stored form.
type UnhashedLister interface {
	ListCopiesByForm(ctx context.Context, f *core.FormFilter, limit int, after core.Cursor) ([]core.ObjectLocation, error)
}

// RewriteStore is the store surface the bulk rewrite passes and key rotation
// read and write: the copies a pass selects by stored form, and the patch one
// copy takes when a rotation re-wraps its DEK or a compression pass records
// the probe that declined it.
type RewriteStore interface {
	core.CopyLister
	UpdateCopyMetadata(ctx context.Context, loc *core.ObjectLocation, p *core.CopyPatch) error
}

// ProvisioningStore is the bucket and credential surface the provisioning
// operations read and write.
type ProvisioningStore interface {
	core.ProvisioningStore
}

// BucketMatcher reports whether an object key names a bucket the deployment
// declares, counting both the config file and the store.
// *provisioning.Declared satisfies it, so newly provisioned buckets are seen
// immediately.
type BucketMatcher interface {
	HasPrefix(key string) bool
}

// NamespaceCounter answers how many objects live under a prefix, which is what
// tells a bucket deletion whether the namespace it is about to drop still
// addresses anything.
type NamespaceCounter interface {
	CountObjectsByPrefix(ctx context.Context, prefix string) (int64, error)
}

// RegistryPublisher rebuilds the credential registry the request path
// authenticates against and installs it, so a provisioning change takes effect
// on the next request rather than the next restart.
type RegistryPublisher interface {
	Republish(ctx context.Context) error
}

// IntegrityConfigLoader reads the hot-reloadable integrity settings that gate
// a scrub. Satisfied by the *syncutil.AtomicConfig[config.IntegrityConfig]
// the composition root builds, so the operations layer reads the same value
// the scrubber and the read path do rather than a copy of it.
type IntegrityConfigLoader interface {
	Load() *config.IntegrityConfig
}

// -------------------------------------------------------------------------
// RUNTIME AND WORKERS
// -------------------------------------------------------------------------

// RuntimeOps is the backend-runtime surface: backend lookup for the bulk
// rewrite passes, and the post-mutation quota-metric refresh.
// *infra.BackendRuntime satisfies it.
type RuntimeOps interface {
	GetBackend(name string) (s3be.ObjectBackend, error)
	UpdateQuotaMetrics(ctx context.Context) error
}

// Mover moves one copy through the placement step, which is how a bulk rewrite
// replaces a copy with its reformed bytes. *writepath.Coordinator satisfies it.
type Mover interface {
	MoveObject(ctx context.Context, req *writepath.MoveRequest) (int64, error)
}

// ReplicatorOps is the slice of *worker.Replicator the replication operations
// use. Config returns nil when the worker is unconfigured; Replicate runs one
// cycle and returns the copies it created alongside the per-item tally, so a
// caller can tell a cycle that did its work from one where objects failed.
type ReplicatorOps interface {
	Config() *config.ReplicationConfig
	Replicate(ctx context.Context, cfg config.ReplicationConfig, observer progress.Observer) (worker.ReplicationSummary, error)
}

// RebalancerOps is the slice of *worker.Rebalancer the rebalance operation
// uses. Config returns nil when the worker is unconfigured; Rebalance runs one
// cycle, reporting each move through the observer.
type RebalancerOps interface {
	Config() *config.RebalanceConfig
	Rebalance(ctx context.Context, cfg config.RebalanceConfig, observer progress.Observer) (worker.RebalanceSummary, error)
}

// LifecycleOps is the slice of *expiry.Manager the on-demand expiration sweep
// uses. Config returns nil when no lifecycle block is configured, which is
// distinct from one configured with no rules; ProcessRules applies every rule
// once and reports what it deleted against what it could not.
type LifecycleOps interface {
	Config() *config.LifecycleConfig
	ProcessRules(ctx context.Context, rules []config.LifecycleRule, observer progress.Observer) batch.Summary
}

// OverReplicationOps is the slice of *worker.OverReplicationCleaner the
// surplus-copy operations use. Clean reports the copies it removed alongside
// the per-item tally, so an object it could not clean is visible rather than
// hidden behind a smaller count.
type OverReplicationOps interface {
	Config() *config.ReplicationConfig
	CountPending(ctx context.Context, factor int) (int64, error)
	Clean(ctx context.Context, cfg config.ReplicationConfig, observer progress.Observer) (worker.OverReplicationSummary, error)
}

// ScrubberOps is the slice of *worker.Scrubber the integrity operations use.
type ScrubberOps interface {
	Scrub(ctx context.Context, batchSize int, backend string, observer progress.Observer) batch.Summary
	ScrubKey(ctx context.Context, key string) ([]worker.CopyVerification, error)
	HashCopies(ctx context.Context, locs []core.ObjectLocation, observer progress.Observer) batch.Summary
	ListUnreadable(ctx context.Context, limit int) ([]core.ObjectLocation, int64, error)
	PurgeUnreadable(ctx context.Context, batchSize int, observer progress.Observer) batch.Summary
}

// AdvisoryLocker is the cluster-wide mutual exclusion an operator-triggered
// pass needs so it cannot run alongside the scheduled sweep or a pass on
// another instance. Reports whether the lock was taken rather than blocking.
type AdvisoryLocker interface {
	WithAdvisoryLock(ctx context.Context, lockID int64, fn func(ctx context.Context) error) (bool, error)
}

// -------------------------------------------------------------------------
// ASSERTIONS
// -------------------------------------------------------------------------

// Compile-time assertions.
var (
	_ ObjectAPI             = (*object.Manager)(nil)
	_ IntegrityConfigLoader = (*syncutil.AtomicConfig[config.IntegrityConfig])(nil)
	_ RuntimeOps            = (*infra.BackendRuntime)(nil)
	_ Mover                 = (*writepath.Coordinator)(nil)
	_ ReplicatorOps         = (*worker.Replicator)(nil)
	_ RebalancerOps         = (*worker.Rebalancer)(nil)
	_ OverReplicationOps    = (*worker.OverReplicationCleaner)(nil)
	_ ScrubberOps           = (*worker.Scrubber)(nil)
)
