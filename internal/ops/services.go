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
)

// Deps holds every collaborator the operations layer needs. Encryptor and
// EncStore are nil when the orchestrator runs without encryption, Codec and
// CompStore when it runs without a compression codec, and Rebalancer when the
// worker pool is not wired; the operations that depend on them report that
// rather than failing.
type Deps struct {
	Objects      ObjectAPI
	Store        ObjectStore
	Encryptor    *encryption.Encryptor
	EncStore     EncryptionStore
	Codec        CompressionCodec
	CompStore    CompressionStore
	Runtime      RuntimeOps
	Usage        UsageGate
	IntegrityCfg IntegrityConfigLoader
	Replicator   ReplicatorOps
	OverRep      OverReplicationOps
	Rebalancer   RebalancerOps
	Scrubber     ScrubberOps
	Cfg          *config.Config
}

// Services is the assembled operations layer. Transports hold the services
// they serve; the composition root holds Config so it can push reloads.
type Services struct {
	Config      *ConfigStore
	Objects     *Objects
	Integrity   *Integrity
	Replication *Replication
	Rebalance   *Rebalance
	Encryption  *Encryption
	Compression *Compression
}

// New builds every operation service from one dependency bag.
func New(d *Deps) *Services {
	cfg := NewConfigStore(d.Cfg)
	return &Services{
		Config: cfg,
		Objects: NewObjects(ObjectsDeps{
			Objects: d.Objects,
			Store:   d.Store,
			Config:  cfg,
		}),
		Integrity: NewIntegrity(IntegrityDeps{
			Scrubber:     d.Scrubber,
			IntegrityCfg: d.IntegrityCfg,
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
		Encryption: NewEncryption(EncryptionDeps{
			Encryptor: d.Encryptor,
			Store:     d.EncStore,
			Runtime:   d.Runtime,
			Usage:     d.Usage,
		}),
		Compression: NewCompression(&CompressionDeps{
			Codec:     d.Codec,
			Config:    d.Cfg.Compression,
			Encryptor: d.Encryptor,
			Store:     d.CompStore,
			Runtime:   d.Runtime,
			Usage:     d.Usage,
		}),
	}
}

// UpdateConfig replaces the configuration every service reads. Called by the
// reload hook after a successful SIGHUP.
func (s *Services) UpdateConfig(cfg *config.Config) {
	s.Config.UpdateConfig(cfg)
}
