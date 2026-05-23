// -------------------------------------------------------------------------------
// Readpath Consumer-Declared Interfaces
//
// Author: Alex Freidah
//
// Narrow contracts the read-failover Failover pulls from *infra.Core,
// the metadata store, and the location cache. Pattern rationale:
// docs/style-guide.md (Interface Design section).
// -------------------------------------------------------------------------------

package readpath

import (
	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
)

// Core is the subset of *infra.Core the Failover orchestrator needs:
// backend registry + lookup, and the accounting Recorder that owns
// the per-backend usage / per-operation metric semantics.
type Core interface {
	Backends() map[string]backend.ObjectBackend
	BackendOrder() []string
	Acct() *accounting.Recorder
}

// LocationCache is the subset of the object-package location cache the
// orchestrator needs to remember and reuse degraded-mode winners.
// Declared as an interface here (not *object.LocationCache) to avoid
// the import cycle that would otherwise form (object imports readpath
// for Failover).
type LocationCache interface {
	Get(key string) (backendName string, ok bool)
	Set(key, backendName string)
}
