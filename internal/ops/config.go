// -------------------------------------------------------------------------------
// Ops - Running Configuration Holder
//
// Author: Alex Freidah
//
// Operations that answer from configuration rather than from a worker read it
// here. The store is swapped on SIGHUP by the reload hook, so an operation
// reads the configuration in force at the moment it runs rather than the one
// present when the process started.
// -------------------------------------------------------------------------------

package ops

import (
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// ConfigStore is the operations layer's view of the running configuration.
// The zero value is not usable; construct it with NewConfigStore.
type ConfigStore struct {
	cfg syncutil.AtomicConfig[config.Config]
}

// NewConfigStore seeds the holder with the configuration the process started
// with.
func NewConfigStore(cfg *config.Config) *ConfigStore {
	s := &ConfigStore{}
	s.cfg.Store(cfg)
	return s
}

// UpdateConfig atomically replaces the configuration the operations layer
// reads. Called by the reload hook after a successful SIGHUP.
func (s *ConfigStore) UpdateConfig(cfg *config.Config) {
	s.cfg.Store(cfg)
}

// Load returns the configuration currently in force.
func (s *ConfigStore) Load() *config.Config {
	return s.cfg.Load()
}
