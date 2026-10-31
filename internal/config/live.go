// -------------------------------------------------------------------------------
// Running Configuration
//
// Author: Alex Freidah
//
// Defines Live - the one holder of the configuration in force, which a reload
// replaces. Every component reads its reloadable settings through it, so a
// reload reaches them all in one store with no copies to update.
// -------------------------------------------------------------------------------

package config

import "github.com/afreidah/s3-orchestrator/internal/util/syncutil"

// Live holds the running configuration. The section methods return readers a
// component embeds, each following every Store.
type Live struct {
	syncutil.AtomicConfig[Config]
}

// NewLive seeds the holder with the configuration the process started with.
func NewLive(cfg *Config) *Live {
	l := &Live{}
	l.Store(cfg)
	return l
}

// Update stores a copy of the configuration in force with edit applied, so a
// change to one section keeps the others.
func (l *Live) Update(edit func(*Config)) {
	next := *l.Load()
	edit(&next)
	l.Store(&next)
}

// Integrity reads the integrity section of the configuration in force.
func (l *Live) Integrity() syncutil.Section[IntegrityConfig] {
	return func() *IntegrityConfig { return &l.Load().Integrity }
}

// Rebalance reads the rebalance section of the configuration in force.
func (l *Live) Rebalance() syncutil.Section[RebalanceConfig] {
	return func() *RebalanceConfig { return &l.Load().Rebalance }
}

// Replication reads the replication section of the configuration in force.
func (l *Live) Replication() syncutil.Section[ReplicationConfig] {
	return func() *ReplicationConfig { return &l.Load().Replication }
}

// Lifecycle reads the lifecycle section of the configuration in force.
func (l *Live) Lifecycle() syncutil.Section[LifecycleConfig] {
	return func() *LifecycleConfig { return &l.Load().Lifecycle }
}

// UsageFlush reads the usage-flush section of the configuration in force.
func (l *Live) UsageFlush() syncutil.Section[UsageFlushConfig] {
	return func() *UsageFlushConfig { return &l.Load().UsageFlush }
}
