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
type ConfigStore = syncutil.AtomicConfig[config.Config]

// NewConfigStore seeds the holder with the configuration the process started
// with.
func NewConfigStore(cfg *config.Config) *ConfigStore {
	s := &ConfigStore{}
	s.Store(cfg)
	return s
}
