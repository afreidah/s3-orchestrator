// -------------------------------------------------------------------------------
// Backend Runtime - Shared Storage Infrastructure
//
// Author: Alex Freidah
//
// *BackendRuntime is the infrastructure every proxy subpackage and worker
// shares: the backend map and its drain and health filters, usage limits,
// admission, per-call backend timeouts and write-error classification.
// Consumers reach it through their own consumer-declared interfaces. Its
// methods are split across files by concern: registry.go, usage.go,
// admission.go, timeout.go and classify.go.
// -------------------------------------------------------------------------------

package infra

import (
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// StateChecker reports a named backend's state flags. *BackendRuntime consumes
// this so state ownership can live in the backendstate subpackage while
// *BackendRuntime filters eligibility.
type StateChecker interface {
	Flags(name string) core.BackendFlags
}

// Config bundles every input *BackendRuntime needs at construction. Exposed so
// callers (root proxy package, tests) can build a *BackendRuntime directly. A
// nil AdmissionSem means unbounded admission, and a zero BackendTimeout
// disables the per-call timeout.
type Config struct {
	Backends        map[string]backend.ObjectBackend
	Order           []string
	BackendTimeout  time.Duration
	Usage           *counter.UsageTracker
	Quota           *counter.QuotaTracker
	RoutingStrategy config.RoutingStrategy
	Profiles        map[string]core.BackendProfile
	AdmissionSem    chan struct{}
}

// BackendRuntime holds the shared backend infrastructure. It holds no store:
// each collaborator takes the store roles it needs directly. For which methods
// belong here, see docs/style-guide.md "Where new methods live". states is
// wired after construction by SetStateChecker.
type BackendRuntime struct {
	cfg      Config
	states   StateChecker
	recorder *accounting.Recorder
}

// New constructs a *BackendRuntime from cfg. The accounting Recorder is built
// here so every consumer shares one instance over the same usage tracker.
func New(cfg *Config) *BackendRuntime {
	return &BackendRuntime{cfg: *cfg, recorder: accounting.New(cfg.Usage, telemetry.ManagerOps.Record)}
}

// -------------------------------------------------------------------------
// ACCOUNTING + LOGGING + METRICS
// -------------------------------------------------------------------------

// Acct returns the shared accounting.Recorder, which every usage charge goes
// through.
func (c *BackendRuntime) Acct() *accounting.Recorder {
	return c.recorder
}

// RoutingStrategy returns the configured routing strategy.
func (c *BackendRuntime) RoutingStrategy() config.RoutingStrategy {
	return c.cfg.RoutingStrategy
}

// Quota returns the occupancy snapshot write placement ranks backends by. It
// only orders candidates: whether a write fits is settled by the conditional
// insert that claims the space.
func (c *BackendRuntime) Quota() *counter.QuotaTracker {
	return c.cfg.Quota
}
