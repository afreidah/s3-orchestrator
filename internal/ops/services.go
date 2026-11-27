// -------------------------------------------------------------------------------
// Ops - Service Construction
//
// Author: Alex Freidah
//
// One construction site for the whole operations layer. Each service takes
// only the collaborators it uses, but they share a configuration holder, so a
// SIGHUP reaches every operation through a single update rather than one hook
// per service.
// -------------------------------------------------------------------------------

package ops

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
)

// logComponent names every operation service's log lines.
const logComponent = "ops"

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Deps holds every collaborator the operations layer needs. Encryptor and
// EncStore are nil when the orchestrator runs without encryption, Codec and
// CompStore when it runs without a compression codec, Rebalancer when the
// worker pool is not wired, and Reconciler outside worker mode; the
// operations that depend on them report that rather than failing.
type Deps struct {
	Objects      ObjectAPI
	Store        ObjectStore
	Encryptor    *encryption.Encryptor
	EncStore     RewriteStore
	Codec        storedform.Codec
	CompStore    RewriteStore
	Rewriter     Rewriter
	Replicator   Replicator
	OverRep      OverReplicationCleaner
	Backlog      BacklogCounter
	Rebalancer   Rebalancer
	Reconciler   Reconciler
	Expiry       RuleProcessor
	Scrubber     ScrubberOps
	CopyStore    CopyStore
	Locker       core.AdvisoryLocker
	Provisioning ProvisioningStore
	Registry     RegistryPublisher
	Declared     DeclaredBuckets
	Config       *config.Live
}

// Services is the assembled operations layer. Transports hold the services
// they serve.
type Services struct {
	Objects     *Objects
	Integrity   *Integrity
	Replication *Replication
	Rebalance   *Rebalance
	Lifecycle   *Lifecycle
	Encryption  *Encryption
	Compression *Compression
	Provision   *Provisioning
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// New builds every operation service from one dependency bag.
func New(d *Deps) *Services {
	cfg := d.Config
	return &Services{
		Objects: NewObjects(ObjectsDeps{
			Objects: d.Objects,
			Store:   d.Store,
			Buckets: d.Declared,
		}),
		Integrity: NewIntegrity(&IntegrityDeps{
			Scrubber:   d.Scrubber,
			Reconciler: d.Reconciler,
			Store:      d.CopyStore,
			Buckets:    d.Declared,
			Locker:     d.Locker,
			Config:     cfg,
		}),
		Replication: NewReplication(ReplicationDeps{
			Replicator: d.Replicator,
			OverRep:    d.OverRep,
			Backlog:    d.Backlog,
			Locker:     d.Locker,
			Config:     cfg,
		}),
		Rebalance: NewRebalance(RebalanceDeps{
			Rebalancer: d.Rebalancer,
			Locker:     d.Locker,
			Config:     cfg,
		}),
		Lifecycle: NewLifecycle(LifecycleDeps{Expiry: d.Expiry, Config: cfg}),
		Encryption: NewEncryption(&EncryptionDeps{
			Encryptor: d.Encryptor,
			Codec:     d.Codec,
			Store:     d.EncStore,
			Buckets:   d.Declared,
			Rewriter:  d.Rewriter,
			Locker:    d.Locker,
		}),
		Compression: NewCompression(&CompressionDeps{
			Codec:     d.Codec,
			Config:    d.Config.Load().Compression,
			Encryptor: d.Encryptor,
			Store:     d.CompStore,
			Buckets:   d.Declared,
			Rewriter:  d.Rewriter,
			Locker:    d.Locker,
		}),
		Provision: NewProvisioning(ProvisioningDeps{
			Store:    d.Provisioning,
			Objects:  d.Store,
			Registry: d.Registry,
			Config:   cfg,
		}),
	}
}

// underLock runs an operator's pass under its advisory lock, the one its
// scheduled service holds when it has one, so two runs never overlap, or
// reports busy without waiting while the lock is held. A pass that fails keeps
// the tally it gathered.
func underLock[S any](ctx context.Context, locker core.AdvisoryLocker, lockID int64, busy error, pass func(context.Context) (S, error)) (S, error) {
	var sum S
	acquired, err := locker.WithAdvisoryLock(ctx, lockID, func(lockCtx context.Context) error {
		var err error
		sum, err = pass(lockCtx)
		return err
	})
	if err == nil && !acquired {
		return sum, busy
	}
	return sum, err
}
