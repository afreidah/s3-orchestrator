// -------------------------------------------------------------------------------
// Admin API Handler - Type Definition and Skeleton
//
// Author: Alex Freidah
//
// Handler/Deps types and constructor for the admin API exposed under
// /admin/api/. The per-domain endpoint implementations live in
// handler_<domain>.go siblings, route registration in handler_routes.go, and
// authentication and authorization in authz.go.
// -------------------------------------------------------------------------------

package admin

import (
	"crypto/rand"
	"io"
	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/proxy/backendstate"
	"github.com/afreidah/s3-orchestrator/internal/sharedstate"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Handler serves the admin API endpoints. d holds the dependencies New was
// given; the remaining fields are derived from them or set later.
//
// reloadStatus and liveConfig are functions set after construction rather
// than dependencies, so this package does not import internal/reload - which
// would cycle back here through the UI.
type Handler struct {
	d            Deps
	log          *slog.Logger
	cacheTopic   sharedstate.Topic[cache.Invalidation]
	instances    *sharedstate.Roster[adminapi.InstanceReport]
	confirmKey   []byte
	reloadStatus func() *adminapi.ReloadStatusResponse    // nil before the first reload
	liveConfig   func() (*adminapi.ConfigResponse, error) // nil until the runtime wires it
}

// Deps groups the narrow role interfaces and infrastructure the admin
// handler touches. Each field carries the smallest contract the handler
// actually uses, so the constructor (and the backing DI provider) never
// hand the handler a god-shaped orchestration object.
type Deps struct {
	BackendOps   BackendOps
	Dashboard    DashboardReader
	Objects      *ops.Objects
	Integrity    *ops.Integrity
	Replication  *ops.Replication
	Rebalance    *ops.Rebalance
	Expiry       *ops.Lifecycle
	Encryption   *ops.Encryption
	Compression  *ops.Compression
	Provision    *ops.Provisioning
	States       *backendstate.Manager
	StateStore   core.BackendStateStore
	DBHealthy    func() bool                    // typically *breaker.CircuitBreaker.IsHealthy
	WorkerHealth func() []adminapi.WorkerHealth // typically lifecycle.Manager.Health adapted
	LogBuffer    LogReader                      // nil when the log buffer is not wired
	ReplMetrics  replicationSnapshotter         // nil when the metrics collector is not wired
	Cleanup      core.CleanupStore
	ObjectCache  cache.ObjectCache // nil when object data caching is disabled
	FlightRec    io.WriterTo       // nil when debug.flight_recorder.enabled is false
	Reconciler   Reconciler
	Registry     func() *auth.BucketRegistry // the live registry, re-read so a reload is not missed
	BackendNames func() []string             // typically *infra.BackendRuntime.BackendOrder
	LogLevel     *slog.LevelVar
	Instance     adminapi.InstanceStatus // this instance's identity, reported on every status response
	Shared       *sharedstate.Shared     // state shared with the other instances; nil on a single instance
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// New creates a new admin API handler from its narrow dependency bag.
func New(d *Deps) *Handler {
	must.NotNil("d", d)
	must.NotNil("d.BackendOps", d.BackendOps)
	must.NotNil("d.Objects", d.Objects)
	must.NotNil("d.Integrity", d.Integrity)
	must.NotNil("d.Replication", d.Replication)
	must.NotNil("d.Rebalance", d.Rebalance)
	must.NotNil("d.Encryption", d.Encryption)
	must.NotNil("d.Compression", d.Compression)
	must.NotNil("d.Provision", d.Provision)
	must.NotNil("d.States", d.States)
	must.NotNil("d.StateStore", d.StateStore)
	must.NotNil("d.Cleanup", d.Cleanup)
	must.NotNil("d.LogLevel", d.LogLevel)
	must.NotNil("d.Registry", d.Registry)
	must.NotNil("d.BackendNames", d.BackendNames)
	h := &Handler{
		d:          *d,
		log:        slog.Default().With(logfmt.Component("admin")),
		cacheTopic: cache.InvalidationTopic(d.Shared),
		confirmKey: mustConfirmKey(),
	}
	h.instances = h.instanceRoster(d.Shared)
	return h
}

// InstanceReporter is the service that publishes this instance's report for
// the others to list, run by the lifecycle manager wherever there is Redis.
func (h *Handler) InstanceReporter() *sharedstate.Roster[adminapi.InstanceReport] {
	return h.instances
}

// mustConfirmKey mints a random per-process key for purge confirmation tokens,
// so a leaked config cannot forge one. A restart invalidates tokens in flight.
func mustConfirmKey() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("admin: cannot read random bytes for the confirmation key: " + err.Error())
	}
	return key
}

// SetReloadStatusProvider wires the callback that returns the latest reload
// result. A setter because admin cannot import the reload package.
func (h *Handler) SetReloadStatusProvider(fn func() *adminapi.ReloadStatusResponse) {
	h.reloadStatus = fn
}

// SetConfigProvider wires the callback that returns the running configuration,
// set after construction for the same reason as SetReloadStatusProvider.
func (h *Handler) SetConfigProvider(fn func() (*adminapi.ConfigResponse, error)) {
	h.liveConfig = fn
}

// Outcome values every operation response carries, so a client can branch on
// what happened before reading the operation-specific counts.
const (
	statusOK       = "ok"
	statusSkipped  = "skipped"
	statusComplete = "complete"
)

// maxSmallBody caps a request body that carries names, tags or a CORS rule
// set, never data. Ten tags at their maximum lengths is a few kilobytes.
const maxSmallBody = 64 << 10
