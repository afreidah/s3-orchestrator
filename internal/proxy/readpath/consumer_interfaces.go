// -------------------------------------------------------------------------------
// Readpath Consumer-Declared Interfaces
//
// Author: Alex Freidah
//
// Narrow interfaces listing only the subset of *infra.Core, the
// metadata store, and the location cache that the read-failover
// Failover orchestrator actually calls. Decouples readpath from the
// wider proxy infrastructure surface.
//
// Mirrors the consumer-declared-interfaces pattern documented in
// docs/style-guide.md (Interface Design section).
// -------------------------------------------------------------------------------

package readpath

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// Core is the subset of *infra.Core the Failover orchestrator needs:
// backend registry + lookup, and the accounting Recorder that owns
// the per-backend usage / per-operation metric semantics.
type Core interface {
	Backends() map[string]backend.ObjectBackend
	BackendOrder() []string
	Acct() *accounting.Recorder
}

// ObjectLocationLister is the subset of core.MetadataStore the orchestrator
// needs to resolve which backends hold which keys.
type ObjectLocationLister interface {
	GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error)
}

// LocationCache is the subset of the object-package location cache the
// orchestrator needs to remember and reuse degraded-mode winners.
type LocationCache interface {
	Get(key string) (backendName string, ok bool)
	Set(key, backendName string)
}
