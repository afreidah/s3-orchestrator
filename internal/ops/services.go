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
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Deps holds every collaborator the operations layer needs. Encryptor and
// EncStore are nil when the orchestrator runs without encryption, Codec and
// CompStore when it runs without a compression codec, and Rebalancer when the
// worker pool is not wired; the operations that depend on them report that
// rather than failing.
type Deps struct {
	Objects      ObjectAPI
	Store        ObjectStore
	Encryptor    *encryption.Encryptor
	EncStore     RewriteStore
	Codec        storedform.Codec
	CompStore    RewriteStore
	Runtime      RuntimeOps
	Mover        Mover
	Replicator   Replicator
	OverRep      OverReplicationOps
	Rebalancer   Rebalancer
	Expiry       RuleProcessor
	Scrubber     ScrubberOps
	Unhashed     UnhashedLister
	Locker       AdvisoryLocker
	Provisioning ProvisioningStore
	Registry     RegistryPublisher
	Declared     BucketMatcher
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
		Integrity: NewIntegrity(IntegrityDeps{
			Scrubber: d.Scrubber,
			Store:    d.Unhashed,
			Locker:   d.Locker,
			Config:   cfg,
		}),
		Replication: NewReplication(ReplicationDeps{
			Replicator: d.Replicator,
			OverRep:    d.OverRep,
			Runtime:    d.Runtime,
			Config:     cfg,
		}),
		Rebalance: NewRebalance(RebalanceDeps{
			Rebalancer: d.Rebalancer,
			Runtime:    d.Runtime,
			Config:     cfg,
		}),
		Lifecycle: NewLifecycle(LifecycleDeps{Expiry: d.Expiry, Config: cfg}),
		Encryption: NewEncryption(EncryptionDeps{
			Encryptor: d.Encryptor,
			Store:     d.EncStore,
			Mover:     d.Mover,
		}),
		Compression: NewCompression(&CompressionDeps{
			Codec:     d.Codec,
			Config:    d.Config.Load().Compression,
			Encryptor: d.Encryptor,
			Store:     d.CompStore,
			Mover:     d.Mover,
		}),
		Provision: NewProvisioning(ProvisioningDeps{
			Store:    d.Provisioning,
			Objects:  d.Store,
			Registry: d.Registry,
			Config:   cfg,
		}),
	}
}
